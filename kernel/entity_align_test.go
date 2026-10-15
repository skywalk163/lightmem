// kernel/entity_align_test.go — R13-A 跨 Agent 参考系对齐前置单测（Monty 建议 5）。
//
// 覆盖任务书 A.4 的「未对齐跨 Agent 融合被拒绝」与配套可判定语义：
//  1. 未对齐（entity 不同）→ 分属不同组，绝不进同一融合组；
//  2. entity 相同但 scene/task 语境冲突 → 未对齐，只并列展示；
//  3. entity/scene/task 缺失 → AlignUnknown（保守拒绝融合）；
//  4. 同参考系跨 Agent → 同组且标 CrossAgent；
//  5. 确定性：输入顺序不影响分组结论；重复 / 空输入不 panic；
//  6. 默认路径不变：本函数不触碰 RankFusion 与 voting.go 的既有行为。
//
// 红线遵守：不调 LLM；纯字符串比对，无随机源；不修改既有 Schema 与默认融合路径。
package kernel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// alignMem 构造一条带参考系与 Agent 的记忆（确定性）。
func alignMem(id, agent, entity, scene, task string, vis Visibility) *Memory {
	return &Memory{
		MemoryID:   id,
		Fact:       "fact-" + id,
		TenantID:   "t1",
		UserID:     "u1",
		AgentID:    agent,
		Visibility: vis,
		Layer:      LayerL2,
		RecordedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		ReferenceFrame: ReferenceFrame{
			Tenant: "t1", User: "u1", Agent: agent,
			Entity: entity, Scene: scene, Task: task,
		},
	}
}

// groupKeyOf 返回某记忆所属的对齐组键（不在任何组时返回 ""）。
func groupKeyOf(rep AlignReport, id string) string {
	for _, g := range rep.Groups {
		for _, m := range g.Members {
			if m == id {
				return g.FrameKey
			}
		}
	}
	return ""
}

// TestEntityAlignCrossAgentFusionDifferentEntityRejected
// 未对齐（entity 不同）→ 禁止融合：对方记忆进 Unaligned，且不与本体同组。
func TestEntityAlignCrossAgentFusionDifferentEntityRejected(t *testing.T) {
	ms := []*Memory{
		alignMem("m1", "agent-a", "客户A", "会议", "报价", VisSharedUser),
		alignMem("m2", "agent-b", "客户B", "会议", "报价", VisSharedUser),
	}
	// 接收方参考系 = 客户A。客户B 的记忆因此未对齐。
	ref := &ReferenceFrame{Tenant: "t1", User: "u1", Entity: "客户A", Scene: "会议", Task: "报价"}
	rep := AlignCrossAgentFusion(ms, ref, DefaultAlignPolicy())
	if rep.Mode != "reference" {
		t.Fatalf("want reference mode, got %s", rep.Mode)
	}
	if rep.Aligned() {
		t.Fatalf("different entity must produce unaligned entries, got %+v", rep)
	}
	if len(rep.Unaligned) != 1 || rep.Unaligned[0].MemoryID != "m2" {
		t.Fatalf("want m2 unaligned, got %+v", rep.Unaligned)
	}
	if rep.Unaligned[0].Status != AlignUnaligned {
		t.Fatalf("m2 want AlignUnaligned, got %s", rep.Unaligned[0].Status)
	}
	if len(rep.Groups) != 1 || len(rep.Groups[0].Members) != 1 || rep.Groups[0].Members[0] != "m1" {
		t.Fatalf("only m1 may be fusable, got %+v", rep.Groups)
	}
	// 自分组模式：不同实体天然分属不同键，绝不合并。
	rep2 := AlignCrossAgentFusion(ms, nil, DefaultAlignPolicy())
	if len(rep2.Groups) != 2 {
		t.Fatalf("group mode: want 2 distinct groups, got %+v", rep2.Groups)
	}
	if groupKeyOf(rep2, "m1") == groupKeyOf(rep2, "m2") {
		t.Fatalf("m1/m2 must not share a group: %+v", rep2.Groups)
	}
}

// TestEntityAlignCrossAgentFusionSceneConflictRejected
// entity 相同但 scene 语境冲突 → 未对齐，只并列展示（Monty：同一特征在不同参考系指代不同）。
func TestEntityAlignCrossAgentFusionSceneConflictRejected(t *testing.T) {
	ms := []*Memory{
		alignMem("m1", "agent-a", "客户A", "会议", "报价", VisSharedUser),
		alignMem("m2", "agent-b", "客户A", "工单", "报价", VisSharedUser),
	}
	// 参考系 = 会议场景 → 工单场景的记忆未对齐。
	ref := &ReferenceFrame{Entity: "客户A", Scene: "会议", Task: "报价"}
	rep := AlignCrossAgentFusion(ms, ref, DefaultAlignPolicy())
	if len(rep.Unaligned) != 1 || rep.Unaligned[0].MemoryID != "m2" {
		t.Fatalf("scene conflict must mark m2 unaligned, got %+v", rep.Unaligned)
	}
	if !strings.Contains(rep.Unaligned[0].Reason, "scene") {
		t.Fatalf("reason must name the conflicting dim, got %q", rep.Unaligned[0].Reason)
	}
	// 放宽 scene 要求后（且 ref 也不要求 scene）二者可对齐同组。
	rep2 := AlignCrossAgentFusion(ms, &ReferenceFrame{Entity: "客户A"}, EntityAlignPolicy{})
	if len(rep2.Groups) != 1 || len(rep2.Groups[0].Members) != 2 {
		t.Fatalf("with RequireScene=false want 1 group of 2, got %+v", rep2.Groups)
	}
}

// TestEntityAlignCrossAgentFusionMissingCoordUnknown
// 关键坐标缺失 → AlignUnknown，保守拒绝融合（不静默放行）。
func TestEntityAlignCrossAgentFusionMissingCoordUnknown(t *testing.T) {
	ms := []*Memory{
		alignMem("m1", "agent-a", "", "会议", "报价", VisSharedUser),  // entity 缺失
		alignMem("m2", "agent-b", "客户A", "", "报价", VisSharedUser), // scene 缺失
		alignMem("m3", "agent-c", "客户A", "会议", "", VisSharedUser), // task 缺失
	}
	rep := AlignCrossAgentFusion(ms, nil, DefaultAlignPolicy())
	if len(rep.Groups) != 0 {
		t.Fatalf("missing coords must not form groups, got %+v", rep.Groups)
	}
	if len(rep.Unaligned) != 3 {
		t.Fatalf("want 3 unaligned, got %d", len(rep.Unaligned))
	}
	for _, u := range rep.Unaligned {
		if u.Status != AlignUnknown {
			t.Fatalf("%s want AlignUnknown, got %s", u.MemoryID, u.Status)
		}
		if u.Reason == "" {
			t.Fatalf("%s missing reason", u.MemoryID)
		}
	}
}

// TestEntityAlignCrossAgentFusionCrossAgentGroupFlagged
// 同参考系的跨 Agent 记忆可同组，但必须显式标 CrossAgent（供上层追加确认）。
func TestEntityAlignCrossAgentFusionCrossAgentGroupFlagged(t *testing.T) {
	ms := []*Memory{
		alignMem("m1", "agent-a", "客户A", "会议", "报价", VisSharedUser),
		alignMem("m2", "agent-b", "客户A", "会议", "报价", VisSharedUser),
	}
	rep := AlignCrossAgentFusion(ms, nil, DefaultAlignPolicy())
	if len(rep.Groups) != 1 {
		t.Fatalf("want 1 aligned group, got %+v", rep.Groups)
	}
	g := rep.Groups[0]
	if !g.CrossAgent {
		t.Fatalf("group with 2 agents must be flagged CrossAgent: %+v", g)
	}
	if len(g.Agents) != 2 {
		t.Fatalf("want 2 agents, got %v", g.Agents)
	}
	if len(g.Members) != 2 {
		t.Fatalf("want 2 members, got %v", g.Members)
	}
	// 单 Agent 组不标 CrossAgent。
	rep2 := AlignCrossAgentFusion(ms[:1], nil, DefaultAlignPolicy())
	if len(rep2.Groups) != 1 || rep2.Groups[0].CrossAgent {
		t.Fatalf("single-agent group must not be CrossAgent: %+v", rep2.Groups)
	}
}

// TestEntityAlignCrossAgentFusionNormalizationAndDeterminism
// 归一化（大小写/标点/空白）后相同视为同参考系；输入顺序不影响结论。
func TestEntityAlignCrossAgentFusionNormalizationAndDeterminism(t *testing.T) {
	a := alignMem("m1", "agent-a", "客户A", "会议", "报价", VisSharedUser)
	b := alignMem("m2", "agent-b", " 客户a ", "会  议", "报价", VisSharedUser)
	rep := AlignCrossAgentFusion([]*Memory{a, b}, nil, DefaultAlignPolicy())
	if len(rep.Groups) != 1 {
		t.Fatalf("normalized-equal frames should merge, got %+v", rep.Groups)
	}
	repRev := AlignCrossAgentFusion([]*Memory{b, a}, nil, DefaultAlignPolicy())
	if len(repRev.Groups) != len(rep.Groups) {
		t.Fatalf("order-dependent groups: %d vs %d", len(rep.Groups), len(repRev.Groups))
	}
	if repRev.Groups[0].FrameKey != rep.Groups[0].FrameKey {
		t.Fatalf("order-dependent frame key: %s vs %s", rep.Groups[0].FrameKey, repRev.Groups[0].FrameKey)
	}
	// 空 / nil 输入不 panic。
	if got := AlignCrossAgentFusion(nil, nil, DefaultAlignPolicy()); len(got.Groups) != 0 || len(got.Unaligned) != 0 {
		t.Fatalf("nil input must return empty report, got %+v", got)
	}
	if got := AlignCrossAgentFusion([]*Memory{nil}, nil, DefaultAlignPolicy()); len(got.Groups) != 0 || len(got.Unaligned) != 0 {
		t.Fatalf("nil memory must be skipped, got %+v", got)
	}
}

// TestEntityAlignCrossAgentFusionDoesNotTouchDefaultFusion
// 默认路径不变：调用对齐前置前后，RankFusion.Rank（RRF）输出逐位一致；
// 且本函数不改动入参 Memory 的任何字段。
func TestEntityAlignCrossAgentFusionDoesNotTouchDefaultFusion(t *testing.T) {
	ctx := context.Background()
	m1 := alignMem("m1", "agent-a", "客户A", "会议", "报价", VisSharedUser)
	m2 := alignMem("m2", "agent-b", "客户B", "会议", "报价", VisSharedUser)
	cands := []RecalledCandidate{
		{Memory: m1, Layer: LayerL2},
		{Memory: m2, Layer: LayerL2},
	}
	rf := DefaultRankFusion()
	rf.Now = montyFixedNow()
	before := rf.Rank(ctx, "客户A 报价", cands)

	_ = AlignCrossAgentFusion([]*Memory{m1, m2}, nil, DefaultAlignPolicy())

	after := rf.Rank(ctx, "客户A 报价", cands)
	if len(before) != len(after) {
		t.Fatalf("default path length changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].MemoryID != after[i].MemoryID || before[i].Score != after[i].Score {
			t.Fatalf("default path changed at %d", i)
		}
	}
	if m1.Visibility != VisSharedUser || m1.ReferenceFrame.Entity != "客户A" {
		t.Fatalf("align must not mutate input memory: %+v", m1)
	}
}
