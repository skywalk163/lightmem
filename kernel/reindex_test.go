// R24-B：元数据为事实源 + 索引可重建 测试。
//
// 覆盖判据（§5.2）：
//
//	① Qdrant 真机 ZeroVector==0（TestQdrantZeroVectorAfterRebuild，env-gated）；
//	② Reconcile 幂等（TestReconcileIdempotent）；
//	③ Reconcile 只读（TestReconcileReadOnly，写计数 wrapper）；
//	④ BackfillVectors 在 PG 上行为逐位不变（TestPGBackfillReportShape，env-gated；
//	   不带 PG 时既有 eval 测试仍全绿，见交付报告）；
//	⑤ 三后端 superseded 行为一致（TestSupersededConsistencyMatrix，SQLite 必跑）；
//	⑦ ≥12 个 Test。
//
// 北极星（判据⑥）不在本文件断言：需 D.3 holdout-official 口径 + eval/vectors 产物，
// 该产物目前不在仓库（R11c 起遗留），已移交 E 线，见 _R24_A线交付报告.md §3。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// 装配 helper
// ------------------------------------------------------------------

// putReindexMemory 写一条记忆（可指定是否被取代）。
func putReindexMemory(t *testing.T, b StorageBackend, id, tenant, user string, fact string, supersededBy *string) {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     VisTenant,
		MemoryKind:     KindFact,
		Fact:           fact,
		Confidence:     0.9,
		Layer:          LayerL2,
		SupersededBy:   supersededBy,
		Permission:     Permission{Owner: user},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// deterministicEmbedder 确定性 embedder：由文本哈希生成前 32 维非零向量。
// 与 eval/e2e/pg 里的 hashEmbed 同口径，保证跨轮可复现。
func deterministicEmbedder(dim int) func(string) ([]float32, error) {
	return func(text string) ([]float32, error) {
		vec := make([]float32, dim)
		h := uint32(0)
		for _, c := range text {
			h = h*31 + uint32(c)
		}
		for i := 0; i < 32 && i < dim; i++ {
			vec[i] = float32((h>>uint(i%16))&0x7F) / 127.0
		}
		return vec, nil
	}
}

// writeCountingBackend 包装后端并计数**写操作**（判据 3：Reconcile 只读）。
// 只实现 Reconcile 会用到的路径；写方法计数，读方法透传。
type writeCountingBackend struct {
	inner   StorageBackend
	upserts int64
}

func (w *writeCountingBackend) Name() string                   { return w.inner.Name() }
func (w *writeCountingBackend) Ping(ctx context.Context) error { return w.inner.Ping(ctx) }
func (w *writeCountingBackend) Upsert(ctx context.Context, m *Memory) error {
	w.upserts++
	return w.inner.Upsert(ctx, m)
}
func (w *writeCountingBackend) Get(ctx context.Context, id, viewer, tenant string) (*Memory, error) {
	return w.inner.Get(ctx, id, viewer, tenant)
}
func (w *writeCountingBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	return w.inner.Query(ctx, q)
}
func (w *writeCountingBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	return w.inner.VectorSearch(ctx, q)
}
func (w *writeCountingBackend) CountByKind(ctx context.Context, k MemoryKind, tenant string) (int64, error) {
	return w.inner.CountByKind(ctx, k, tenant)
}
func (w *writeCountingBackend) Close() error { return w.inner.Close() }

// ScanLayer 透传（Reconcile 只读路径）。
func (w *writeCountingBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	sc, ok := w.inner.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("inner backend does not implement LayerScanner")
	}
	return sc.ScanLayer(ctx, layer, tenant, max)
}

// UpdateLayer 透传（LayerScanner 接口的第二个方法；缺失会导致 wrapper 不满足
// 该接口，Reconcile 会误报 "does not implement LayerScanner"）。
func (w *writeCountingBackend) UpdateLayer(ctx context.Context, memoryID string, layer MemoryLayer) error {
	sc, ok := w.inner.(LayerScanner)
	if !ok {
		return fmt.Errorf("inner backend does not implement LayerScanner")
	}
	return sc.UpdateLayer(ctx, memoryID, layer)
}

// ------------------------------------------------------------------
// 索引层测试
// ------------------------------------------------------------------

// TestReconcileSQLiteBasic 基础对账：总数、分层计数、pending 统计。
func TestReconcileSQLiteBasic(t *testing.T) {
	_, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "rx-01", "t1", "u1", "facts about quantum physics", nil)
	putReindexMemory(t, b, "rx-02", "t1", "u1", "facts about thermodynamics", nil)

	rep, err := Reconcile(context.Background(), b, "t1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Backend != b.Name() {
		t.Errorf("backend: %q, want %q", rep.Backend, b.Name())
	}
	if rep.Total != 2 {
		t.Errorf("total: %d, want 2", rep.Total)
	}
	if rep.CheckedAt == "" {
		t.Error("checked_at must be set")
	}
	if rep.LayerCounts[LayerL2] != 2 {
		t.Errorf("layer_counts[L2]: %d, want 2", rep.LayerCounts[LayerL2])
	}
}

// TestReconcileIdempotent 判据②：连续两次 Reconcile，除 CheckedAt 外全等。
func TestReconcileIdempotent(t *testing.T) {
	_, b := newSearchTestEnv(t)
	for i := 1; i <= 5; i++ {
		putReindexMemory(t, b, fmt.Sprintf("rx-i%02d", i), "t1", "u1",
			fmt.Sprintf("fact number %d", i), nil)
	}
	ctx := context.Background()
	first, err := Reconcile(ctx, b, "t1")
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// 确保 CheckedAt 至少不同（同秒取巧会让幂等断言失去意义）。
	time.Sleep(1100 * time.Millisecond)
	second, err := Reconcile(ctx, b, "t1")
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if first.CheckedAt == second.CheckedAt {
		t.Error("checked_at must differ between runs (otherwise idempotence assertion is vacuous)")
	}
	if !reconcileEqual(first, second) {
		t.Fatalf("reconcile not idempotent:\n first=%+v\nsecond=%+v", first, second)
	}
}

// TestReconcileReadOnly 判据③：Reconcile 不得写库（Upsert 计数 = 0）。
func TestReconcileReadOnly(t *testing.T) {
	_, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "rx-ro", "t1", "u1", "read only probe", nil)

	w := &writeCountingBackend{inner: b}
	if _, err := Reconcile(context.Background(), w, "t1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if w.upserts != 0 {
		t.Errorf("Reconcile must be read-only: got %d Upsert calls, want 0", w.upserts)
	}
}

// TestReconcileDriftedIDsCap DriftedIDs 上限 50（红线：防响应爆炸）。
func TestReconcileDriftedIDsCap(t *testing.T) {
	_, b := newSearchTestEnv(t)
	// 80 条 pending（无向量）→ 全部计入 missing，必须被截断到 50。
	for i := 1; i <= 80; i++ {
		putReindexMemory(t, b, fmt.Sprintf("rx-d%03d", i), "t1", "u1", fmt.Sprintf("drift %d", i), nil)
	}
	rep, err := Reconcile(context.Background(), b, "t1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rep.DriftedIDs) > driftIDCap {
		t.Errorf("DriftedIDs len %d exceeds cap %d", len(rep.DriftedIDs), driftIDCap)
	}
	if len(rep.DriftedIDs) != driftIDCap {
		t.Errorf("DriftedIDs len %d, want exactly cap %d", len(rep.DriftedIDs), driftIDCap)
	}
	// 必须有序且去重（幂等可比的前提）。
	for i := 1; i < len(rep.DriftedIDs); i++ {
		if rep.DriftedIDs[i-1] >= rep.DriftedIDs[i] {
			t.Fatalf("DriftedIDs not sorted/unique at %d: %q >= %q",
				i, rep.DriftedIDs[i-1], rep.DriftedIDs[i])
		}
	}
}

// TestRebuildIndexUnsupportedBackend 不支持补向量的后端：返回明确错误，不静默成功。
//
// R25-B · L-232 偏离登记（必要连带修改）：SQLite 自 R25-B 起实现 VectorBackfiller，
// 故原「SQLite 未实现 → 走兜底」前提失效。本测试改为验证新契约：
//   - 后端支持 backfill 但 embedder 为 nil → RebuildIndex 明确报错（不静默成功）；
//   - 提供 embedder → 真正补全向量（scanned=vectorized）。
//
// 原「兜底路径」覆盖由 TestRebuildIndexNoBackend（nil backend）间接保留。
func TestRebuildIndexUnsupportedBackend(t *testing.T) {
	_, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "rx-fb", "t1", "u1", "fallback probe", nil)
	// nil embedder：后端支持 backfill 但无 embedder → 明确错误（不静默）。
	if _, err := RebuildIndex(context.Background(), b, nil, "t1"); err == nil {
		t.Fatal("rebuild with nil embedder on backfill-capable backend: want error, got nil")
	}
	// 有 embedder：真正补向量。
	rep, err := RebuildIndex(context.Background(), b, HashEmbedder(32), "t1")
	if err != nil {
		t.Fatalf("rebuild with embedder: %v", err)
	}
	if rep.Backend != b.Name() {
		t.Errorf("backend: %q want %q", rep.Backend, b.Name())
	}
	if rep.Scanned != 1 {
		t.Errorf("scanned = %d, want 1", rep.Scanned)
	}
	if rep.Vectorized != 1 {
		t.Errorf("vectorized = %d, want 1", rep.Vectorized)
	}
	if rep.Failed != 0 {
		t.Errorf("failed = %d, want 0", rep.Failed)
	}
}

// TestRebuildIndexNoBackend 空后端：明确报错，不 panic。
func TestRebuildIndexNoBackend(t *testing.T) {
	if _, err := RebuildIndex(context.Background(), nil, nil, "t1"); err == nil {
		t.Error("nil backend: want error, got nil")
	}
	if _, err := Reconcile(context.Background(), nil, "t1"); err == nil {
		t.Error("nil backend: want error, got nil")
	}
}

// ------------------------------------------------------------------
// B4 · 版本链只读一致（L-226）
// ------------------------------------------------------------------

// TestSupersededSQLiteDefaultExcluded 判据⑤：/search 默认不含已被取代的版本。
func TestSupersededSQLiteDefaultExcluded(t *testing.T) {
	_, b := newSearchTestEnv(t)
	next := "rx-new"
	putReindexMemory(t, b, "rx-old", "t1", "u1", "superseded probe fact", &next)
	putReindexMemory(t, b, "rx-new", "t1", "u1", "current probe fact", nil)

	res, err := b.Query(context.Background(), QueryParams{TenantID: "t1", Viewer: "u1", Text: "probe"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, r := range res {
		if r.MemoryID == "rx-old" {
			t.Fatal("default query must exclude superseded memory rx-old")
		}
	}
	found := false
	for _, r := range res {
		if r.MemoryID == "rx-new" {
			found = true
		}
	}
	if !found {
		t.Fatal("current version rx-new must be returned")
	}
}

// TestSupersededSQLiteIncludeTrue 判据⑤：include_superseded=true 时带出历史版本。
func TestSupersededSQLiteIncludeTrue(t *testing.T) {
	_, b := newSearchTestEnv(t)
	next := "rx-new"
	putReindexMemory(t, b, "rx-old", "t1", "u1", "superseded probe fact", &next)
	putReindexMemory(t, b, "rx-new", "t1", "u1", "current probe fact", nil)

	res, err := b.Query(context.Background(), QueryParams{
		TenantID: "t1", Viewer: "u1", Text: "probe", IncludeSuperseded: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found := false
	for _, r := range res {
		if r.MemoryID == "rx-old" {
			found = true
		}
	}
	if !found {
		t.Fatal("IncludeSuperseded=true must include superseded memory rx-old")
	}
}

// TestSupersededConsistencyMatrix 判据⑤：三后端同一数据集行为一致。
// SQLite 必跑；PG / Qdrant 有真机环境时一并跑（否则显式 SKIP，不假 PASS）。
func TestSupersededConsistencyMatrix(t *testing.T) {
	type backendCase struct {
		name    string
		backend StorageBackend
	}
	cases := []backendCase{}

	// SQLite：必跑
	_, sb := newSearchTestEnv(t)
	cases = append(cases, backendCase{"sqlite", sb})

	// PG：真机可选
	if cfg := loadPgVisConfig(); cfg != nil {
		pb, err := newPgBackend(context.Background(), &StorageConfig{
			PG: &PGConfig{Host: cfg.host, Port: cfg.port, Database: cfg.dbname,
				User: cfg.user, Password: cfg.password, SSLMode: "disable", PoolSize: 5},
		})
		if err != nil {
			t.Logf("pg backend unavailable, skipping: %v", err)
		} else {
			cases = append(cases, backendCase{"pg", pb})
		}
	} else {
		t.Log("LIGHTMEM_PG_HOST not set: pg column of the consistency matrix SKIPPED (explicit, not a fake PASS)")
	}

	// Qdrant：真机可选
	if qcfg := loadQdrantRealTestConfigOrNil(); qcfg != nil {
		qb, err := newQdrantBackend(context.Background(), qcfg.storageConfig())
		if err != nil {
			t.Logf("qdrant backend unavailable, skipping: %v", err)
		} else {
			cases = append(cases, backendCase{"qdrant", qb})
		}
	} else {
		t.Log("LIGHTMEM_QDRANT_HOST not set: qdrant column of the consistency matrix SKIPPED (explicit, not a fake PASS)")
	}

	next := "cm-new"
	for _, c := range cases {
		// 每后端用独立 memory_id 前缀，避免互相干扰。
		oldID := fmt.Sprintf("cm-%s-old", c.name)
		newID := fmt.Sprintf("cm-%s-new", c.name)
		putReindexMemory(t, c.backend, oldID, "t1", "u1", "superseded matrix fact", &next)
		putReindexMemory(t, c.backend, newID, "t1", "u1", "current matrix fact", nil)

		// 默认：不含 superseded
		def, err := c.backend.Query(context.Background(), QueryParams{TenantID: "t1", Viewer: "u1", Text: "matrix"})
		if err != nil {
			t.Fatalf("[%s] default query: %v", c.name, err)
		}
		for _, r := range def {
			if r.MemoryID == oldID {
				t.Errorf("[%s] default query must exclude %s", c.name, oldID)
			}
		}
		// include=true：含 superseded
		inc, err := c.backend.Query(context.Background(), QueryParams{
			TenantID: "t1", Viewer: "u1", Text: "matrix", IncludeSuperseded: true,
		})
		if err != nil {
			t.Fatalf("[%s] include query: %v", c.name, err)
		}
		found := false
		for _, r := range inc {
			if r.MemoryID == oldID {
				found = true
			}
		}
		if !found {
			t.Errorf("[%s] IncludeSuperseded=true must include %s", c.name, oldID)
		}
		t.Logf("[%s] superseded consistency OK (default excludes, include=true includes)", c.name)
	}
}

// ------------------------------------------------------------------
// B1/B2 · Qdrant 真机（判据①）
// ------------------------------------------------------------------

// TestQdrantZeroVectorAfterRebuild 判据①：Qdrant 真机上，
// 造 10 条 pending 写入 → Reconcile → RebuildIndex → 再 Reconcile，
// 断言 ZeroVector == 0 且 Pending == 0。
func TestQdrantZeroVectorAfterRebuild(t *testing.T) {
	cfg := loadQdrantRealTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_QDRANT_HOST not set; skipping real Qdrant L-227 verification")
	}
	qb, err := newQdrantBackend(context.Background(), cfg.storageConfig())
	if err != nil {
		t.Fatalf("newQdrantBackend: %v", err)
	}
	ctx := context.Background()
	const n = 10
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("r24b-zv%02d", i)
		ids = append(ids, id)
		m := &Memory{
			MemoryID: id, IdempotencyKey: id,
			TenantID: "r24b", UserID: "u1", Visibility: VisTenant,
			MemoryKind: KindFact, Fact: fmt.Sprintf("zero vector probe %d", i),
			Confidence: 0.9, Layer: LayerL2, Permission: Permission{Owner: "u1"},
		}
		if err := qb.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = qb.(*qdrantBackend).do(ctx, "POST",
				"/collections/"+qb.(*qdrantBackend).col+"/points/delete",
				map[string]any{"points": []string{qdrantPointID(id)}}, nil)
		}
	})

	before, err := Reconcile(ctx, qb, "r24b")
	if err != nil {
		t.Fatalf("reconcile before: %v", err)
	}
	t.Logf("before rebuild: total=%d vectorized=%d pending=%d zero=%d missing=%d",
		before.Total, before.Vectorized, before.Pending, before.ZeroVector, before.MissingIndex)

	// 重建前：Qdrant 1.19 强制要求 vector 字段，故未补向量的点必然是零向量。
	// 这正是 L-227 要「能发现」的那一档——Reconcile 必须如实报出来（>0），
	// 而不是像修复前那样把零向量伪装成 vectorized 让人看不出来。
	if before.ZeroVector == 0 {
		t.Errorf("L-227: Reconcile must DETECT the zero vectors before rebuild, got ZeroVector=0")
	}
	if before.Pending == 0 {
		t.Errorf("L-227: pending must be > 0 before rebuild (got %d) — vector_status not persisted?",
			before.Pending)
	}

	rep, err := RebuildIndex(ctx, qb, deterministicEmbedder(qb.(*qdrantBackend).dim), "r24b")
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Logf("rebuild: scanned=%d vectorized=%d failed=%d", rep.Scanned, rep.Vectorized, rep.Failed)

	after, err := Reconcile(ctx, qb, "r24b")
	if err != nil {
		t.Fatalf("reconcile after: %v", err)
	}
	t.Logf("after rebuild: total=%d vectorized=%d pending=%d zero=%d missing=%d",
		after.Total, after.Vectorized, after.Pending, after.ZeroVector, after.MissingIndex)
	if after.ZeroVector != 0 {
		t.Errorf("after rebuild: ZeroVector = %d, want 0", after.ZeroVector)
	}
	if after.Pending != 0 {
		t.Errorf("after rebuild: Pending = %d, want 0", after.Pending)
	}
}

// TestQdrantBackfillVectors B2：Qdrant 实现了 VectorBackfiller 且能真正补向量。
func TestQdrantBackfillVectors(t *testing.T) {
	cfg := loadQdrantRealTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_QDRANT_HOST not set; skipping real Qdrant backfill test")
	}
	qb, err := newQdrantBackend(context.Background(), cfg.storageConfig())
	if err != nil {
		t.Fatalf("newQdrantBackend: %v", err)
	}
	if _, ok := qb.(VectorBackfiller); !ok {
		t.Fatal("qdrantBackend must implement VectorBackfiller (B2)")
	}
	ctx := context.Background()
	id := "r24b-bf01"
	m := &Memory{
		MemoryID: id, IdempotencyKey: id,
		TenantID: "r24b", UserID: "u1", Visibility: VisTenant,
		MemoryKind: KindFact, Fact: "backfill probe fact", Confidence: 0.9,
		Layer: LayerL2, Permission: Permission{Owner: "u1"},
	}
	if err := qb.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = qb.(*qdrantBackend).do(ctx, "POST",
			"/collections/"+qb.(*qdrantBackend).col+"/points/delete",
			map[string]any{"points": []string{qdrantPointID(id)}}, nil)
	})

	bf := qb.(VectorBackfiller)
	rep, err := bf.BackfillVectors(ctx, deterministicEmbedder(qb.(*qdrantBackend).dim))
	if err != nil {
		t.Fatalf("BackfillVectors: %v", err)
	}
	t.Logf("backfill: scanned=%d vectorized=%d failed=%d", rep.Scanned, rep.Vectorized, rep.Failed)
	// 补完后再补一次：第二次应为 0 变更（幂等）。
	rep2, err := bf.BackfillVectors(ctx, deterministicEmbedder(qb.(*qdrantBackend).dim))
	if err != nil {
		t.Fatalf("second BackfillVectors: %v", err)
	}
	if rep2.Vectorized != 0 {
		t.Errorf("second backfill must be a no-op: vectorized=%d, want 0", rep2.Vectorized)
	}
}

// ------------------------------------------------------------------
// B2/B3 · PG 真机（判据④）
// ------------------------------------------------------------------

// TestPGBackfillReportShape 判据④：PG 上 BackfillVectors 行为逐位不变
// （同样的 pending→vectorized 转换），只是新增计数返回。
func TestPGBackfillReportShape(t *testing.T) {
	cfg := loadPgVisConfig()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST not set; skipping real PG backfill shape test")
	}
	ensurePgVisDatabase(t, cfg)
	db := openPgVis(t, cfg, cfg.dbname)
	bootstrapPgVisSchema(t, db)

	pb, err := newPgBackend(context.Background(), &StorageConfig{
		PG: &PGConfig{Host: cfg.host, Port: cfg.port, Database: cfg.dbname,
			User: cfg.user, Password: cfg.password, SSLMode: "disable", PoolSize: 5},
	})
	if err != nil {
		t.Fatalf("newPgBackend: %v", err)
	}
	bf, ok := pb.(VectorBackfiller)
	if !ok {
		t.Fatal("pgBackend must implement VectorBackfiller (B2)")
	}
	ctx := context.Background()
	id := "r24b-pg-bf01"
	m := &Memory{
		MemoryID: id, IdempotencyKey: id, TenantID: "r24b", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindFact, Fact: "pg backfill probe",
		Confidence: 0.9, Layer: LayerL2, Permission: Permission{Owner: "u1"},
	}
	if err := pb.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _, _ = pb.(*pgBackend).db.Exec(`DELETE FROM memories WHERE memory_id=$1`, id) })

	rep, err := bf.BackfillVectors(ctx, deterministicEmbedder(1024))
	if err != nil {
		t.Fatalf("BackfillVectors: %v", err)
	}
	t.Logf("pg backfill: scanned=%d vectorized=%d failed=%d", rep.Scanned, rep.Vectorized, rep.Failed)
	if rep.Failed != 0 {
		t.Errorf("failed=%d, want 0", rep.Failed)
	}
	// 幂等：第二次 0 变更（全部已 vectorized）。
	rep2, err := bf.BackfillVectors(ctx, deterministicEmbedder(1024))
	if err != nil {
		t.Fatalf("second BackfillVectors: %v", err)
	}
	if rep2.Vectorized != 0 {
		t.Errorf("second backfill must be a no-op: vectorized=%d, want 0", rep2.Vectorized)
	}
}

// ------------------------------------------------------------------
// 端点与序列化
// ------------------------------------------------------------------

// TestReconcileReportJSONShape ReconcileReport 的 JSON 字段完整性（供运维消费）。
func TestReconcileReportJSONShape(t *testing.T) {
	_, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "rx-json", "t1", "u1", "json shape probe", nil)
	rep, err := Reconcile(context.Background(), b, "t1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"backend", "total", "vectorized", "pending",
		"zero_vector", "orphan_index", "missing_index", "layer_counts", "checked_at"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("missing JSON key %q in %s", key, raw)
		}
	}
}

// TestRegisterReindexRoutesDoesNotTouchRouter B3：路由自注册**不污染全局
// routeHooks**（T0 判据 5 的对拍前提：routeHooks 必须保持为空）。
func TestRegisterReindexRoutesDoesNotTouchRouter(t *testing.T) {
	before := len(routeHooks)
	mux := http.NewServeMux()
	RegisterReindexRoutes(mux)
	if len(routeHooks) != before {
		t.Fatalf("RegisterReindexRoutes must not append to global routeHooks: %d -> %d",
			before, len(routeHooks))
	}
	// 端点已注册且默认关（未绑定 server → 503，不静默成功）。
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/reconcile?tenant_id=t1", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("unbound /admin/reconcile: code %d, want 503", w.Code)
	}
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/admin/reindex?tenant_id=t1", nil))
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("unbound /admin/reindex: code %d, want 503", w2.Code)
	}
}

// TestEnableReindexRoutesBoundServer 绑定 server 后端点可用（显式启用路径）。
func TestEnableReindexRoutesBoundServer(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "rx-route", "t1", "u1", "route probe fact", nil)
	// 保存并恢复全局状态，避免污染其它用例。
	beforeHooks := len(routeHooks)
	beforeServer := reindexServer
	t.Cleanup(func() {
		routeHooks = routeHooks[:beforeHooks]
		reindexServer = beforeServer
	})

	EnableReindexRoutes(srv)
	if len(routeHooks) != beforeHooks+1 {
		t.Fatalf("EnableReindexRoutes must append exactly 1 hook: %d -> %d",
			beforeHooks, len(routeHooks))
	}
	mux := http.NewServeMux()
	for _, fn := range routeHooks {
		fn(mux)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/reconcile?tenant_id=t1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("bound /admin/reconcile: code %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var rep ReconcileReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal reconcile report: %v (body=%s)", err, w.Body.String())
	}
	if rep.Backend != b.Name() {
		t.Errorf("report backend %q, want %q", rep.Backend, b.Name())
	}
	t.Logf("reconcile via HTTP: total=%d vectorized=%d pending=%d", rep.Total, rep.Vectorized, rep.Pending)
}
