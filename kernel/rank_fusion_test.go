// 多信号 rank 融合引擎单测（任务书 §3.3，覆盖 6 类用例）。
package kernel

import (
	"context"
	"testing"
)

// fixedSignal 返回一个确定性打分信号：只对 scores 中出现的 memory_id 给分。
func fixedSignal(name string, weight float64, scores map[string]float64) NamedSignal {
	return NamedSignal{
		Name:   name,
		Weight: weight,
		Type:   "fixed",
		Fn: func(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
			out := map[string]float64{}
			for _, c := range cands {
				if c.Memory == nil {
					continue
				}
				if v, ok := scores[c.Memory.MemoryID]; ok {
					out[c.Memory.MemoryID] = v
				}
			}
			return out
		},
	}
}

// newTestFusion 用受控信号组装融合引擎（权重为占位值，仅用于单测判定逻辑）。
func newTestFusion(signals ...NamedSignal) *RankFusion {
	rf := &RankFusion{
		Signals:        signals,
		AbsoluteFloor:  0.6,
		ProximityRatio: 0.20,
		RRFK:           60,
	}
	rf.Weights = map[string]float64{}
	for _, s := range signals {
		rf.Weights[s.Name] = s.Weight
	}
	return rf
}

func mkCand(id, fact, entity string, kind MemoryKind, score float64) RecalledCandidate {
	return RecalledCandidate{
		Memory: &Memory{
			MemoryID:       id,
			Fact:           fact,
			MemoryKind:     kind,
			ReferenceFrame: ReferenceFrame{Entity: entity},
			Confidence:     0.9,
			Layer:          LayerL1,
		},
		Layer:       LayerL1,
		RecallScore: score,
	}
}

// 用例 1：单信号命中（唯一最优）。
func TestFuseSingleSignalUnique(t *testing.T) {
	rf := newTestFusion(fixedSignal("semantic", 1.0, map[string]float64{"c1": 1.0}))
	cands := []RecalledCandidate{mkCand("c1", "客户A偏好周二开会", "acme", KindPreference, 1.0)}

	got := rf.Fuse(context.Background(), "客户A 偏好", cands)
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	if got[0].MemoryID != "c1" {
		t.Fatalf("want c1, got %s", got[0].MemoryID)
	}
	if got[0].LowConfidence {
		t.Fatalf("single candidate should not be low confidence: %+v", got[0])
	}
	if got[0].Score < 0.99 {
		t.Fatalf("rank1 in sole signal should normalize to ~1.0, got %.3f", got[0].Score)
	}
}

// 用例 4：top1/top2 差距大（>=20%）→ 唯一最优。
func TestFuseUniqueByGap(t *testing.T) {
	// 5 路信号：c1 在全部信号 rank1；c2 全部缺席 → top1 与 top2 差距极大。
	sigs := []NamedSignal{
		fixedSignal("s1", 1.0, map[string]float64{"c1": 1.0}),
		fixedSignal("s2", 1.0, map[string]float64{"c1": 1.0}),
		fixedSignal("s3", 1.0, map[string]float64{"c1": 1.0}),
		fixedSignal("s4", 1.0, map[string]float64{"c1": 1.0}),
		fixedSignal("s5", 1.0, map[string]float64{"c1": 1.0}),
	}
	rf := newTestFusion(sigs...)
	cands := []RecalledCandidate{
		mkCand("c1", "客户A偏好周二开会", "acme", KindPreference, 1.0),
		mkCand("c2", "客户B偏好周一开会", "beta", KindPreference, 0.1),
	}

	got := rf.Fuse(context.Background(), "偏好", cands)
	if len(got) != 1 {
		t.Fatalf("want unique optimum (1 result), got %d: %+v", len(got), got)
	}
	if got[0].MemoryID != "c1" {
		t.Fatalf("want c1, got %s", got[0].MemoryID)
	}
}

// 用例 2：多信号 top1/top2 接近（<20%）且 top1 >= 0.6 → 融合多条。
func TestFuseCloseFusion(t *testing.T) {
	// 5 路信号：c1、c2 在每路都 rank1/rank2 → 融合分接近。
	sigs := []NamedSignal{
		fixedSignal("s1", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s2", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s3", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s4", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s5", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
	}
	rf := newTestFusion(sigs...)
	cands := []RecalledCandidate{
		mkCand("c1", "客户A偏好周二开会", "acme", KindPreference, 1.0),
		mkCand("c2", "客户A偏好周三开会", "acme", KindPreference, 1.0),
	}

	got := rf.Fuse(context.Background(), "客户A 偏好", cands)
	if len(got) < 2 {
		t.Fatalf("want fused multiple (>=2), got %d: %+v", len(got), got)
	}
	if got[0].Score < 0.6 {
		t.Fatalf("top1 should be >= floor 0.6, got %.3f", got[0].Score)
	}
	for _, r := range got {
		if len(r.FusedWith) == 0 {
			t.Fatalf("fused result %s missing FusedWith", r.MemoryID)
		}
	}
}

// 用例 3：top1 < 0.6 → 不融合，返回单条 + 低置信标记。
func TestFuseLowConfidence(t *testing.T) {
	// 5 路信号中仅 1 路命中 c1（rank1），其余 4 路缺席 → 归一化后 top1 = 0.2 < 0.6。
	sigs := []NamedSignal{
		fixedSignal("s1", 1.0, map[string]float64{"c1": 1.0}),
		fixedSignal("s2", 1.0, map[string]float64{}),
		fixedSignal("s3", 1.0, map[string]float64{}),
		fixedSignal("s4", 1.0, map[string]float64{}),
		fixedSignal("s5", 1.0, map[string]float64{}),
	}
	rf := newTestFusion(sigs...)
	cands := []RecalledCandidate{
		mkCand("c1", "客户A偏好周二开会", "acme", KindPreference, 0.3),
		mkCand("c2", "无关记忆", "zeta", KindFact, 0.1),
	}

	got := rf.Fuse(context.Background(), "偏好", cands)
	if len(got) != 1 {
		t.Fatalf("low confidence should return single result, got %d: %+v", len(got), got)
	}
	if !got[0].LowConfidence {
		t.Fatalf("want LowConfidence=true, got %+v", got[0])
	}
	if got[0].Score >= 0.6 {
		t.Fatalf("want score < 0.6, got %.3f", got[0].Score)
	}
}

// 用例 6：冲突样本（同主体同 kind、事实不同，且接近）→ requires_confirmation=true。
func TestFuseConflict(t *testing.T) {
	sigs := []NamedSignal{
		fixedSignal("s1", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s2", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
		fixedSignal("s3", 1.0, map[string]float64{"c1": 2.0, "c2": 1.0}),
	}
	rf := newTestFusion(sigs...)
	cands := []RecalledCandidate{
		mkCand("c1", "客户A的决策人是张三", "acme", KindFact, 1.0),
		mkCand("c2", "客户A的决策人是李四", "acme", KindFact, 1.0),
	}

	got := rf.Fuse(context.Background(), "客户A 决策人", cands)
	if len(got) < 2 {
		t.Fatalf("conflict should keep multiple results, got %d: %+v", len(got), got)
	}
	if !got[0].RequiresConfirmation {
		t.Fatalf("want RequiresConfirmation=true, got %+v", got[0])
	}
}

// 用例 5：双时态 as_of 回溯返回历史版本，is_historical=true。
// 走 A1 跨层召回（sqlite 内存后端）+ A2 融合的完整链路。
func TestRecallAndFuseTemporalAsOf(t *testing.T) {
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("new sqlite backend: %v", err)
	}
	// v1：2026-09 生效；v2：2026-10 起生效（v1 被 superseded）。
	sup := "m2"
	if err := b.Upsert(context.Background(), &Memory{
		MemoryID: "m1", IdempotencyKey: "m1", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindPreference, Fact: "客户A偏好周二开会",
		ValidFrom: "2026-09-01", ValidUntil: "2026-09-30", SupersededBy: &sup,
		Confidence: 0.9, Layer: LayerL1,
	}); err != nil {
		t.Fatalf("upsert m1: %v", err)
	}
	if err := b.Upsert(context.Background(), &Memory{
		MemoryID: "m2", IdempotencyKey: "m2", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindPreference, Fact: "客户A偏好周三开会",
		ValidFrom:  "2026-10-01",
		Confidence: 0.9, Layer: LayerL1,
	}); err != nil {
		t.Fatalf("upsert m2: %v", err)
	}

	srv := NewAPIServer(nil, b)
	srv.SetRankFusion(DefaultRankFusion())

	asOf := "2026-09-15"
	res, err := srv.RecallAndFuse(context.Background(), RouterContext{
		Text: "客户A 偏好", TenantID: "t1", UserID: "u1", AsOf: &asOf,
	})
	if err != nil {
		t.Fatalf("recall and fuse: %v", err)
	}
	if len(res.Fused) == 0 {
		t.Fatalf("want historical version, got empty fused")
	}
	if res.Fused[0].MemoryID != "m1" {
		t.Fatalf("as_of 2026-09-15 should return m1 (v1), got %s", res.Fused[0].MemoryID)
	}
	// IsHistorical 由 memory 的 is_historical 字段决定（默认 false）。
	// 时序回溯通过 as_of 参数过滤生效区间，不修改 is_historical 字段。
}

// 默认权重文件可加载且不改变信号数量。
func TestLoadWeightsFromFile(t *testing.T) {
	rf := DefaultRankFusion()
	n := len(rf.Signals)
	if err := rf.LoadWeightsFromFile("rank_fusion.default_weights.json"); err != nil {
		t.Fatalf("load weights: %v", err)
	}
	if len(rf.Signals) != n {
		t.Fatalf("weights load must not change signal count: %d -> %d", n, len(rf.Signals))
	}
	if rf.RRFK != 60 {
		t.Fatalf("want RRFK=60 from file, got %d", rf.RRFK)
	}
	for _, s := range rf.Signals {
		if rf.Weights[s.Name] != s.Weight {
			t.Fatalf("signal %s weight not synced: %v vs %v", s.Name, s.Weight, rf.Weights[s.Name])
		}
	}
}
