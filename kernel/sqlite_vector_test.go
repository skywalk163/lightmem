// R25-B · solo 档纯 Go 向量检索测试（L-232）
//
// 覆盖任务书 §5.2 八条判据：
//  1. 往返一致（与 Go 侧参考余弦排序逐一相同）
//  2. /search/vector 在 solo 档可用（HTTP 集成）
//  3. 降级路径仍在（不可见/无向量不返回垃圾结果）
//  4. 默认关零回归（空向量表时行为与 6cd26e0 逐位一致）
//  5. 零新增依赖（编译即证明，无运行断言）
//  6. BackfillVectors 在 SQLite 上可用（Count==N）
//  7. 北极星不降（由 E 线复跑 holdout-official 证明）
//  8. 测试规模 ≥ 10（本文件 12 个 Test）
//
// 向量化器用 HashEmbedder（kernel/embedder.go，确定性、无外部依赖），
// 参考余弦实现复用 embedder_vector.go 的 CosineSimilarity，与产品路径逐位一致。
package kernel

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// putVectorMemory 写一条带 source 的记忆（聚焦向量检索侧断言）。
func putVectorMemory(t *testing.T, b StorageBackend, id, tenant, user, fact, source string) {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     VisPrivate,
		MemoryKind:     KindFact,
		Fact:           fact,
		Source:         source,
		Confidence:     0.9,
		Layer:          LayerL1,
		Permission:     Permission{Owner: user},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// refCosineRank 参考实现：对给定 query 向量，按 CosineSimilarity 降序、memory_id 升序
// 排序全部 (id, vec)，返回前 topK 的 (id, score)。与 SQLiteVectorStore.Search 的排序
// 口径逐位一致，用于判据 1「往返一致」对拍。
func refCosineRank(query []float32, items []struct {
	id  string
	vec []float32
}, topK int) []VectorHit {
	hits := make([]VectorHit, 0, len(items))
	for _, it := range items {
		hits = append(hits, VectorHit{MemoryID: it.id, Score: CosineSimilarity(query, it.vec)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].MemoryID < hits[j].MemoryID
	})
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// 判据 1 · 往返一致：N≥100 条（含中文 fact），Search 的 top-k 与参考余弦排序逐一相同。
func TestSQLiteVectorStoreRoundTripMatchesReference(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "rt.sqlite"))
	defer b.Close()
	store := b.vec

	const dim = 32
	const N = 120
	embed := HashEmbedder(dim)
	items := make([]struct {
		id  string
		vec []float32
	}, 0, N)
	for i := 0; i < N; i++ {
		fact := factText(i) // 含中文，覆盖多语言
		vec, err := embed(fact)
		if err != nil {
			t.Fatalf("embed %d: %v", i, err)
		}
		id := sprintfID("v", i)
		if err := store.Put(ctx, id, vec); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
		items = append(items, struct {
			id  string
			vec []float32
		}{id, vec})
	}

	// 用第 7 条作为 query（其自身余弦应为 1.0，排第一）。
	query := items[7].vec
	topK := 10
	got, err := store.Search(ctx, query, topK)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != topK {
		t.Fatalf("search returned %d hits, want %d", len(got), topK)
	}
	want := refCosineRank(query, items, topK)
	for i := range want {
		if got[i].MemoryID != want[i].MemoryID {
			t.Fatalf("rank %d: got %s want %s", i, got[i].MemoryID, want[i].MemoryID)
		}
		if math.Abs(got[i].Score-want[i].Score) > 1e-6 {
			t.Fatalf("rank %d score: got %v want %v", i, got[i].Score, want[i].Score)
		}
	}
	// 第 7 条自身必须排第一且余弦≈1.0。
	if got[0].MemoryID != items[7].id {
		t.Fatalf("top1 = %s, want self %s", got[0].MemoryID, items[7].id)
	}
	if math.Abs(got[0].Score-1.0) > 1e-6 {
		t.Fatalf("top1 score = %v, want ~1.0", got[0].Score)
	}
}

// 判据 1（补充）· 空 query 向量报错，不 panic。
func TestSQLiteVectorStoreSearchEmptyQuery(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "sq.sqlite"))
	defer b.Close()
	if _, err := b.vec.Search(ctx, nil, 5); err == nil {
		t.Fatal("Search(nil) want error")
	}
	if _, err := b.vec.Search(ctx, []float32{}, 5); err == nil {
		t.Fatal("Search(empty) want error")
	}
}

// 判据 6 · BackfillVectors 在 SQLite 上可用：造 10 条 pending → BackfillVectors → Count==10。
func TestSQLiteBackendBackfillVectors(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "bf.sqlite"))
	defer b.Close()
	sb := b
	const n = 10
	for i := 0; i < n; i++ {
		putVectorMemory(t, sb, sprintfID("bf", i), "t1", "u1", factText(i), "sqlite:vector")
	}
	// 向量表尚未填充。
	if c, _ := sb.vec.Count(ctx); c != 0 {
		t.Fatalf("pre-backfill count = %d, want 0", c)
	}
	rep, err := sb.BackfillVectors(ctx, HashEmbedder(32))
	if err != nil {
		t.Fatalf("BackfillVectors: %v", err)
	}
	if rep.Scanned != n {
		t.Fatalf("scanned = %d, want %d", rep.Scanned, n)
	}
	if rep.Vectorized != n {
		t.Fatalf("vectorized = %d, want %d", rep.Vectorized, n)
	}
	if rep.Failed != 0 {
		t.Fatalf("failed = %d, want 0", rep.Failed)
	}
	// 幂等：再次 backfill，已存在者跳过，Vectorized 不再增长。
	rep2, err := sb.BackfillVectors(ctx, HashEmbedder(32))
	if err != nil {
		t.Fatalf("BackfillVectors 2nd: %v", err)
	}
	if rep2.Vectorized != 0 {
		t.Fatalf("2nd vectorized = %d, want 0 (idempotent)", rep2.Vectorized)
	}
	if c, _ := sb.vec.Count(ctx); c != n {
		t.Fatalf("post-backfill count = %d, want %d", c, n)
	}
}

// 判据 4（零回归）· 空向量表时 VectorSearch 返回降级错误（与 6cd26e0 逐位一致）。
func TestSQLiteBackendVectorSearchDegradedWhenEmpty(t *testing.T) {
	b := newPersistBackend(t, filepathTmp(t, "ve.sqlite"))
	defer b.Close()
	putVectorMemory(t, b, "m1", "t1", "u1", "some fact", "sqlite:vector")
	_, err := b.VectorSearch(context.Background(), QueryParams{Vector: []float32{0.1, 0.2}})
	if err == nil {
		t.Fatal("VectorSearch on empty vector store: want downgrade error")
	}
	if !strings.Contains(err.Error(), "no vectors stored") {
		t.Fatalf("downgrade message missing 'no vectors stored': %v", err)
	}
	if !strings.Contains(err.Error(), "downgrade to text") {
		t.Fatalf("downgrade message missing 'downgrade to text': %v", err)
	}
}

// 判据 2（后端级）· /search/vector 在 solo 档可用：Backfill 后 VectorSearch 返回非空且 Source 标 sqlite。
func TestSQLiteBackendVectorSearchUsable(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "vu.sqlite"))
	defer b.Close()
	sb := b
	const n = 8
	for i := 0; i < n; i++ {
		putVectorMemory(t, sb, sprintfID("vu", i), "t1", "u1",
			"上海是中国的城市编号"+itoaStr(i), "sqlite:vector")
	}
	if _, err := sb.BackfillVectors(ctx, HashEmbedder(32)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// 用第 3 条 fact 的向量作为 query。
	qvec, err := HashEmbedder(32)("上海是中国的城市编号" + itoaStr(3))
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	res, err := sb.VectorSearch(ctx, QueryParams{
		TenantID: "t1", Viewer: "u1", Vector: qvec, TopK: 5,
	})
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("VectorSearch returned empty, want non-empty")
	}
	// 命中应包含 vu3 且其 source 标明 sqlite。
	found := false
	for _, r := range res {
		if r.MemoryID == "vu3" {
			found = true
		}
		if !strings.Contains(r.Source, "sqlite") {
			t.Fatalf("result %s source %q not labeled sqlite", r.MemoryID, r.Source)
		}
	}
	if !found {
		t.Fatal("vu3 (the queried fact) not in top results")
	}
}

// 判据 3 · 降级路径仍在：向量存在但记忆不可见 → 返回降级错误，不泄露不可见记忆。
func TestSQLiteBackendVectorSearchNoVisibleDegraded(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "nv.sqlite"))
	defer b.Close()
	sb := b
	// 仅 u9 可见的私有记忆。
	putVectorMemory(t, sb, "secret", "t1", "u9", "机密事实", "sqlite:vector")
	if _, err := sb.BackfillVectors(ctx, HashEmbedder(32)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	qvec, _ := HashEmbedder(32)("机密事实")
	// 用 u1 查询（看不到 u9 的私有记忆）。
	_, err := sb.VectorSearch(ctx, QueryParams{TenantID: "t1", Viewer: "u1", Vector: qvec, TopK: 5})
	if err == nil {
		t.Fatal("VectorSearch with no visible hits: want downgrade error (no garbage result)")
	}
	if !strings.Contains(err.Error(), "no visible results") {
		t.Fatalf("downgrade message missing 'no visible results': %v", err)
	}
}

// 判据 2（HTTP 集成）· POST /search/vector 返回非空且 Source 标明 sqlite 向量路径。
func TestSQLiteVectorSearchHTTPIntegration(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	sb := b.(*sqliteBackend)
	embed := HashEmbedder(32)
	srv.SetEmbedder(embed)
	const n = 6
	for i := 0; i < n; i++ {
		putVectorMemory(t, sb, sprintfID("h", i), "t1", "u1",
			"北京是首都编号"+itoaStr(i), "sqlite:vector")
	}
	if _, err := sb.BackfillVectors(context.Background(), embed); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	body := mustJSON(t, map[string]any{
		"query":     "北京是首都编号" + itoaStr(2),
		"tenant_id": "t1",
		"viewer":    "u1",
		"top_k":     3,
	})
	req := httptest.NewRequest(http.MethodPost, "/search/vector", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/search/vector: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res []QueryResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("/search/vector returned empty, want non-empty")
	}
	for _, r := range res {
		if !strings.Contains(r.Source, "sqlite") {
			t.Fatalf("result %s source %q not labeled sqlite", r.MemoryID, r.Source)
		}
	}
	if res[0].MemoryID != "h2" {
		t.Fatalf("top1 = %s, want h2 (the queried fact)", res[0].MemoryID)
	}
}

// 判据 6（补充）· Count 与 Put 幂等：同 id 覆盖写，计数不增。
func TestSQLiteVectorStoreCountAndPutIdempotent(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "cp.sqlite"))
	defer b.Close()
	store := b.vec
	vec := []float32{0.1, 0.2, 0.3}
	if err := store.Put(ctx, "x1", vec); err != nil {
		t.Fatalf("put1: %v", err)
	}
	if err := store.Put(ctx, "x2", vec); err != nil {
		t.Fatalf("put2: %v", err)
	}
	if c, _ := store.Count(ctx); c != 2 {
		t.Fatalf("count = %d, want 2", c)
	}
	// 覆盖 x1（同 id 不同向量）→ 计数仍为 2。
	if err := store.Put(ctx, "x1", []float32{0.9, 0.8, 0.7}); err != nil {
		t.Fatalf("put1 overwrite: %v", err)
	}
	if c, _ := store.Count(ctx); c != 2 {
		t.Fatalf("count after overwrite = %d, want 2", c)
	}
	// 非法输入。
	if err := store.Put(ctx, "", vec); err == nil {
		t.Fatal("Put empty id: want error")
	}
	if err := store.Put(ctx, "x3", nil); err == nil {
		t.Fatal("Put empty vec: want error")
	}
}

// 判据 1（补充）· BLOB 编解码逐位保真：float32 → BLOB → float32 无精度损失。
func TestSQLiteVectorBlobRoundTrip(t *testing.T) {
	src := []float32{0.0, -1.5, 3.14159, 1e-7, 65504.0, -0.0001}
	blob := floatsToBlob(src)
	got, err := blobToFloats(blob)
	if err != nil {
		t.Fatalf("blobToFloats: %v", err)
	}
	if len(got) != len(src) {
		t.Fatalf("len = %d, want %d", len(got), len(src))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("idx %d: got %v want %v (float32 应逐位一致)", i, got[i], src[i])
		}
	}
	// 长度非 4 倍数视为损坏。
	if _, err := blobToFloats([]byte{1, 2, 3}); err == nil {
		t.Fatal("blobToFloats(3 bytes): want error")
	}
}

// 判据 2（补充）· topK 截断：请求 topK 小于命中数时只返回前 topK。
func TestSQLiteVectorStoreTopKTruncation(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "tk.sqlite"))
	defer b.Close()
	store := b.vec
	embed := HashEmbedder(16)
	const n = 20
	for i := 0; i < n; i++ {
		vec, _ := embed(factText(i))
		if err := store.Put(ctx, sprintfID("t", i), vec); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	query, _ := embed(factText(0))
	got, err := store.Search(ctx, query, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (topK truncation)", len(got))
	}
	if got[0].MemoryID != "t0" {
		t.Fatalf("top1 = %s, want t0", got[0].MemoryID)
	}
}

// 判据 4（补充）· VectorSearch 缺 query 向量时返回明确错误（不 panic）。
func TestSQLiteBackendVectorSearchMissingQuery(t *testing.T) {
	b := newPersistBackend(t, filepathTmp(t, "mq.sqlite"))
	defer b.Close()
	if _, err := b.VectorSearch(context.Background(), QueryParams{}); err == nil {
		t.Fatal("VectorSearch without vector: want error")
	}
}

// B1（L-235）· 多租户预筛：少数派租户查询不再被多数派挤占（复刻 R25 D.12 观察 2 场景）。
// 双租户语料（t-beam=600 多数派 / t-lc-v2=200 少数派），以少数派 viewer/tenant 查询，
// 命中应全部来自少数派且不降级（不返回 no visible results / 503）。
func TestSQLiteBackendVectorSearchMultiTenantNoCrowdOut(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "mt.sqlite"))
	defer b.Close()
	sb := b
	const majN, minN = 600, 200
	for i := 0; i < majN; i++ {
		putVectorMemory(t, sb, sprintfID("maj", i), "t-beam", "u-beam", "多数派事实编号"+itoaStr(i), "sqlite:vector")
	}
	for i := 0; i < minN; i++ {
		putVectorMemory(t, sb, sprintfID("min", i), "t-lc-v2", "u-lc", "少数派事实编号"+itoaStr(i), "sqlite:vector")
	}
	if _, err := sb.BackfillVectors(ctx, HashEmbedder(32)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	qvec, err := HashEmbedder(32)("少数派事实编号" + itoaStr(3))
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	res, err := sb.VectorSearch(ctx, QueryParams{
		TenantID: "t-lc-v2", Viewer: "u-lc", Vector: qvec, TopK: 10,
	})
	if err != nil {
		t.Fatalf("minority VectorSearch: %v (应为可见命中，不应降级 no visible results)", err)
	}
	if len(res) == 0 {
		t.Fatal("minority VectorSearch returned empty, want non-empty")
	}
	majLeak := 0
	for _, r := range res {
		if strings.HasPrefix(r.MemoryID, "maj") {
			majLeak++
		}
		// 每条结果 source 必须标明 sqlite 向量路径。
		if !strings.Contains(r.Source, "sqlite") {
			t.Fatalf("result %s source %q not labeled sqlite", r.MemoryID, r.Source)
		}
	}
	if majLeak > 0 {
		t.Fatalf("minority query leaked %d majority-tenant memories into results (pre-filter failed)", majLeak)
	}
	found := false
	for _, r := range res {
		if r.MemoryID == "min3" {
			found = true
		}
	}
	if !found {
		t.Fatal("min3 (queried minority fact) not in results; visibility pre-filter failed")
	}
}

// B1（L-235）· SearchAllowed 在 allowedIDs 集合内的余弦排序与参考实现逐一相同；
// 且 allowedIDs==nil 时等价于全局 Search（判据 3 short-circuit）。
func TestSQLiteVectorStoreSearchAllowedMatchesReference(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "sa.sqlite"))
	defer b.Close()
	store := b.vec
	const dim, N, topK = 16, 60, 10
	embed := HashEmbedder(dim)
	items := make([]struct {
		id  string
		vec []float32
	}, 0, N)
	for i := 0; i < N; i++ {
		vec, _ := embed(factText(i))
		id := sprintfID("v", i)
		if err := store.Put(ctx, id, vec); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
		items = append(items, struct {
			id  string
			vec []float32
		}{id, vec})
	}
	allowed := make(map[string]bool)
	for i := 0; i < 30; i++ {
		allowed[sprintfID("v", i)] = true
	}
	query := items[5].vec
	got, err := store.SearchAllowed(ctx, query, topK, allowed)
	if err != nil {
		t.Fatalf("SearchAllowed: %v", err)
	}
	if len(got) != topK {
		t.Fatalf("SearchAllowed returned %d, want %d", len(got), topK)
	}
	var allowedItems []struct {
		id  string
		vec []float32
	}
	for _, it := range items {
		if allowed[it.id] {
			allowedItems = append(allowedItems, it)
		}
	}
	want := refCosineRank(query, allowedItems, topK)
	for i := range want {
		if got[i].MemoryID != want[i].MemoryID {
			t.Fatalf("allowed rank %d: got %s want %s", i, got[i].MemoryID, want[i].MemoryID)
		}
		if math.Abs(got[i].Score-want[i].Score) > 1e-6 {
			t.Fatalf("allowed rank %d score: got %v want %v", i, got[i].Score, want[i].Score)
		}
	}
	// allowedIDs=nil → 等价于全局 Search。
	gotNil, err := store.SearchAllowed(ctx, query, topK, nil)
	if err != nil {
		t.Fatalf("SearchAllowed(nil): %v", err)
	}
	wantGlobal := refCosineRank(query, items, topK)
	if len(gotNil) != len(wantGlobal) {
		t.Fatalf("nil-allowed len = %d, want %d", len(gotNil), len(wantGlobal))
	}
	for i := range wantGlobal {
		if gotNil[i].MemoryID != wantGlobal[i].MemoryID {
			t.Fatalf("nil rank %d: got %s want %s", i, gotNil[i].MemoryID, wantGlobal[i].MemoryID)
		}
	}
}

// B2（L-236）· PutBatch 单事务批量写入：≥800 条一次收敛，Count 正确，覆盖写幂等，
// 空 batch 安全，非法项（空 id）回滚并报错（不残留脏数据）。
func TestSQLiteVectorStorePutBatch(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "pb.sqlite"))
	defer b.Close()
	store := b.vec
	const N = 800
	items := make([]VectorPutItem, 0, N)
	for i := 0; i < N; i++ {
		items = append(items, VectorPutItem{MemoryID: sprintfID("pb", i), Vec: []float32{float32(i % 7), float32(i % 3), 0.5}})
	}
	if err := store.PutBatch(ctx, items); err != nil {
		t.Fatalf("PutBatch: %v", err)
	}
	if c, _ := store.Count(ctx); c != N {
		t.Fatalf("post-batch count = %d, want %d", c, N)
	}
	// 幂等：再次 PutBatch 同 id（不同向量）→ 覆盖写，计数不增。
	re := make([]VectorPutItem, 0, 10)
	for i := 0; i < 10; i++ {
		re = append(re, VectorPutItem{MemoryID: sprintfID("pb", i), Vec: []float32{0.1, 0.2, 0.3}})
	}
	if err := store.PutBatch(ctx, re); err != nil {
		t.Fatalf("PutBatch 2nd: %v", err)
	}
	if c, _ := store.Count(ctx); c != N {
		t.Fatalf("post-batch idempotent count = %d, want %d (覆盖写不应增长)", c, N)
	}
	// 空 batch 安全。
	if err := store.PutBatch(ctx, nil); err != nil {
		t.Fatalf("PutBatch(nil): %v", err)
	}
	if c, _ := store.Count(ctx); c != N {
		t.Fatalf("post-nil-batch count = %d, want %d", c, N)
	}
	// 非法项（空 id）立即回滚并报错。
	bad := []VectorPutItem{{MemoryID: "", Vec: []float32{1, 2, 3}}}
	if err := store.PutBatch(ctx, bad); err == nil {
		t.Fatal("PutBatch empty id: want error")
	}
	if c, _ := store.Count(ctx); c != N {
		t.Fatalf("post-bad-batch count = %d, want %d (应回滚)", c, N)
	}
}

// B2（L-236）· BackfillVectors 在 ≥800 条 pending 下，单次调用一次收敛（不再逐条 Put 超 ctx），
// Count==N、幂等（二次 vectorized=0）。计时打印供回归对照。
func TestSQLiteBackendBackfillVectorsLargeBatch(t *testing.T) {
	ctx := context.Background()
	b := newPersistBackend(t, filepathTmp(t, "bfl.sqlite"))
	defer b.Close()
	sb := b
	const n = 800
	for i := 0; i < n; i++ {
		putVectorMemory(t, sb, sprintfID("bfl", i), "t1", "u1", factText(i), "sqlite:vector")
	}
	t0 := time.Now()
	rep, err := sb.BackfillVectors(ctx, HashEmbedder(32))
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("BackfillVectors (large): %v", err)
	}
	if rep.Scanned != n {
		t.Fatalf("scanned = %d, want %d", rep.Scanned, n)
	}
	if rep.Vectorized != n {
		t.Fatalf("vectorized = %d, want %d", rep.Vectorized, n)
	}
	if rep.Failed != 0 {
		t.Fatalf("failed = %d, want 0", rep.Failed)
	}
	if c, _ := sb.vec.Count(ctx); c != n {
		t.Fatalf("post-backfill count = %d, want %d", c, n)
	}
	// 幂等：二次 backfill 不重复增长。
	rep2, err := sb.BackfillVectors(ctx, HashEmbedder(32))
	if err != nil {
		t.Fatalf("BackfillVectors 2nd: %v", err)
	}
	if rep2.Vectorized != 0 {
		t.Fatalf("2nd vectorized = %d, want 0 (idempotent)", rep2.Vectorized)
	}
	t.Logf("backfill %d items in one call took %s", n, elapsed)
}

// ------------------------------------------------------------------
// 测试小工具
// ------------------------------------------------------------------

// filepathTmp 在测试临时目录下生成 SQLite 文件路径（每个用例隔离）。
func filepathTmp(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// factText 生成含中文的测试事实文本（覆盖多语言向量路径）。
func factText(i int) string {
	zh := []string{
		"上海是中国的一座城市",
		"北京是中国的首都",
		"杭州以西湖闻名",
		"深圳是科技中心",
		"成都以美食著称",
		"广州是南方门户",
		"武汉是九省通衢",
		"西安是古都",
		"南京是历史名城",
		"重庆是山城",
	}
	return zh[i%len(zh)] + " #" + strconv.Itoa(i)
}

// sprintfID 拼接 id 前缀与序号。
func sprintfID(prefix string, i int) string {
	return prefix + strconv.Itoa(i)
}

// itoa 整型转字符串。
func itoaStr(i int) string {
	return strconv.Itoa(i)
}

// mustJSON 把任意值编码为 JSON 字符串（测试用，失败即 fatal）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
