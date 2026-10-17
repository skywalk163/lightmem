// SQLite 后端持久化测试（R19-B 方案 a）
// 真实证明：写入 → Close → 重开同路径 → 数据仍在（不测内存路径）。
// 仅当 newSQLiteBackend 落到磁盘文件时才成立；nil/空路径走 :memory: 不做此验证。
package kernel

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// newPersistBackend 在显式文件路径上装配真实 SQLite 后端（WAL 持久化）。
func newPersistBackend(t *testing.T, path string) *sqliteBackend {
	t.Helper()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{Path: path, WALMode: true, FKEnabled: true}}
	b, err := newSQLiteBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend(file=%s): %v", path, err)
	}
	return b.(*sqliteBackend)
}

func persistSeed(t *testing.T, b *sqliteBackend, id, tenant, user string) {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     VisPrivate,
		MemoryKind:     KindFact,
		Fact:           "fact for " + id,
		RecordedAt:     time.Now().UTC().Add(-time.Hour),
		Confidence:     0.9,
		Version:        1,
		Layer:          LayerL1,
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// 核心：写入 → Close → 重开同路径 → 仍能召回。
func TestSQLitePersistReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, path)
	persistSeed(t, b1, "m1", "t1", "u1")
	persistSeed(t, b1, "m2", "t1", "u1")
	if err := b1.Close(); err != nil {
		t.Fatalf("close b1: %v", err)
	}

	// 重建实例：必须仍读到数据（真实磁盘持久化，而非内存）。
	b2 := newPersistBackend(t, path)
	defer b2.Close()
	got, err := b2.Get(ctx, "m1", "u1", "t1")
	if err != nil {
		t.Fatalf("reopen get m1: %v", err)
	}
	if got.Fact != "fact for m1" {
		t.Fatalf("reopen fact = %q, want %q", got.Fact, "fact for m1")
	}
	all, err := b2.ScanAll(ctx, 100)
	if err != nil {
		t.Fatalf("reopen scanall: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("reopen scanall count = %d, want 2", len(all))
	}
}

// 跨实例隔离：不同路径的实例互不可见。
func TestSQLitePersistCrossInstanceIsolation(t *testing.T) {
	p1 := filepath.Join(t.TempDir(), "a.sqlite")
	p2 := filepath.Join(t.TempDir(), "b.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, p1)
	persistSeed(t, b1, "m1", "t1", "u1")
	b2 := newPersistBackend(t, p2)
	persistSeed(t, b2, "m2", "t2", "u2")

	// b1 看不到 b2 的数据
	if _, err := b1.Get(ctx, "m2", "u2", "t2"); err == nil {
		t.Fatalf("b1 should NOT see b2's m2")
	}
	// b2 看不到 b1 的数据
	if _, err := b2.Get(ctx, "m1", "u1", "t1"); err == nil {
		t.Fatalf("b2 should NOT see b1's m1")
	}
	b1.Close()
	b2.Close()
}

// 幂等键：同 idempotency_key 重 upsert → version 递增，且重开后仍递增。
func TestSQLitePersistIdempotencyVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, path)
	persistSeed(t, b1, "m1", "t1", "u1") // version 1
	m2 := &Memory{
		MemoryID: "m1", IdempotencyKey: "m1", TenantID: "t1", UserID: "u1",
		Visibility: VisPrivate, MemoryKind: KindFact, Fact: "updated",
		Confidence: 0.8, Layer: LayerL1,
	}
	if err := b1.Upsert(ctx, m2); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	got1, _ := b1.Get(ctx, "m1", "u1", "t1")
	if got1.Version != 2 {
		t.Fatalf("in-memory version = %d, want 2", got1.Version)
	}
	if got1.Fact != "updated" {
		t.Fatalf("in-memory fact = %q, want updated", got1.Fact)
	}
	b1.Close()

	b2 := newPersistBackend(t, path)
	defer b2.Close()
	got2, err := b2.Get(ctx, "m1", "u1", "t1")
	if err != nil {
		t.Fatalf("reopen get: %v", err)
	}
	if got2.Version != 2 {
		t.Fatalf("reopen version = %d, want 2", got2.Version)
	}
	if got2.Fact != "updated" {
		t.Fatalf("reopen fact = %q, want updated", got2.Fact)
	}
}

// 并发写入：多 goroutine upsert 不同 id，重开后计数一致、无丢失。
func TestSQLitePersistConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, path)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			m := &Memory{
				MemoryID: id, IdempotencyKey: id, TenantID: "t1", UserID: "u1",
				Visibility: VisPrivate, MemoryKind: KindFact, Fact: "c " + id,
				Confidence: 0.5, Layer: LayerL1,
			}
			if err := b1.Upsert(ctx, m); err != nil {
				t.Errorf("concurrent upsert %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	b1.Close()

	b2 := newPersistBackend(t, path)
	defer b2.Close()
	all, err := b2.ScanAll(ctx, 1000)
	if err != nil {
		t.Fatalf("reopen scanall: %v", err)
	}
	if len(all) != n {
		t.Fatalf("reopen count = %d, want %d", len(all), n)
	}
}

// GDPR 三件套在重开后仍生效。
func TestSQLitePersistComplianceAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gdpr.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, path)
	persistSeed(t, b1, "m1", "t1", "u1")
	persistSeed(t, b1, "m2", "t1", "u1")
	persistSeed(t, b1, "m3", "t1", "u2")
	b1.Close()

	b2 := newPersistBackend(t, path)
	defer b2.Close()

	// 导出作用域
	exp, err := b2.ExportByScope(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exp) != 2 {
		t.Fatalf("export count = %d, want 2", len(exp))
	}

	// 级联删除 t1/u1
	ids, err := b2.DeleteByScope(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("delete scope: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("deleted ids = %d, want 2", len(ids))
	}
	if _, err := b2.Get(ctx, "m1", "u1", "t1"); err == nil {
		t.Fatalf("m1 should be gone")
	}
	// u2 仍存活
	if _, err := b2.Get(ctx, "m3", "u2", "t1"); err != nil {
		t.Fatalf("m3 should survive: %v", err)
	}

	// 被遗忘权：删 m3 后重开不可见
	if err := b2.DeleteByID(ctx, "m3"); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	if _, err := b2.Get(ctx, "m3", "u2", "t1"); err == nil {
		t.Fatalf("m3 should be forgotten")
	}
}

// 分层/扫描/计数/幂等查找在重开后行为一致。
func TestSQLitePersistLayerAndScanAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layer.sqlite")
	ctx := context.Background()

	b1 := newPersistBackend(t, path)
	base := time.Now().UTC()
	for i, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		id := fmt.Sprintf("l%d", i)
		m := &Memory{
			MemoryID: id, IdempotencyKey: id, TenantID: "t1", UserID: "u1",
			Visibility: VisTenant, MemoryKind: KindFact, Fact: "layer " + id,
			RecordedAt: base.Add(time.Duration(i) * time.Minute), Confidence: 0.5, Layer: layer,
		}
		if err := b1.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// FindByIdempotencyKey 预备
	idem := &Memory{
		MemoryID: "idem1", IdempotencyKey: "keyX", TenantID: "t1", UserID: "u1",
		Visibility: VisPrivate, MemoryKind: KindFact, Fact: "idem", Confidence: 0.5, Layer: LayerL1,
	}
	if err := b1.Upsert(ctx, idem); err != nil {
		t.Fatalf("upsert idem: %v", err)
	}
	b1.Close()

	b2 := newPersistBackend(t, path)
	defer b2.Close()

	// ScanLayer(L1)：idem1 也是 L1，故预期 [idem1, l1]（确定性排序后）。
	l1, err := b2.ScanLayer(ctx, LayerL1, "t1", 100)
	if err != nil {
		t.Fatalf("scan layer L1: %v", err)
	}
	got := memIDs(l1)
	sort.Strings(got)
	want := []string{"idem1", "l1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scan layer L1 = %v, want %v", got, want)
	}
	// CountByLayer
	cL3, err := b2.CountByLayer(ctx, LayerL3, "t1")
	if err != nil {
		t.Fatalf("count layer: %v", err)
	}
	if cL3 != 1 {
		t.Fatalf("count L3 = %d, want 1", cL3)
	}
	// CountByKind
	ck, err := b2.CountByKind(ctx, KindFact, "t1")
	if err != nil {
		t.Fatalf("count kind: %v", err)
	}
	if ck != 5 {
		t.Fatalf("count kind = %d, want 5", ck)
	}
	// FindByIdempotencyKey
	found, err := b2.FindByIdempotencyKey(ctx, "t1", "u1", "keyX")
	if err != nil {
		t.Fatalf("find idem: %v", err)
	}
	if found == nil || found.MemoryID != "idem1" {
		t.Fatalf("find idem = %+v, want idem1", found)
	}
	// UpdateLayer → 重开后生效
	if err := b2.UpdateLayer(ctx, "l0", LayerL2); err != nil {
		t.Fatalf("update layer: %v", err)
	}
	upd, err := b2.Get(ctx, "l0", "u1", "t1")
	if err != nil {
		t.Fatalf("get after update layer: %v", err)
	}
	if upd.Layer != LayerL2 {
		t.Fatalf("updated layer = %s, want L2", upd.Layer)
	}
}

// VectorSearch 仍返回降级错误（方案 a 不引入 sqlite-vec）。
func TestSQLitePersistVectorSearchDegraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.sqlite")
	b1 := newPersistBackend(t, path)
	persistSeed(t, b1, "m1", "t1", "u1")
	b1.Close()

	b2 := newPersistBackend(t, path)
	defer b2.Close()
	_, err := b2.VectorSearch(context.Background(), QueryParams{})
	if err == nil {
		t.Fatalf("VectorSearch should return error (no sqlite-vec)")
	}
}

// 后端名不得含 "skeleton"（R19-B 验收）。
func TestSQLitePersistNameNoSkeleton(t *testing.T) {
	b := newPersistBackend(t, filepath.Join(t.TempDir(), "name.sqlite"))
	defer b.Close()
	if b.Name() != "sqlite" {
		t.Fatalf("Name() = %q, want sqlite", b.Name())
	}
	if strings.Contains(b.Name(), "skeleton") {
		t.Fatalf("Name() %q must not contain 'skeleton'", b.Name())
	}
}

func memIDs(ms []*Memory) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.MemoryID)
	}
	return out
}
