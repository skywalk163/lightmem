// R24-A：L2 静态记忆确定性查找表 + 快车道测试。
// 真源：lightmem.md 任务书 R24 §4「A 线」、§9「接口冻结」。
//
// 覆盖判据（§4.2）：
//
//	① 开关关逐位零回归（TestFastPathDisabledZeroRegression）；
//	② Rebuild 幂等确定性（TestLookupRebuildDeterministic）；
//	③ 快慢 top1 一致率 ≥95%（TestFastSlowTop1Consistency）；
//	④ bench 快车道 ≤ 慢车道 1/5（BenchmarkRecallFastPath / BenchmarkRecallSlowPath）；
//	⑤ 可重建（TestLookupRebuildRestoresAll）+ Rebuild 只读（TestLookupRebuildReadOnly）；
//	⑥ 北极星 M1 不降由 gate 整体回归保障（本文件不单独断言）；
//	⑦ ≥12 个 Test（本文件 20 个业务 Test + 2 个回退/接缝 Test = 22 个）。
//
// 测试只走公开接口：NewLookupIndex / Rebuild / Add / Remove / Lookup / Stats / Built /
// SetLookupIndex / SetLookupEnabled / RecallAndFuse / NoteRecall。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// tbHelper 适配 *testing.T 与 *testing.B 的最小接口（fatalf 语义一致）。
type tbHelper interface {
	Helper()
	Fatalf(format string, args ...any)
}

// ------------------------------------------------------------------
// 装配 helper
// ------------------------------------------------------------------

// putLookupMemory 写一条带 ReferenceFrame 的记忆（与 putSearchMemory 同协议，
// 额外携带 layer / entity / category / importance / recent / confidence）。
func putLookupMemory(t tbHelper, b StorageBackend, id, tenant, user string, vis Visibility,
	layer MemoryLayer, entity, category, fact string, importance, recent, conf float64) {
	t.Helper()
	putLookupMemoryFull(t, b, id, tenant, user, vis, layer, entity, category, fact,
		importance, recent, conf, nil)
}

// putLookupMemoryFull 与 putLookupMemory 同协议，额外允许显式指定 OccurredAt，
// 使慢车道 freshness 信号构成确定性梯度（避免同构语料下按时效打平）。
// occurredAt 传 nil 时由后端落 RecordedAt（与既有行为一致）。
func putLookupMemoryFull(t tbHelper, b StorageBackend, id, tenant, user string, vis Visibility,
	layer MemoryLayer, entity, category, fact string, importance, recent, conf float64,
	occurredAt *time.Time) {
	t.Helper()
	m := &Memory{
		MemoryID:          id,
		IdempotencyKey:    id,
		TenantID:          tenant,
		UserID:            user,
		Visibility:        vis,
		MemoryKind:        KindFact,
		Fact:              fact,
		Confidence:        conf,
		Layer:             layer,
		ReferenceFrame:    ReferenceFrame{Entity: entity, Category: category},
		Importance:        importance,
		RecentAccessScore: recent,
		OccurredAt:        occurredAt,
		Permission:        Permission{Owner: user},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// seedLookupCorpus 写 n 条同构 L2 语料：实体 客户01..客户NN、属性 偏好、
// fact "客户NN的偏好是事项NN"、id "mNN"。唯一精确命中场景。
// importance 与 OccurredAt 随序号严格递增：让慢车道 value/freshness 两个逐条目
// 常数信号有真实梯度，避免同构语料下它们全部打平并按 MemoryID 升序把 m01 抬到
// rank1（RRF 名次融合的非线性会压过文本信号的 rank1 优势，造成快慢车道系统性分歧）。
func seedLookupCorpus(t tbHelper, b StorageBackend, n int, tenant, user string, layer MemoryLayer) {
	t.Helper()
	seedLookupCorpusRange(t, b, 1, n, tenant, user, layer)
}

// seedLookupCorpusRange 从 start 号开始写 n 条语料（id m<start>..m<start+n-1>），
// 用于混入不与主语料 MemoryID 冲突的旁支语料（如他租户/低层干扰项）。
func seedLookupCorpusRange(t tbHelper, b StorageBackend, start, n int, tenant, user string, layer MemoryLayer) {
	t.Helper()
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := start; i < start+n; i++ {
		ent := fmt.Sprintf("客户%02d", i)
		fact := fmt.Sprintf("%s的偏好是事项%02d", ent, i)
		imp := 0.3 + 0.02*float64(i-1)
		occ := base.Add(time.Duration(i) * time.Minute)
		putLookupMemoryFull(t, b, fmt.Sprintf("m%02d", i), tenant, user, VisTenant,
			layer, ent, "偏好", fact, imp, 0.5, 0.9, &occ)
	}
}

// corpusQuery 返回第 i 条语料的查询文本（与索引侧 entity/attr 提取同口径）。
func corpusQuery(i int) string { return fmt.Sprintf("客户%02d的偏好", i) }

// rebuildIndex 用后端 LayerScanner 重建索引。
func rebuildIndex(t tbHelper, b StorageBackend, idx *LookupIndex, tenant string) {
	t.Helper()
	sc, ok := b.(LayerScanner)
	if !ok {
		t.Fatalf("backend %s does not implement LayerScanner", b.Name())
	}
	if _, err := idx.Rebuild(context.Background(), sc, tenant); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

// lookupOne 断言 Lookup 恰好命中 want 一个 id 且 exact 符合预期。
func lookupOne(t *testing.T, idx *LookupIndex, text, want string, wantExact bool) {
	t.Helper()
	ids, exact := idx.Lookup(text)
	if exact != wantExact || len(ids) != 1 || ids[0] != want {
		t.Fatalf("Lookup(%q): want (%v, exact=%v) got ids=%v exact=%v", text, []string{want}, wantExact, ids, exact)
	}
}

// checkLookupMiss 断言 Lookup 无命中。
func checkLookupMiss(t *testing.T, idx *LookupIndex, text string) {
	t.Helper()
	ids, exact := idx.Lookup(text)
	if len(ids) != 0 || exact {
		t.Fatalf("Lookup(%q): want miss, got ids=%v exact=%v", text, ids, exact)
	}
}

// equalFused 比较两条结果的 Fused 主体（忽略 trace 时间戳）。
func equalFused(a, b []FusedResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].MemoryID != b[i].MemoryID || a[i].Fact != b[i].Fact ||
			a[i].Score != b[i].Score || a[i].Layer != b[i].Layer {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------
// 索引层测试（LookupIndex 纯内存，不依赖后端）
// ------------------------------------------------------------------

// TestLookupEmptyIndex 空/未构建索引：miss + Built=false（§4.1 no_index 前置）。
func TestLookupEmptyIndex(t *testing.T) {
	idx := NewLookupIndex()
	if idx.Built() {
		t.Fatal("new index must not be built")
	}
	checkLookupMiss(t, idx, "客户01的偏好")
	st := idx.Stats()
	if st.Keys != 0 || st.Entries != 0 || st.Ambiguous != 0 || st.RebuiltAtUnix != 0 {
		t.Fatalf("empty stats: %+v", st)
	}
}

// TestLookupAddExactHit Add 单条 -> Lookup 精确唯一命中。
func TestLookupAddExactHit(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{
		MemoryID:       "m01",
		Fact:           "客户01的偏好是事项01",
		Layer:          LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Importance:     0.8,
		Confidence:     0.9,
	})
	lookupOne(t, idx, "客户01的偏好", "m01", true)
	if !idx.Built() {
		t.Fatal("after Add index must be built")
	}
}

// TestLookupKeyNormalize 键构造归一化：索引侧与查询侧标点/空白差异仍同键。
func TestLookupKeyNormalize(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "m01", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户01。", Category: "偏好 "},
		Confidence:     0.9})
	idx.Add(&Memory{MemoryID: "m02", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "项目X　", Category: "预算。"},
		Confidence:     0.9})
	lookupOne(t, idx, "客户01的偏好", "m01", true)
	lookupOne(t, idx, "项目X的预算", "m02", true)
}

// TestLookupEntityAttrFallbackToFact ReferenceFrame 为空时退化为 Fact 正则提取。
func TestLookupEntityAttrFallbackToFact(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "m01", Layer: LayerL2,
		Fact:       "客户A偏好周一开会",
		Confidence: 0.9})
	lookupOne(t, idx, "客户A的偏好", "m01", true)
}

// TestLookupDeterministicOrder 同键多值确定性排序：
// (L2 > L1, IntrinsicValue 降序, memory_id 升序)，禁止 map 迭代序。
func TestLookupDeterministicOrder(t *testing.T) {
	idx := NewLookupIndex()
	// value = 0.5*importance + 0.3*recent + 0.2*confidence（clamp01）
	mk := func(id string, layer MemoryLayer, imp, rec, conf float64) *Memory {
		return &Memory{MemoryID: id, Layer: layer,
			ReferenceFrame:    ReferenceFrame{Entity: "客户A", Category: "偏好"},
			Importance:        imp,
			RecentAccessScore: rec,
			Confidence:        conf,
		}
	}
	idx.Add(mk("m-a", LayerL1, 0.9, 0.5, 0.9)) // value 0.78, L1
	idx.Add(mk("m-b", LayerL2, 0.9, 0.5, 0.9)) // value 0.78, L2
	idx.Add(mk("m-c", LayerL2, 0.1, 0.5, 0.9)) // value 0.38, L2
	idx.Add(mk("m-d", LayerL2, 0.9, 0.5, 0.9)) // value 0.78, L2, id > m-b

	ids, exact := idx.Lookup("客户A的偏好")
	if !exact {
		t.Fatal("want exact hit")
	}
	want := []string{"m-b", "m-d", "m-c", "m-a"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("order: want %v got %v", want, ids)
	}
}

// TestLookupAmbiguousKey 同键两条 -> Lookup 返回全部 id + exact，Stats.Ambiguous=1。
func TestLookupAmbiguousKey(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "m01", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Confidence:     0.9})
	idx.Add(&Memory{MemoryID: "m02", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Importance:     0.9,
		Confidence:     0.9})
	ids, exact := idx.Lookup("客户01的偏好")
	if !exact || len(ids) != 2 {
		t.Fatalf("want 2 ids exact=true, got ids=%v exact=%v", ids, exact)
	}
	if st := idx.Stats(); st.Ambiguous != 1 || st.Entries != 2 || st.Keys != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// TestLookupStatsCountsAndRemove Add/Remove 的统计与键回收。
func TestLookupStatsCountsAndRemove(t *testing.T) {
	idx := NewLookupIndex()
	mk := func(id string) *Memory {
		return &Memory{MemoryID: id, Layer: LayerL2,
			ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
			Confidence:     0.9}
	}
	idx.Add(mk("m1"))
	idx.Add(mk("m2"))
	idx.Add(mk("m3"))
	idx.Add(&Memory{MemoryID: "m4", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户02", Category: "预算"},
		Confidence:     0.9})
	st := idx.Stats()
	if st.Keys != 2 || st.Entries != 4 || st.Ambiguous != 1 {
		t.Fatalf("after add: %+v", st)
	}

	idx.Remove("m1")
	if st := idx.Stats(); st.Entries != 3 || st.Ambiguous != 1 {
		t.Fatalf("after remove m1: %+v", st)
	}
	idx.Remove("m2")
	idx.Remove("m3") // 该键清空 -> 键回收
	st = idx.Stats()
	if st.Keys != 1 || st.Entries != 1 || st.Ambiguous != 0 {
		t.Fatalf("after remove m2/m3: %+v", st)
	}
	checkLookupMiss(t, idx, "客户01的偏好")
	lookupOne(t, idx, "客户02的预算", "m4", true)
}

// TestLookupSkipsL0L3AndNoEntity 非 L2/L1 层与无实体记忆不入索引。
func TestLookupSkipsL0L3AndNoEntity(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "l0", Layer: LayerL0,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Confidence:     0.9})
	idx.Add(&Memory{MemoryID: "l3", Layer: LayerL3,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Confidence:     0.9})
	idx.Add(&Memory{MemoryID: "ne", Layer: LayerL2,
		Fact:       "每周一开会",
		Confidence: 0.9}) // 无实体候选
	if idx.Built() {
		t.Fatal("no indexed entry must not mark built")
	}
	if st := idx.Stats(); st.Entries != 0 {
		t.Fatalf("want 0 entries, got %+v", st)
	}
	idx.Add(&Memory{MemoryID: "l1", Layer: LayerL1,
		ReferenceFrame: ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Confidence:     0.9})
	if !idx.Built() {
		t.Fatal("L1 entry must mark built")
	}
	if st := idx.Stats(); st.Entries != 1 {
		t.Fatalf("want 1 entry, got %+v", st)
	}
	lookupOne(t, idx, "客户01的偏好", "l1", true)
}

// TestLookupTrigGramApprox 精确未命中但 3-gram 唯一候选：返回候选 id，exact=false。
func TestLookupTrigGramApprox(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "m-ab", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户AB", Category: "偏好"},
		Confidence:     0.9})
	// 查询文本提取实体 "客户A"（前缀），与索引 "客户AB" 共享 3-gram "客户A"。
	lookupOne(t, idx, "客户A的偏好", "m-ab", false)
}

// TestLookupTrigGramAmbiguous 3-gram 命中多桶 -> 歧义，不返回静默结果。
func TestLookupTrigGramAmbiguous(t *testing.T) {
	idx := NewLookupIndex()
	idx.Add(&Memory{MemoryID: "m-ab", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户AB", Category: "偏好"},
		Confidence:     0.9})
	idx.Add(&Memory{MemoryID: "m-ac", Layer: LayerL2,
		ReferenceFrame: ReferenceFrame{Entity: "客户AC", Category: "偏好"},
		Confidence:     0.9})
	// 查询 "客户A"：两个实体（客户AB / 客户AC）都落在 3-gram 桶 "客户A" 下。
	checkLookupMiss(t, idx, "客户A的偏好")
}

// ------------------------------------------------------------------
// Rebuild 层测试（真实 SQLite 后端 + LayerScanner）
// ------------------------------------------------------------------

// TestLookupRebuildDeterministic 判据②：Rebuild 两次 Stats 与全 key 查询序列完全一致。
func TestLookupRebuildDeterministic(t *testing.T) {
	_, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 30, "t1", "u1", LayerL2)
	// 混入他租户 + 低层（id 错开到 m31~m33，避免按 MemoryID 覆盖主语料 m01~m03），
	// 验证 tenant 过滤与分层（L1 不混入 t1 索引）。
	seedLookupCorpusRange(t, b, 31, 3, "t2", "u2", LayerL1)

	run := func() *LookupIndex {
		idx := NewLookupIndex()
		rebuildIndex(t, b, idx, "t1")
		return idx
	}
	i1, i2 := run(), run()

	s1, s2 := i1.Stats(), i2.Stats()
	if s1 != s2 {
		t.Fatalf("stats differ between rebuilds: %+v vs %+v", s1, s2)
	}
	if s1.Entries != 30 {
		t.Fatalf("want 30 entries (tenant t1, L2+L1), got %+v", s1)
	}
	for i := 1; i <= 30; i++ {
		q := corpusQuery(i)
		ids1, ex1 := i1.Lookup(q)
		ids2, ex2 := i2.Lookup(q)
		if ex1 != ex2 || fmt.Sprint(ids1) != fmt.Sprint(ids2) {
			t.Fatalf("rebuild deterministic mismatch at %q: %v/%v vs %v/%v", q, ids1, ex1, ids2, ex2)
		}
		if !ex1 || len(ids1) != 1 || ids1[0] != fmt.Sprintf("m%02d", i) {
			t.Fatalf("corpus %d: want unique m%02d, got %v exact=%v", i, i, ids1, ex1)
		}
	}
}

// TestLookupRebuildRestoresAll 判据⑤：清空索引后 Rebuild 恢复全部条目。
func TestLookupRebuildRestoresAll(t *testing.T) {
	_, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 30, "t1", "u1", LayerL2)

	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	if st := idx.Stats(); st.Entries != 30 {
		t.Fatalf("initial rebuild: %+v", st)
	}
	for i := 1; i <= 30; i++ {
		idx.Remove(fmt.Sprintf("m%02d", i))
	}
	if st := idx.Stats(); st.Entries != 0 {
		t.Fatalf("after remove all: %+v", st)
	}
	rebuildIndex(t, b, idx, "t1") // 可重建
	st := idx.Stats()
	if st.Entries != 30 || st.Keys != 30 || st.Ambiguous != 0 {
		t.Fatalf("after restore rebuild: %+v", st)
	}
	for i := 1; i <= 30; i++ {
		lookupOne(t, idx, corpusQuery(i), fmt.Sprintf("m%02d", i), true)
	}
}

// TestLookupRebuildReadOnly 判据⑤补充：Rebuild 只读 LayerScanner，不写后端。
func TestLookupRebuildReadOnly(t *testing.T) {
	_, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 10, "t1", "u1", LayerL2)
	sc, ok := b.(LayerScanner)
	if !ok {
		t.Fatal("backend must implement LayerScanner")
	}
	ctx := context.Background()
	snap := func() ([]string, string) {
		ms, err := sc.ScanLayer(ctx, "", "t1", 0)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids := make([]string, 0, len(ms))
		var facts string
		for _, m := range ms {
			ids = append(ids, m.MemoryID)
			facts += m.Fact + ";"
		}
		return ids, facts
	}
	beforeIDs, beforeFacts := snap()

	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	afterIDs, afterFacts := snap()

	if fmt.Sprint(beforeIDs) != fmt.Sprint(afterIDs) || beforeFacts != afterFacts {
		t.Fatal("Rebuild must be read-only: backend data changed")
	}
	if st := idx.Stats(); st.Entries != 10 {
		t.Fatalf("rebuild entries: %+v", st)
	}
}

// ------------------------------------------------------------------
// 快车道分流测试（RecallAndFuse + SetLookupIndex/SetLookupEnabled）
// ------------------------------------------------------------------

// TestFastPathUniqueHit 唯一精确命中：快车道直接返回，不跑 8 信号融合。
func TestFastPathUniqueHit(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 1, "t1", "u1", LayerL2)
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srv.SetLookupIndex(idx)
	srv.SetLookupEnabled(true)

	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户01的偏好", TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Mode != "unique" {
		t.Fatalf("mode: want unique, got %q", res.Mode)
	}
	if res.Recalled != nil || res.FusionScores != nil {
		t.Fatalf("fast path must not run recall/fusion: recalled=%v fusion=%v", res.Recalled, res.FusionScores)
	}
	if len(res.Fused) != 1 || res.Fused[0].MemoryID != "m01" || res.Fused[0].Score != 1.0 {
		t.Fatalf("fused: %+v", res.Fused)
	}
	if res.Trace == nil || !res.Trace.FastPath || res.Trace.FallbackReason != "" {
		t.Fatalf("trace: want fast_path=true, got %+v", res.Trace)
	}
}

// TestFastPathDisabledZeroRegression 判据①：开关关闭时与未注入索引完全一致，
// 且 trace 不输出 fast_path / fallback_reason（omitempty，逐位零回归的信号）。
func TestFastPathDisabledZeroRegression(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 3, "t1", "u1", LayerL2)
	ctx := context.Background()
	rc := RouterContext{Text: "客户01的偏好", TenantID: "t1", UserID: "u1"}

	rNo, err := srv.RecallAndFuse(ctx, rc) // 未注入索引（默认关）
	if err != nil {
		t.Fatalf("recall base: %v", err)
	}

	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srv.SetLookupIndex(idx) // 注入但保持开关关
	rOff, err := srv.RecallAndFuse(ctx, rc)
	if err != nil {
		t.Fatalf("recall off: %v", err)
	}

	if rOff.Mode != rNo.Mode || !equalFused(rOff.Fused, rNo.Fused) {
		t.Fatalf("switch-off must equal baseline: mode %q vs %q; fused %+v vs %+v",
			rOff.Mode, rNo.Mode, rOff.Fused, rNo.Fused)
	}
	if rOff.Trace == nil || rOff.Trace.FastPath || rOff.Trace.FallbackReason != "" {
		t.Fatalf("switch-off trace must be unmarked: %+v", rOff.Trace)
	}
	if raw, err := json.Marshal(rOff); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if s := string(raw); strings.Contains(s, "fast_path") || strings.Contains(s, "fallback_reason") {
		t.Fatalf("switch-off JSON must omit fast-path fields: %s", s)
	}

	// 同一 server 打开开关：同一查询应走快车道（证明开关确实只影响分流）。
	srv.SetLookupEnabled(true)
	rOn, err := srv.RecallAndFuse(ctx, rc)
	if err != nil {
		t.Fatalf("recall on: %v", err)
	}
	if rOn.Trace == nil || !rOn.Trace.FastPath {
		t.Fatalf("switch-on must use fast path: %+v", rOn.Trace)
	}
}

// TestFallbackNoIndex 索引空/未构建/未注入 -> 回退慢车道，reason=no_index。
func TestFallbackNoIndex(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 1, "t1", "u1", LayerL2)
	ctx := context.Background()
	rc := RouterContext{Text: "客户01的偏好", TenantID: "t1", UserID: "u1"}

	var res RouterResult
	var err error

	// 子情况 1：未注入索引。
	srv.SetLookupEnabled(true)
	res, err = srv.RecallAndFuse(ctx, rc)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "no_index" {
		t.Fatalf("want fallback no_index, got %+v", res.Trace)
	}

	// 子情况 2：注入空索引（未 Rebuild / 未 Add）。
	srv.SetLookupIndex(NewLookupIndex())
	res, err = srv.RecallAndFuse(ctx, rc)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "no_index" {
		t.Fatalf("want fallback no_index, got %+v", res.Trace)
	}
	if len(res.Fused) == 0 {
		t.Fatal("slow path should still serve the corpus hit")
	}
}

// TestFallbackMiss 精确未命中 -> 回退慢车道，reason=miss。
func TestFallbackMiss(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 3, "t1", "u1", LayerL2)
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srv.SetLookupIndex(idx)
	srv.SetLookupEnabled(true)

	// 查询语料之外的实体键：索引无此实体、也无 3-gram 候选。
	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "项目B的预算", TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "miss" {
		t.Fatalf("want fallback miss, got %+v", res.Trace)
	}
}

// TestFallbackAmbiguous 同键多条 -> 回退慢车道，reason=ambiguous。
func TestFallbackAmbiguous(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	// 同实体同属性两条记忆 -> 主索引同键。
	putLookupMemory(t, b, "m-a", "t1", "u1", VisTenant, LayerL2, "客户01", "偏好",
		"客户01的偏好是事项一", 0.8, 0.5, 0.9)
	putLookupMemory(t, b, "m-b", "t1", "u1", VisTenant, LayerL2, "客户01", "偏好",
		"客户01的偏好是事项二", 0.8, 0.5, 0.9)
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	if st := idx.Stats(); st.Ambiguous != 1 {
		t.Fatalf("setup: want 1 ambiguous key, got %+v", st)
	}
	srv.SetLookupIndex(idx)
	srv.SetLookupEnabled(true)

	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户01的偏好", TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "ambiguous" {
		t.Fatalf("want fallback ambiguous, got %+v", res.Trace)
	}
}

// TestFastPathVisibilityGuard 索引命中但当前查询无访问权 -> 快车道拒绝并回退 miss。
func TestFastPathVisibilityGuard(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, 1, "t1", "u1", LayerL2) // t1 租户记忆
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srv.SetLookupIndex(idx)
	srv.SetLookupEnabled(true)

	// 用不相关租户 t2 的身份查询：Get/canView 会拒绝，miss 回退慢车道（慢车道也扫不到）。
	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户01的偏好", TenantID: "t2", UserID: "u2",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "miss" {
		t.Fatalf("cross-tenant must fall back miss, got %+v", res.Trace)
	}
	if len(res.Fused) != 0 {
		t.Fatalf("cross-tenant must not leak memory, got %+v", res.Fused)
	}
}

// TestFastPathSupersededExcluded 索引命中但记忆已被取代 -> 快车道拒绝（当前版本语义）。
func TestFastPathSupersededExcluded(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	sup := "m-next"
	m := &Memory{
		MemoryID:          "m-old",
		IdempotencyKey:    "m-old",
		TenantID:          "t1",
		UserID:            "u1",
		Visibility:        VisTenant,
		MemoryKind:        KindFact,
		Fact:              "客户01的偏好是事项旧版",
		Confidence:        0.9,
		Layer:             LayerL2,
		SupersededBy:      &sup,
		ReferenceFrame:    ReferenceFrame{Entity: "客户01", Category: "偏好"},
		Importance:        0.8,
		RecentAccessScore: 0.5,
		Permission:        Permission{Owner: "u1"},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	lookupOne(t, idx, "客户01的偏好", "m-old", true) // 索引侧仍在
	srv.SetLookupIndex(idx)
	srv.SetLookupEnabled(true)

	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户01的偏好", TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.Trace == nil || res.Trace.FastPath || res.Trace.FallbackReason != "miss" {
		t.Fatalf("superseded memory must not serve fast path, got %+v", res.Trace)
	}
}

// TestNoteRecallNoop 接缝空实现：access 为 nil 时 NoteRecall 不 panic、无副作用。
func TestNoteRecallNoop(t *testing.T) {
	srv, _ := newSearchTestEnv(t)
	// 直接调用：nil access 下必须 no-op。
	srv.NoteRecall([]string{"m01", "m02"})
	// 经 RecallAndFuse（快/慢两条路径）也不应 panic。
	srv.SetLookupEnabled(true)
	_, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户01的偏好", TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
}

// BenchmarkRecallFastPath 判据④：快车道唯一精确命中的单次 RecallAndFuse。
// 任务书 §4.2 判据④ 指定的对比命令为：
//
//	go test -bench BenchmarkRecallFastPath -benchmem ./kernel/
func BenchmarkRecallFastPath(b *testing.B) {
	srv, _ := benchLookupEnv(b)
	srv.SetLookupEnabled(true)
	ctx := context.Background()
	rc := RouterContext{Text: "客户15的偏好", TenantID: "t1", UserID: "u1"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := srv.RecallAndFuse(ctx, rc); err != nil {
			b.Fatalf("recall: %v", err)
		}
	}
}

// BenchmarkRecallSlowPath 判据④：同语料慢车道（开关默认关 -> 全链路 8 信号融合）。
//
//	go test -bench BenchmarkRecallSlowPath -benchmem ./kernel/
func BenchmarkRecallSlowPath(b *testing.B) {
	srv, _ := benchLookupEnv(b) // 开关默认关
	ctx := context.Background()
	rc := RouterContext{Text: "客户15的偏好", TenantID: "t1", UserID: "u1"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := srv.RecallAndFuse(ctx, rc); err != nil {
			b.Fatalf("recall: %v", err)
		}
	}
}

// TestFastSlowTop1Consistency 判据③：≥30 条 L2 静态语料上快车道 top1 == 慢车道 top1，
// 一致率 ≥95%。
func TestFastSlowTop1Consistency(t *testing.T) {
	const n = 30
	srvFast, b := newSearchTestEnv(t)
	seedLookupCorpus(t, b, n, "t1", "u1", LayerL2)
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srvFast.SetLookupIndex(idx)
	srvFast.SetLookupEnabled(true)

	srvSlow := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)

	ctx := context.Background()
	consistent, fastHits := 0, 0
	var samples []string
	for i := 1; i <= n; i++ {
		q := corpusQuery(i)
		rc := RouterContext{Text: q, TenantID: "t1", UserID: "u1"}
		fast, err := srvFast.RecallAndFuse(ctx, rc)
		if err != nil {
			t.Fatalf("fast recall %s: %v", q, err)
		}
		slow, err := srvSlow.RecallAndFuse(ctx, rc)
		if err != nil {
			t.Fatalf("slow recall %s: %v", q, err)
		}
		fastTop, slowTop := "", ""
		if len(fast.Fused) > 0 {
			fastTop = fast.Fused[0].MemoryID
		}
		if len(slow.Fused) > 0 {
			slowTop = slow.Fused[0].MemoryID
		}
		if fast.Trace != nil && fast.Trace.FastPath {
			fastHits++
		}
		if fastTop != "" && fastTop == slowTop {
			consistent++
		} else {
			samples = append(samples, fmt.Sprintf("%s: fast=%q slow=%q", q, fastTop, slowTop))
		}
	}
	rate := float64(consistent) / float64(n)
	t.Logf("fast-slow top1 consistency: %d/%d = %.2f (fast hits %d/%d)", consistent, n, rate, fastHits, n)
	if fastHits != n {
		t.Fatalf("corpus should yield %d fast-path hits, got %d", n, fastHits)
	}
	if rate < 0.95 {
		t.Errorf("consistency rate %.2f < 0.95; mismatches: %v", rate, samples)
	}
}

// TestFallbackSamples 交付物要求：产出 5 条具名回退样本（查询文本 + 回退原因）。
// 逐条断言 fast_path=false 且 fallback_reason 符合预期，并把样本打到日志里，
// 便于交付报告直接引用原始输出。
//
// 红线呼应：回退样本全部是「明确回退」，不存在静默给错的快车道结果
// （任务书 §4.3「不许为了命中率而放宽歧义判定」）。
func TestFallbackSamples(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	// 语料：m01/m02 同实体同属性（歧义键），m03 唯一键。
	putLookupMemory(t, b, "m01", "t1", "u1", VisTenant, LayerL2, "客户01", "偏好",
		"客户01的偏好是事项一", 0.8, 0.5, 0.9)
	putLookupMemory(t, b, "m02", "t1", "u1", VisTenant, LayerL2, "客户01", "偏好",
		"客户01的偏好是事项二", 0.8, 0.5, 0.9)
	putLookupMemory(t, b, "m03", "t1", "u1", VisTenant, LayerL2, "客户03", "偏好",
		"客户03的偏好是事项三", 0.8, 0.5, 0.9)
	idx := NewLookupIndex()
	rebuildIndex(t, b, idx, "t1")
	srv.SetLookupEnabled(true) // 先不注入索引：样本 1 走 no_index

	asOf := "2026-10-01T00:00:00Z"
	cases := []struct {
		name   string
		text   string
		tenant string
		user   string
		asOf   *string
		reason string
	}{
		{name: "1-未注入索引", text: "客户01的偏好", tenant: "t1", user: "u1", reason: "no_index"},
		{name: "2-语料外实体", text: "项目B的预算", tenant: "t1", user: "u1", reason: "miss"},
		{name: "3-同键歧义", text: "客户01的偏好", tenant: "t1", user: "u1", reason: "ambiguous"},
		{name: "4-跨租户越权", text: "客户03的偏好", tenant: "t2", user: "u2", reason: "miss"},
		{name: "5-历史回溯as_of", text: "客户03的偏好", tenant: "t1", user: "u1", asOf: &asOf, reason: "miss"},
	}

	ctx := context.Background()
	for i, c := range cases {
		if i == 1 { // 样本 2 起注入索引
			srv.SetLookupIndex(idx)
		}
		res, err := srv.RecallAndFuse(ctx, RouterContext{
			Text: c.text, TenantID: c.tenant, UserID: c.user, AsOf: c.asOf,
		})
		if err != nil {
			t.Fatalf("sample %s: %v", c.name, err)
		}
		if res.Trace == nil {
			t.Fatalf("sample %s: trace nil", c.name)
		}
		if res.Trace.FastPath {
			t.Errorf("sample %s: must NOT take fast path", c.name)
		}
		if res.Trace.FallbackReason != c.reason {
			t.Errorf("sample %s: reason %q, want %q", c.name, res.Trace.FallbackReason, c.reason)
		}
		t.Logf("回退样本 %s | query=%q tenant=%s | fast_path=%v fallback_reason=%q",
			c.name, c.text, c.tenant, res.Trace.FastPath, res.Trace.FallbackReason)
	}
}

// ------------------------------------------------------------------
// Bench（判据④：快车道 ≤ 慢车道 1/5，0.88 上对比原始输出）
// ------------------------------------------------------------------

// benchLookupEnv 装配 30 条 L2 语料 + 已重建索引的 server。
func benchLookupEnv(b *testing.B) (*APIServer, StorageBackend) {
	b.Helper()
	backend, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		b.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, backend)
	seedLookupCorpus(b, backend, 30, "t1", "u1", LayerL2)
	idx := NewLookupIndex()
	rebuildIndex(b, backend, idx, "t1")
	srv.SetLookupIndex(idx)
	return srv, backend
}

// BenchmarkLookupFastPath 别名：与 BenchmarkRecallFastPath 同一实现，
// 保留命名以便既有脚本按 lookup 前缀检索。
func BenchmarkLookupFastPath(b *testing.B) { BenchmarkRecallFastPath(b) }

// BenchmarkLookupSlowPath 别名：与 BenchmarkRecallSlowPath 同一实现。
func BenchmarkLookupSlowPath(b *testing.B) { BenchmarkRecallSlowPath(b) }
