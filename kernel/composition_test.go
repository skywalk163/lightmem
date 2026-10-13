// 组合记忆（R6-B）单测：Compose / Decompose / 参考系继承 / 嵌套展开 / 检索展开集成。
// 全部确定性；不依赖排序随机源（seed 无关）。
package kernel

import (
	"context"
	"testing"
	"time"
)

// newTestServerForComposition 构造仅用于组合测试的 APIServer（SQLite 内存后端 + 固定 Now）。
func newTestServerForComposition(t *testing.T) (*APIServer, StorageBackend) {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	srv.SetRankFusion(rf)
	return srv, b
}

// 用例 1：Compose 设置 Components 并写入；子记忆参考系从父继承。
func TestComposition_Compose(t *testing.T) {
	ctx := context.Background()
	_, b := newTestServerForComposition(t)
	parent := &Memory{
		MemoryID: "p1", Fact: "会议X的决议", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1,
		ReferenceFrame: ReferenceFrame{Tenant: "t1", User: "u1", Project: "projX"},
	}
	c1 := &Memory{MemoryID: "c1", Fact: "周二开项目周会", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1, ReferenceFrame: ReferenceFrame{Entity: "决议A"}}
	c2 := &Memory{MemoryID: "c2", Fact: "张三负责周会汇报", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1}
	if err := Compose(ctx, b, parent, []*Memory{c1, c2}); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// 父记忆 Components 应为 [c1, c2]（去重保序）。
	if len(parent.Components) != 2 || parent.Components[0] != "c1" || parent.Components[1] != "c2" {
		t.Fatalf("parent.Components = %v, want [c1 c2]", parent.Components)
	}
	// 子记忆参考系继承父的 Project（自身 Entity 保留/覆盖）。
	gotC1, err := b.Get(ctx, "c1", "u1", "t1")
	if err != nil {
		t.Fatalf("get c1: %v", err)
	}
	if gotC1.ReferenceFrame.Project != "projX" {
		t.Fatalf("c1 should inherit Project=projX, got %q", gotC1.ReferenceFrame.Project)
	}
	if gotC1.ReferenceFrame.Entity != "决议A" {
		t.Fatalf("c1 Entity should keep its own value 决议A, got %q", gotC1.ReferenceFrame.Entity)
	}
	gotC2, _ := b.Get(ctx, "c2", "u1", "t1")
	if gotC2.ReferenceFrame.Project != "projX" || gotC2.ReferenceFrame.Tenant != "t1" {
		t.Fatalf("c2 should inherit Tenant/Project from parent, got %+v", gotC2.ReferenceFrame)
	}
}

// 用例 2：参考系继承 + 覆盖规则（父空字段由子覆盖；子已设字段保留）。
func TestComposition_ReferenceFrameInheritance(t *testing.T) {
	ctx := context.Background()
	_, b := newTestServerForComposition(t)
	parent := &Memory{
		MemoryID: "p", Fact: "pf", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1,
		ReferenceFrame: ReferenceFrame{Tenant: "t1", User: "u1", Project: "projX", Scene: "会议"},
	}
	// 子记忆自带 Project（覆盖父）与 Entity（父为空，继承父的 Tenant/Scene）。
	child := &Memory{
		MemoryID: "ch", Fact: "cf", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1,
		ReferenceFrame: ReferenceFrame{Project: "projY", Entity: "e1"},
	}
	if err := Compose(ctx, b, parent, []*Memory{child}); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	got, err := b.Get(ctx, "ch", "u1", "t1")
	if err != nil {
		t.Fatalf("get ch: %v", err)
	}
	if got.ReferenceFrame.Project != "projY" {
		t.Fatalf("child Project should override parent (projY), got %q", got.ReferenceFrame.Project)
	}
	if got.ReferenceFrame.Tenant != "t1" || got.ReferenceFrame.Scene != "会议" {
		t.Fatalf("child should inherit Tenant/Scene from parent, got %+v", got.ReferenceFrame)
	}
	if got.ReferenceFrame.Entity != "e1" {
		t.Fatalf("child Entity should keep e1, got %q", got.ReferenceFrame.Entity)
	}
}

// 用例 3：Decompose 返回子记忆并清空父 Components。
func TestComposition_Decompose(t *testing.T) {
	ctx := context.Background()
	_, b := newTestServerForComposition(t)
	parent := &Memory{MemoryID: "p", Fact: "pf", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	c1 := &Memory{MemoryID: "c1", Fact: "cf1", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	c2 := &Memory{MemoryID: "c2", Fact: "cf2", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	if err := Compose(ctx, b, parent, []*Memory{c1, c2}); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	children, err := Decompose(ctx, b, parent)
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("Decompose returned %d children, want 2", len(children))
	}
	if IsComposite(parent) {
		t.Fatalf("parent should no longer be composite after Decompose")
	}
	got, _ := b.Get(ctx, "p", "u1", "t1")
	if IsComposite(got) {
		t.Fatalf("parent.Components should be cleared in backend")
	}
}

// 用例 4：嵌套组合展开（父→子→孙），深度受限、确定性、防环。
func TestComposition_ExpandCompositionNested(t *testing.T) {
	ctx := context.Background()
	_, b := newTestServerForComposition(t)
	p := &Memory{MemoryID: "p", Fact: "pf", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	c1 := &Memory{MemoryID: "c1", Fact: "cf1", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	c2 := &Memory{MemoryID: "c2", Fact: "cf2", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	// c1 自身是组合：包含孙 g1。
	g1 := &Memory{MemoryID: "g1", Fact: "gf1", TenantID: "t1", UserID: "u1", Visibility: VisTenant, Layer: LayerL1}
	if err := Compose(ctx, b, c1, []*Memory{g1}); err != nil {
		t.Fatalf("Compose c1: %v", err)
	}
	if err := Compose(ctx, b, p, []*Memory{c1, c2}); err != nil {
		t.Fatalf("Compose p: %v", err)
	}
	// 深度 3：展开 p → c1,c2 + g1（c1 的子）。
	expanded, err := ExpandComposition(ctx, b, p, 3)
	if err != nil {
		t.Fatalf("ExpandComposition: %v", err)
	}
	ids := []string{}
	for _, m := range expanded {
		ids = append(ids, m.MemoryID)
	}
	if len(ids) != 3 {
		t.Fatalf("expanded ids = %v, want 3 (c1,c2,g1)", ids)
	}
	// 确定性顺序：p 的直接子 [c1,c2] 深度优先，c1 的子 g1 在 c1 之后、c2 之前。
	want := []string{"c1", "g1", "c2"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("expand order = %v, want %v", ids, want)
		}
	}
	// 防环：让 c2 反向引用 p，再展开不应死循环且不应重复。
	c2.Components = []string{"p"}
	if err := b.Upsert(ctx, c2); err != nil {
		t.Fatalf("upsert c2 loop: %v", err)
	}
	expanded2, err := ExpandComposition(ctx, b, p, 3)
	if err != nil {
		t.Fatalf("ExpandComposition loop: %v", err)
	}
	seen := map[string]int{}
	for _, m := range expanded2 {
		seen[m.MemoryID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("memory %s expanded %d times (cycle not guarded)", id, n)
		}
	}
}

// 用例 5：检索路径展开集成——RecallAndFuse 携带 ComposeExpand 时，组合记忆候选
// 的子事实被插入其之后；关闭时（isolate）不展开（逐位等价）。
func TestComposition_RecallAndFuseExpand(t *testing.T) {
	ctx := context.Background()
	srv, b := newTestServerForComposition(t)
	// 父事实与查询完全相等 → 父必被召回（recallScore=1.0）；子事实与查询零词元重合 → 不被独立召回。
	parent := &Memory{
		MemoryID: "p", Fact: "会议X的决议", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1,
		ReferenceFrame: ReferenceFrame{Tenant: "t1", User: "u1", Project: "projX"},
	}
	// 子事实刻意避开查询「会议X的决议」中的汉字（会/议/的/决/X），
	// 保证 recallScore 词元门控为 0、不被独立召回；仅经组合展开后插入。
	c1 := &Memory{MemoryID: "c1", Fact: "周一工程同步碰头", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1}
	c2 := &Memory{MemoryID: "c2", Fact: "张三负责纪要梳理", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, Layer: LayerL1}
	if err := Compose(ctx, b, parent, []*Memory{c1, c2}); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// 构建 tfidf 词表（语义信号需要 embedder；不影响召回门控，仅避免 nil）。
	vocab := NewCharNgramVocab(3)
	vocab.BuildIDF([]string{parent.Fact, c1.Fact, c2.Fact, "会议X的决议"})
	srv.fusion.SetEmbedder(vocab.EmbedFn())

	query := "会议X的决议" // 与父事实相等；预期命中 = c1 的事实（经展开）

	// 关闭展开：仅父事实在候选中，不应命中 c1。
	offRes, err := srv.RecallAndFuse(ctx, RouterContext{Text: query, TenantID: "t1", UserID: "u1", TopK: 5})
	if err != nil {
		t.Fatalf("RecallAndFuse off: %v", err)
	}
	for _, f := range offRes.Fused {
		if f.Source == "composition_expand" {
			t.Fatalf("expansion must not occur when ComposeExpand=false")
		}
	}
	offHit := containsFact(offRes.Fused, c1.Fact)
	if offHit {
		t.Fatalf("c1 fact must NOT appear without expansion (isolate 复现)")
	}

	// 开启展开：父候选之后应插入 c1/c2 子事实；c1 事实应命中。
	onRes, err := srv.RecallAndFuse(ctx, RouterContext{Text: query, TenantID: "t1", UserID: "u1", TopK: 5, ComposeExpand: true})
	if err != nil {
		t.Fatalf("RecallAndFuse on: %v", err)
	}
	var expanded int
	for _, f := range onRes.Fused {
		if f.Source == "composition_expand" {
			expanded++
		}
	}
	if expanded != 2 {
		t.Fatalf("expected 2 expanded children, got %d", expanded)
	}
	if !containsFact(onRes.Fused, c1.Fact) {
		t.Fatalf("c1 fact must appear after expansion")
	}
	// 父候选后紧邻子事实（展开插入顺序）。
	if len(onRes.Fused) < 2 || onRes.Fused[0].MemoryID != "p" {
		t.Fatalf("parent should be first in fused, got %v", fusedIDs(onRes.Fused))
	}
}

// containsFact 判断融合结果是否含某 fact。
func containsFact(fused []FusedResult, fact string) bool {
	for _, f := range fused {
		if f.Fact == fact {
			return true
		}
	}
	return false
}

func fusedIDs(fused []FusedResult) []string {
	ids := make([]string, 0, len(fused))
	for _, f := range fused {
		ids = append(ids, f.MemoryID)
	}
	return ids
}
