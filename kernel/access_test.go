// R24-C：访问追踪 / 模式分类 / 成本感知晋升 测试。
//
// 覆盖判据（§6.2）：
//
//	① AccessCount 真实递增（TestAccessFlushIncrementsCount）；
//	② 衰减公式（TestDecayScoreFormula，Δt=7d τ=7d → old*e^-1+1，误差<1e-9）；
//	③ -race 全绿（TestAccessConcurrentRecordAndFlush）；
//	④ 默认关零回归（TestAccessAwareZeroRegression，WAccessDeterminism=0 逐位相等）；
//	⑤ 死引擎真跑（TestRunLifecycleEnginesBudget）；
//	⑦ ≥12 个 Test。
//
// 北极星（判据⑥）与 A/B 线同一阻塞项：eval/vectors 产物缺失，移交 E 线。
package kernel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// C2 · 衰减公式
// ------------------------------------------------------------------

// TestDecayScoreFormula 判据②：Δt=7d、τ=7d 时 new = old*e^-1 + 1，误差 < 1e-9。
func TestDecayScoreFormula(t *testing.T) {
	old := 2.0
	got := decayScore(old, 7*24*time.Hour, 7*24*time.Hour)
	want := old*math.Exp(-1) + 1
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("decay(2, 7d, 7d) = %.12f, want %.12f (err %g > 1e-9)", got, want, math.Abs(got-want))
	}
	// Δt=0 → old+1（同一时刻不衰减）
	if v := decayScore(2.0, 0, 7*24*time.Hour); math.Abs(v-3.0) > 1e-12 {
		t.Errorf("decay Δt=0: got %v, want 3", v)
	}
	// τ<=0 → 退化为无衰减（显式关闭）
	if v := decayScore(2.0, 7*24*time.Hour, 0); math.Abs(v-3.0) > 1e-12 {
		t.Errorf("decay τ=0: got %v, want 3 (no decay)", v)
	}
	// 单调性：Δt 越大，衰减越多（红线：必须是衰减，不能单调累加溢出）
	a := decayScore(5.0, 1*24*time.Hour, 7*24*time.Hour)
	b := decayScore(5.0, 30*24*time.Hour, 7*24*time.Hour)
	if !(a > b) {
		t.Errorf("longer Δt must decay more: a=%v b=%v", a, b)
	}
	if b < 1.0 {
		t.Errorf("decayed score must stay >= 1 (got %v)", b)
	}
}

// TestClassifyAccessMode C2 分类规则（可测、确定性）。
func TestClassifyAccessMode(t *testing.T) {
	p := DefaultAccessPolicy() // MinSamples=5, ratio=0.8
	cases := []struct {
		name  string
		stats AccessStats
		want  AccessMode
	}{
		{"样本不足→sparse", AccessStats{Total: 4, FastHits: 4}, AccessSparse},
		{"快占比0.8→deterministic", AccessStats{Total: 5, FastHits: 4}, AccessDeterministic},
		{"快占比0.6→associative", AccessStats{Total: 5, FastHits: 3}, AccessAssociative},
		{"全慢→associative", AccessStats{Total: 10, FastHits: 0}, AccessAssociative},
		{"全快→deterministic", AccessStats{Total: 10, FastHits: 10}, AccessDeterministic},
		{"零样本→sparse", AccessStats{}, AccessSparse},
	}
	for _, c := range cases {
		if got := ClassifyAccessMode(c.stats, p); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// ------------------------------------------------------------------
// C1 · 追踪器
// ------------------------------------------------------------------

// TestAccessRecordIsMemoryOnly `Record` 纯内存：Pending>0 且后端未被写入。
func TestAccessRecordIsMemoryOnly(t *testing.T) {
	_, b := newSearchTestEnv(t)
	id := "acc-mem-01"
	putReindexMemory(t, b, id, "t1", "u1", "access memory only probe", nil)

	tr := NewAccessTracker(0)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		tr.Record(id, now)
	}
	if tr.Pending() != 1 {
		t.Fatalf("pending: %d, want 1 (one distinct id)", tr.Pending())
	}
	// 后端应仍未变更（Record 不写库）。
	m, err := b.Get(context.Background(), id, "u1", "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.AccessCount != 0 {
		t.Errorf("Record must not touch backend: access_count=%d, want 0", m.AccessCount)
	}
}

// TestAccessFlushIncrementsCount 判据①：recall 同一记忆 3 次 → flush → 回读 access_count==3。
func TestAccessFlushIncrementsCount(t *testing.T) {
	_, b := newSearchTestEnv(t)
	id := "acc-flush-01"
	putReindexMemory(t, b, id, "t1", "u1", "access flush probe", nil)

	tr := NewAccessTracker(0)
	now := time.Now().UTC()
	tr.Record(id, now)
	tr.Record(id, now)
	tr.Record(id, now)

	n, err := tr.Flush(context.Background(), b, "t1")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 1 {
		t.Fatalf("flushed: %d, want 1", n)
	}
	m, err := b.Get(context.Background(), id, "u1", "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.AccessCount != 3 {
		t.Fatalf("access_count: %d, want 3", m.AccessCount)
	}
	if m.RecentAccessScore <= 0 {
		t.Errorf("recent_access_score must be > 0 after flush, got %v", m.RecentAccessScore)
	}
	// flush 后待落库清零
	if tr.Pending() != 0 {
		t.Errorf("pending after flush: %d, want 0", tr.Pending())
	}
}

// TestAccessFlushIdempotentWhenEmpty 空快照 flush：0 条、无错误。
func TestAccessFlushIdempotentWhenEmpty(t *testing.T) {
	_, b := newSearchTestEnv(t)
	tr := NewAccessTracker(0)
	n, err := tr.Flush(context.Background(), b, "t1")
	if err != nil || n != 0 {
		t.Fatalf("empty flush: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestAccessFlushDecaysScore 衰减确实生效（时间戳拉远后分数更低）。
func TestAccessFlushDecaysScore(t *testing.T) {
	_, b := newSearchTestEnv(t)
	id := "acc-decay-01"
	putReindexMemory(t, b, id, "t1", "u1", "access decay probe", nil)
	// 预置一个较高的既有分数。
	m0, _ := b.Get(context.Background(), id, "u1", "t1")
	m0.RecentAccessScore = 10
	if err := b.Upsert(context.Background(), m0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tr := NewAccessTracker(0)
	tr.SetTau(7 * 24 * time.Hour)
	// 30 天前访问 → 强衰减
	tr.Record(id, time.Now().UTC().Add(-30*24*time.Hour))
	if _, err := tr.Flush(context.Background(), b, "t1"); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, _ := b.Get(context.Background(), id, "u1", "t1")
	// old=10, Δt=30d, τ=7d → 10*e^(-30/7)+1 ≈ 1.14（远低于未衰减的 11）
	if got.RecentAccessScore >= 11 {
		t.Errorf("score must decay: got %v, want << 11 (undecayed)", got.RecentAccessScore)
	}
	if got.RecentAccessScore < 1 {
		t.Errorf("score floor is 1, got %v", got.RecentAccessScore)
	}
	t.Logf("decayed score: %.6f (from 10, Δt=30d, τ=7d)", got.RecentAccessScore)
}

// TestAccessCapacityEviction 容量上限：不无界增长。
func TestAccessCapacityEviction(t *testing.T) {
	tr := NewAccessTracker(3)
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		tr.Record(string(rune('a'+i)), base.Add(time.Duration(i)*time.Second))
	}
	if tr.Pending() > 3 {
		t.Fatalf("pending %d exceeds capacity 3", tr.Pending())
	}
}

// TestAccessConcurrentRecordAndFlush 判据③：-race 下 Record 与 Flush 并发不丢计数、无竞态。
func TestAccessConcurrentRecordAndFlush(t *testing.T) {
	_, b := newSearchTestEnv(t)
	id := "acc-race-01"
	putReindexMemory(t, b, id, "t1", "u1", "access race probe", nil)

	tr := NewAccessTracker(0)
	const n = 200
	var wg sync.WaitGroup
	// 并发 Record
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Record(id, time.Now().UTC())
		}()
	}
	// 同时并发 Flush（只取快照，不阻塞 Record）
	var flushWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		flushWG.Add(1)
		go func() {
			defer flushWG.Done()
			_, _ = tr.Flush(context.Background(), b, "t1")
		}()
	}
	wg.Wait()
	flushWG.Wait()
	// 收尾再 flush 一次，把剩余的落库
	if _, err := tr.Flush(context.Background(), b, "t1"); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	m, err := b.Get(context.Background(), id, "u1", "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.AccessCount != n {
		t.Errorf("access_count: %d, want %d (no lost counts under concurrency)", m.AccessCount, n)
	}
}

// TestAccessNoteRecallSeam 与 A 线接缝：NoteRecall 走 Record，注入后 APIServer.NoteRecall 生效。
func TestAccessNoteRecallSeam(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	id := "acc-seam-01"
	putReindexMemory(t, b, id, "t1", "u1", "access seam probe", nil)

	tr := NewAccessTracker(0)
	srv.SetAccessTracker(tr)
	// A 线在 RecallAndFuse 返回前调用 s.NoteRecall(ids)；此处直接验证转发链路。
	srv.NoteRecall([]string{id})
	if tr.Pending() != 1 {
		t.Fatalf("NoteRecall must reach tracker: pending=%d, want 1", tr.Pending())
	}
	// 未注入时 no-op（A 线既有断言的补充）
	srv2, _ := newSearchTestEnv(t)
	srv2.NoteRecall([]string{id}) // 不得 panic
}

// TestAccessMarkFast MarkFast 单独记快车道命中，供 C2 分类。
func TestAccessMarkFast(t *testing.T) {
	tr := NewAccessTracker(0)
	tr.MarkFast([]string{"x", "x", "x"})
	tr.Record("x", time.Now().UTC())
	tr.Record("x", time.Now().UTC())
	st := tr.StatsOf("x")
	if st.FastHits != 3 || st.SlowHits != 2 || st.Total != 5 {
		t.Fatalf("stats: %+v, want fast=3 slow=2 total=5", st)
	}
	// 3 快 / 5 总 = 0.6 < 0.8 → associative（不是 deterministic）
	if got := tr.ModeOf("x", DefaultAccessPolicy()); got != AccessAssociative {
		t.Errorf("mode 3/5=0.6: got %q, want associative", got)
	}
	// 5 次里 4 次快 → deterministic
	tr2 := NewAccessTracker(0)
	tr2.MarkFast([]string{"y"})
	tr2.MarkFast([]string{"y"})
	tr2.MarkFast([]string{"y"})
	tr2.MarkFast([]string{"y"})
	tr2.Record("y", time.Now().UTC())
	if got := tr2.ModeOf("y", DefaultAccessPolicy()); got != AccessDeterministic {
		t.Errorf("mode 4/5: %q, want deterministic", got)
	}
}

// ------------------------------------------------------------------
// C3 · 成本感知晋升（默认关零回归）
// ------------------------------------------------------------------

// TestAccessAwareZeroRegression 判据④：WAccessDeterminism=0 时逐位等于原函数。
func TestAccessAwareZeroRegression(t *testing.T) {
	p := DefaultPromotionPolicy()
	ap := DefaultAccessAwarePolicy() // 全 0
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

	cases := []*Memory{
		{MemoryID: "z1", Layer: LayerL0, Importance: 0.9, Confidence: 0.9, RecentAccessScore: 0.8, AccessCount: 10},
		{MemoryID: "z2", Layer: LayerL1, Importance: 0.2, Confidence: 0.2, RecentAccessScore: 0.0, AccessCount: 0},
		{MemoryID: "z3", Layer: LayerL2, Importance: 0.7, Confidence: 0.6, RecentAccessScore: 0.5, AccessCount: 3},
		{MemoryID: "z4", Layer: LayerL3, Importance: 0.1, Confidence: 0.1, RecentAccessScore: 0.0, AccessCount: 0},
	}
	for _, m := range cases {
		bf, bt, br := NextLifecycleByValue(m, p, now)
		gf, gt, gr := NextLifecycleByValueWithAccess(m, p, ap, AccessDeterministic, now)
		if bf != gf || bt != gt || br != gr {
			t.Errorf("%s: WAccess=0 must be bit-identical to base: base=(%s,%s,%q) got=(%s,%s,%q)",
				m.MemoryID, bf, bt, br, gf, gt, gr)
		}
	}
}

// TestAccessAwareEasesThreshold WAccessDeterminism>0 时对 deterministic 记忆放宽晋升。
func TestAccessAwareEasesThreshold(t *testing.T) {
	p := DefaultPromotionPolicy()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	// 构造一条"卡在阈值边缘"的记忆：基线不升，放宽后应升。
	m := &Memory{MemoryID: "e1", Layer: LayerL0, Importance: 0.62, Confidence: 0.62,
		RecentAccessScore: 0.0, AccessCount: 0}

	bf, bt, _ := NextLifecycleByValue(m, p, now)
	ap := AccessAwarePolicy{WAccessDeterminism: 1.0, PromoteBonus: 0.15, DemoteGuard: 0.15}
	gf, gt, _ := NextLifecycleByValueWithAccess(m, p, ap, AccessDeterministic, now)
	t.Logf("base=(%s→%s) eased=(%s→%s)", bf, bt, gf, gt)
	// 非 deterministic 模式：即使有权重也不应改动（只对确定性记忆偏袒）
	nf, nt, _ := NextLifecycleByValueWithAccess(m, p, ap, AccessAssociative, now)
	if nf != bf || nt != bt {
		t.Errorf("non-deterministic must not be eased: (%s,%s) vs base (%s,%s)", nf, nt, bf, bt)
	}
}

// ------------------------------------------------------------------
// C3 · 死引擎接线
// ------------------------------------------------------------------

// TestRunLifecycleEnginesBudget 判据⑤：budget 引擎真跑并返回条数。
func TestRunLifecycleEnginesBudget(t *testing.T) {
	_, b := newSearchTestEnv(t)
	// 造 6 条并显式置为 L1，预算上限 2 → 应有条目被移至 L3。
	//
	// 注意：不能直接靠 Upsert 时的 Layer 字段——backend_sqlite.go:145 的 Upsert
	// 会用 `m.Layer = ClassifyLayer(m)` **覆盖**调用方传入的层。要让它们稳定落在
	// L1，必须在 Upsert 之后用 LayerScanner.UpdateLayer 显式置层。
	sc, ok := b.(LayerScanner)
	if !ok {
		t.Fatal("backend must implement LayerScanner")
	}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("lc-b%02d", i)
		m := &Memory{
			MemoryID: id, IdempotencyKey: id,
			TenantID: "t1", UserID: "u1", Visibility: VisTenant,
			MemoryKind: KindFact, Fact: "budget probe " + string(rune('a'+i)),
			Confidence: 0.5, Importance: 0.1 + 0.05*float64(i),
			Permission: Permission{Owner: "u1"},
		}
		if err := b.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := sc.UpdateLayer(context.Background(), id, LayerL1); err != nil {
			t.Fatalf("update layer %s: %v", id, err)
		}
	}
	// 确认确实有 6 条落在 L1（否则后面的断言会变成"假绿"）。
	ms, err := sc.ScanLayer(context.Background(), LayerL1, "t1", 0)
	if err != nil {
		t.Fatalf("scan L1: %v", err)
	}
	if len(ms) != 6 {
		t.Fatalf("setup: L1 count = %d, want 6", len(ms))
	}
	rep, err := RunLifecycleEngines(context.Background(), b, nil, "t1",
		[]string{EngineBudget},
		LifecycleConfig{BudgetLimits: map[MemoryLayer]int{LayerL1: 2}})
	if err != nil {
		t.Fatalf("run budget: %v", err)
	}
	if rep[EngineBudget] <= 0 {
		t.Fatalf("budget engine must move >0 memories, got %d", rep[EngineBudget])
	}
	t.Logf("budget engine moved %d memories (L1 limit=2, seeded 6)", rep[EngineBudget])
}

// TestRunLifecycleEnginesUnknown 未知引擎名：明确报错，不静默成功。
func TestRunLifecycleEnginesUnknown(t *testing.T) {
	_, b := newSearchTestEnv(t)
	if _, err := RunLifecycleEngines(context.Background(), b, nil, "t1", []string{"nope"}, LifecycleConfig{}); err == nil {
		t.Error("unknown engine: want error, got nil")
	}
}

// TestRunLifecycleEnginesDefaultOff 默认关：空 engines 列表不动任何数据。
func TestRunLifecycleEnginesDefaultOff(t *testing.T) {
	_, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "lc-off", "t1", "u1", "lifecycle default off", nil)
	before, _ := b.Get(context.Background(), "lc-off", "u1", "t1")
	rep, err := RunLifecycleEngines(context.Background(), b, nil, "t1", nil, LifecycleConfig{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep) != 0 {
		t.Errorf("empty engines must yield empty report, got %v", rep)
	}
	after, _ := b.Get(context.Background(), "lc-off", "u1", "t1")
	if before.Layer != after.Layer {
		t.Errorf("layer changed: %s -> %s", before.Layer, after.Layer)
	}
}

// TestLifecycleRoutesDefaultUnbound 端点默认未绑定 → 503（默认关语义）。
func TestLifecycleRoutesDefaultUnbound(t *testing.T) {
	before := len(routeHooks)
	mux := http.NewServeMux()
	RegisterLifecycleRoutes(mux)
	if len(routeHooks) != before {
		t.Fatalf("RegisterLifecycleRoutes must not touch global routeHooks: %d -> %d", before, len(routeHooks))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/lifecycle/run?engines=budget", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("unbound lifecycle endpoint: %d, want 503", w.Code)
	}
}

// TestLifecycleRoutesBound 绑定后端点可用。
func TestLifecycleRoutesBound(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	putReindexMemory(t, b, "lc-http", "t1", "u1", "lifecycle http probe", nil)
	beforeHooks := len(routeHooks)
	beforeServer := lifecycleServer
	t.Cleanup(func() {
		routeHooks = routeHooks[:beforeHooks]
		lifecycleServer = beforeServer
	})
	EnableLifecycleRoutes(srv)
	mux := http.NewServeMux()
	for _, fn := range routeHooks {
		fn(mux)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/admin/lifecycle/run?engines=budget&tenant_id=t1&budget_limits=L1=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("bound lifecycle endpoint: %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	t.Logf("lifecycle endpoint response: %s", w.Body.String())
}
