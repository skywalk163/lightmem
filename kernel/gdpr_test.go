// GDPR 三件套单测（P7-A，§7.3/§7.4/§7.5）
// 仅在 SQLite 骨架内存后端上跑通级联删除 / 导出 / 被遗忘权；PG 路径由集成测试覆盖。
package kernel

import (
	"context"
	"testing"
	"time"
)

// seedMem 构造一条带确定 recorded_at 的记忆。
func seedMem(id, tenant, user string, rec time.Time) *Memory {
	return &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     VisPrivate,
		MemoryKind:     KindFact,
		Fact:           "fact for " + id,
		RecordedAt:     rec,
		Confidence:     0.9,
		Version:        1,
		Layer:          LayerL1,
	}
}

func newTestSQLiteBackend(t testing.TB) *sqliteBackend {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	return b.(*sqliteBackend)
}

func TestGDPRExportAll(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	base := time.Now().UTC()
	mems := []*Memory{
		seedMem("m1", "t1", "u1", base.Add(-3*time.Hour)),
		seedMem("m2", "t1", "u1", base.Add(-2*time.Hour)),
		seedMem("m3", "t1", "u2", base.Add(-1*time.Hour)), // 不同 user
		seedMem("m4", "t2", "u1", base),                   // 不同 tenant
	}
	for _, m := range mems {
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.MemoryID, err)
		}
	}
	out, err := ExportAll(ctx, b, "t1", "u1")
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("export count = %d, want 2", len(out))
	}
	// 确定性排序：recorded_at 升序 → m1, m2
	if out[0].MemoryID != "m1" || out[1].MemoryID != "m2" {
		t.Fatalf("export order = [%s,%s], want [m1,m2]", out[0].MemoryID, out[1].MemoryID)
	}
	// 导出为深拷贝：改动不污染后端
	out[0].Fact = "tampered"
	got, _ := b.Get(ctx, "m1", "u1", "t1")
	if got.Fact == "tampered" {
		t.Fatalf("export returned mutable reference (backend mutated)")
	}
}

func TestGDPRCascadeDelete(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	base := time.Now().UTC()
	mems := []*Memory{
		seedMem("m1", "t1", "u1", base.Add(-3*time.Hour)),
		seedMem("m2", "t1", "u1", base.Add(-2*time.Hour)),
		seedMem("m3", "t1", "u2", base.Add(-1*time.Hour)), // 保留：不同 user
		seedMem("m4", "t2", "u1", base),                   // 保留：不同 tenant
	}
	for _, m := range mems {
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	res, err := CascadeDelete(ctx, b, "t1", "u1", "tester")
	if err != nil {
		t.Fatalf("CascadeDelete: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("cascade count = %d, want 2", res.Count)
	}
	// 被删 ID 升序
	if res.Deleted[0] != "m1" || res.Deleted[1] != "m2" {
		t.Fatalf("deleted ids = %v, want [m1 m2]", res.Deleted)
	}
	// u1/t1 已空
	left, _ := ExportAll(ctx, b, "t1", "u1")
	if len(left) != 0 {
		t.Fatalf("after cascade t1/u1 export = %d, want 0", len(left))
	}
	// u2 不受影响
	u2, _ := ExportAll(ctx, b, "t1", "u2")
	if len(u2) != 1 || u2[0].MemoryID != "m3" {
		t.Fatalf("u2 should be intact, got %d", len(u2))
	}
	// 级联 100%：u2 仍存在
	if _, err := b.Get(ctx, "m3", "u2", "t1"); err != nil {
		t.Fatalf("m3 should survive cascade: %v", err)
	}
}

func TestGDPRRightToBeForgotten(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	m := seedMem("m1", "t1", "u1", time.Now().UTC())
	if err := b.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 被遗忘权：物理删除
	res, err := RightToBeForgotten(ctx, b, "m1", "tester")
	if err != nil {
		t.Fatalf("RightToBeForgotten: %v", err)
	}
	if !res.Deleted || res.TraceKept {
		t.Fatalf("forget result = %+v", res)
	}
	// 已不可见：Get 报错
	if _, err := b.Get(ctx, "m1", "u1", "t1"); err == nil {
		t.Fatalf("m1 should be gone after forget")
	}
	// 幂等：对不存在的 memory 再次 forget 返回 nil
	if _, err := RightToBeForgotten(ctx, b, "m1", "tester"); err != nil {
		t.Fatalf("forget idempotent should be nil, got %v", err)
	}
}

// stubBackend 仅实现 StorageBackend，但不实现 ComplianceBackend —— 用于验证类型断言错误路径。
type stubBackend struct{}

func (stubBackend) Name() string                                                 { return "stub" }
func (stubBackend) Ping(context.Context) error                                   { return nil }
func (stubBackend) Upsert(context.Context, *Memory) error                        { return nil }
func (stubBackend) Get(context.Context, string, string, string) (*Memory, error) { return nil, nil }
func (stubBackend) Query(context.Context, QueryParams) ([]QueryResult, error)    { return nil, nil }
func (stubBackend) VectorSearch(context.Context, QueryParams) ([]QueryResult, error) {
	return nil, nil
}
func (stubBackend) CountByKind(context.Context, MemoryKind, string) (int64, error) { return 0, nil }
func (stubBackend) Close() error                                                   { return nil }

func TestGDPRUnsupportedBackend(t *testing.T) {
	_, err := CascadeDelete(context.Background(), stubBackend{}, "t1", "u1", "tester")
	if err == nil {
		t.Fatalf("expected error for unsupported backend")
	}
	_, err = ExportAll(context.Background(), stubBackend{}, "t1", "u1")
	if err == nil {
		t.Fatalf("expected error for unsupported backend (export)")
	}
	_, err = RightToBeForgotten(context.Background(), stubBackend{}, "m1", "tester")
	if err == nil {
		t.Fatalf("expected error for unsupported backend (forget)")
	}
}

func TestGDPRRequiresScope(t *testing.T) {
	b := newTestSQLiteBackend(t)
	if _, err := CascadeDelete(context.Background(), b, "", "u1", "tester"); err == nil {
		t.Fatalf("cascade delete requires tenant")
	}
	if _, err := ExportAll(context.Background(), b, "t1", ""); err == nil {
		t.Fatalf("export requires user")
	}
}
