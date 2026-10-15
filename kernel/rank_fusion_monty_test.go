// kernel/rank_fusion_monty_test.go — R13-A 只加不减融合算子单测（Monty 建议 2）。
//
// 覆盖任务书 A.4 四个可判定语义：
//  1. 只加不减：弱信号缺失**记 0 不降分**（等价于显式 0，绝不记负分）；
//  2. 相对容差：diff > tolerance（默认 0.2）记 0；≤ 容差内线性得分；
//  3. 绝对下限：top1 < 0.6 禁止融合（返回单条 + LowConfidence）；
//  4. 默认路径不变：FuseMonotonic 不修改 RankFusion 状态，RRF 输出逐位不变。
//
// 红线遵守：不调 LLM；确定性（固定候选 ID / 固定信号函数）；不修改既有 Schema
// 与默认融合路径。
package kernel

import (
	"context"
	"math"
	"testing"
	"time"
)

// montyFixedNow 固定打分基准时刻（确定性，不引入 time.Now）。
func montyFixedNow() time.Time { return time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC) }

// montyCand 构造一个仅含 ID / Fact 的候选（确定性，无时间源）。
func montyCand(id, fact string) RecalledCandidate {
	return RecalledCandidate{
		Memory: &Memory{
			MemoryID: id,
			Fact:     fact,
			Layer:    LayerL1,
			TenantID: "t1",
			UserID:   "u1",
		},
		Layer: LayerL1,
	}
}

// newMontyRF 构造仅含指定信号的 RankFusion（不用 DefaultRankFusion，避免依赖 embedder）。
func newMontyRF(sigs ...NamedSignal) *RankFusion {
	rf := &RankFusion{Weights: map[string]float64{}}
	for _, s := range sigs {
		rf.Signals = append(rf.Signals, s)
		rf.Weights[s.Name] = s.Weight
	}
	return rf
}

// montyFixedSignal 构造返回固定打分的信号函数（缺省未列出的候选 → 0，即「信号缺失」）。
// 前缀 monty 避免与 rank_fusion_test.go 的既有 fixedSignal 助手重名。
func montyFixedSignal(name string, w float64, scores map[string]float64) NamedSignal {
	return NamedSignal{
		Name: name, Weight: w, Type: "fixed",
		Fn: func(_ context.Context, _ string, _ []RecalledCandidate) map[string]float64 {
			out := map[string]float64{}
			for k, v := range scores {
				out[k] = v
			}
			return out
		},
	}
}

// TestMontyEvidenceScoreClipSemantics 证据分公式：clip(tol-diff,0,inf)/tol。
func TestMontyEvidenceScoreClipSemantics(t *testing.T) {
	const tol = 0.2
	cases := []struct {
		diff float64
		want float64
	}{
		{-1.0, 1.0}, // 完全匹配（diff ≤ 0）→ 取最大证据 1，不溢出
		{0, 1.0},    // 无差 → 1
		{0.1, 0.5},  // 容差内线性：(0.2-0.1)/0.2
		{tol, 0},    // 恰在容差边界 → 0（不匹配记 0）
		{0.21, 0},   // 超容差 → 0，**不记负分**
		{10, 0},     // 远离 → 0
	}
	for _, c := range cases {
		got := MontyEvidenceScore(c.diff, tol)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("MontyEvidenceScore(diff=%.3f, tol=%.2f)=%.6f, want %.6f", c.diff, tol, got, c.want)
		}
		if got < 0 {
			t.Fatalf("evidence must be >= 0 (只加不减), got %.6f", got)
		}
	}
	// 容差非正：不做除零，视为无有效证据。
	if got := MontyEvidenceScore(0, 0); got != 0 {
		t.Fatalf("tolerance<=0 must yield 0, got %.6f", got)
	}
	if got := MontyEvidenceScore(-1, -5); got != 0 {
		t.Fatalf("negative tolerance must yield 0, got %.6f", got)
	}
}

// TestMontyFuseMonotonicMissingWeakSignalNotPenalized
// 只加不减：弱信号缺失**记 0 不降分**——缺失与「显式 0」等价，且不记负分。
func TestMontyFuseMonotonicMissingWeakSignalNotPenalized(t *testing.T) {
	ctx := context.Background()
	cands := []RecalledCandidate{
		montyCand("m1", "候选一：强信号最优、弱信号缺失"),
		montyCand("m2", "候选二：强信号最优、弱信号缺失"),
		montyCand("m3", "候选三：强信号次优、弱信号超容差"),
	}
	// strong：m1/m2 并列最优（归一化 1.0），m3 为 0.5（diff=0.5 > tol → 0 证据）。
	// weak：三者皆缺失（模拟「弱信号没有命中任何候选」）。
	rf := newMontyRF(
		montyFixedSignal("strong", 1.0, map[string]float64{"m1": 1.0, "m2": 1.0, "m3": 0.5}),
		montyFixedSignal("weak", 0.3, map[string]float64{}),
	)
	got := rf.FuseMonotonic(ctx, "q", cands, MonotonicConfig{})

	if got.Tolerance != DefaultMontyTolerance {
		t.Fatalf("default tolerance want %.2f, got %.2f", DefaultMontyTolerance, got.Tolerance)
	}
	if got.AbsoluteFloor != DefaultMontyAbsoluteFloor {
		t.Fatalf("default floor want %.2f, got %.2f", DefaultMontyAbsoluteFloor, got.AbsoluteFloor)
	}
	// m1/m2：strong 证据 1.0、weak 证据 0 → (1.0*1.0 + 0.3*0)/1.3 ≈ 0.769（不记负分）。
	const want = 1.0 / 1.3
	byID := map[string]MontyVerdict{}
	for _, v := range got.Ranked {
		byID[v.MemoryID] = v
	}
	for _, id := range []string{"m1", "m2"} {
		v, ok := byID[id]
		if !ok {
			t.Fatalf("%s missing from result %v", id, got.Ranked)
		}
		if math.Abs(v.Score-want) > 1e-9 {
			t.Fatalf("%s score=%.6f want %.6f (弱信号缺失应记 0 且不稀释、不记负分)", id, v.Score, want)
		}
	}
	for _, v := range got.Ranked {
		if v.Score < 0 {
			t.Fatalf("%s score negative (%.6f) — 违反只加不减", v.MemoryID, v.Score)
		}
		for _, e := range v.Evidences {
			if e.Evidence < 0 {
				t.Fatalf("%s signal %s evidence negative (%.6f)", v.MemoryID, e.Signal, e.Evidence)
			}
		}
	}
	// m3 的 strong 证据为 0（diff 超容差），weak 缺失也记 0 → 总分 0（不为负）。
	if v, ok := byID["m3"]; ok && v.Score != 0 {
		t.Fatalf("m3 want 0（全部超容差/缺失），got %.6f", v.Score)
	}
}

// TestMontyFuseMonotonicRelativeToleranceZeroesBeyond
// 相对容差：diff > tolerance → 该信号记 0；容差内 → 线性得分。
func TestMontyFuseMonotonicRelativeToleranceZeroesBeyond(t *testing.T) {
	ctx := context.Background()
	// 单一信号：m1=1.0（最优，归一化 1.0），m2=0.9（diff=0.1 → 证据 0.5），m3=0.7（diff=0.3 → 0）。
	cands := []RecalledCandidate{
		montyCand("m1", "最优"),
		montyCand("m2", "容差内"),
		montyCand("m3", "超容差"),
	}
	rf := newMontyRF(montyFixedSignal("s1", 1.0, map[string]float64{"m1": 1.0, "m2": 0.9, "m3": 0.7}))
	got := rf.FuseMonotonic(ctx, "q", cands, MonotonicConfig{Tolerance: 0.2})

	// 注意：top1 唯一最优时 Ranked 只含 1 条，故从 All（全量逐条证据分）读取。
	scores := map[string]float64{}
	for _, v := range got.All {
		scores[v.MemoryID] = v.Score
	}
	if math.Abs(scores["m1"]-1.0) > 1e-9 {
		t.Fatalf("m1 want 1.0, got %.6f", scores["m1"])
	}
	if math.Abs(scores["m2"]-0.5) > 1e-9 {
		t.Fatalf("m2 want 0.5 (容差内线性), got %.6f", scores["m2"])
	}
	if scores["m3"] != 0 {
		t.Fatalf("m3 want 0 (diff=0.3 > tol=0.2 记 0 不扣分), got %.6f", scores["m3"])
	}

	// 容差扩大到 0.5：m3 的 diff=0.3 进入容差内 → 证据 (0.5-0.3)/0.5 = 0.4。
	got2 := rf.FuseMonotonic(ctx, "q", cands, MonotonicConfig{Tolerance: 0.5})
	for _, v := range got2.All {
		if v.MemoryID == "m3" && math.Abs(v.Score-0.4) > 1e-9 {
			t.Fatalf("m3 with tol=0.5 want 0.4, got %.6f", v.Score)
		}
	}
}

// TestMontyFuseMonotonicAbsoluteFloorBlocksFusion
// 绝对下限：top1 < AbsoluteFloor（默认 0.6）→ 禁止融合，返回单条 + LowConfidence。
func TestMontyFuseMonotonicAbsoluteFloorBlocksFusion(t *testing.T) {
	ctx := context.Background()
	cands := []RecalledCandidate{
		montyCand("m1", "A"),
		montyCand("m2", "B"),
	}
	// 两个信号各自只命中一个候选：m1 拿 s1 最优，m2 拿 s2 最优。
	// m1 = s1 1.0 + s2 0 → 0.5；m2 = s1 0（缺失）+ s2 1.0 → 0.5。top1=0.5 < 0.6。
	rf := newMontyRF(
		montyFixedSignal("s1", 1.0, map[string]float64{"m1": 1.0}),
		montyFixedSignal("s2", 1.0, map[string]float64{"m2": 1.0}),
	)
	got := rf.FuseMonotonic(ctx, "q", cands, MonotonicConfig{})
	if got.FloorPass {
		t.Fatalf("top1=%.3f should be below floor %.2f", got.Top1Score, got.AbsoluteFloor)
	}
	if len(got.Ranked) != 1 {
		t.Fatalf("below floor must return single result, got %d", len(got.Ranked))
	}
	if !got.Ranked[0].LowConfidence {
		t.Fatalf("below floor must set LowConfidence")
	}
	if got.Fused {
		t.Fatalf("below floor must not fuse")
	}

	// 抬升绝对下限之上（两信号同时强烈支持 m1）→ 通过下限，唯一最优。
	rf2 := newMontyRF(
		montyFixedSignal("s1", 1.0, map[string]float64{"m1": 1.0, "m2": 0.5}),
		montyFixedSignal("s2", 1.0, map[string]float64{"m1": 1.0, "m2": 0.5}),
	)
	got2 := rf2.FuseMonotonic(ctx, "q", cands, MonotonicConfig{})
	if !got2.FloorPass {
		t.Fatalf("top1=%.3f should pass floor %.2f", got2.Top1Score, got2.AbsoluteFloor)
	}
	if len(got2.Ranked) != 1 || got2.Ranked[0].MemoryID != "m1" {
		t.Fatalf("want unique optimum m1, got %v", got2.Ranked)
	}
	if got2.Ranked[0].LowConfidence {
		t.Fatalf("above floor must not set LowConfidence")
	}
}

// TestMontyFuseMonotonicNearTieFuses 接近融合：分差比例 < tolerance → 并列返回并互填 FusedWith。
func TestMontyFuseMonotonicNearTieFuses(t *testing.T) {
	ctx := context.Background()
	cands := []RecalledCandidate{
		montyCand("m1", "A"),
		montyCand("m2", "B"),
	}
	// 单信号：m1=1.0（证据 1.0），m2=0.99（diff=0.01 → 证据 0.95）。gap≈5% < 20%。
	rf := newMontyRF(montyFixedSignal("s1", 1.0, map[string]float64{"m1": 1.0, "m2": 0.99}))
	got := rf.FuseMonotonic(ctx, "q", cands, MonotonicConfig{})
	if !got.Fused {
		t.Fatalf("near tie should fuse, top1=%.4f", got.Top1Score)
	}
	if len(got.Ranked) != 2 {
		t.Fatalf("want 2 fused results, got %d", len(got.Ranked))
	}
	if len(got.Ranked[0].FusedWith) != 1 || got.Ranked[0].FusedWith[0] != got.Ranked[1].MemoryID {
		t.Fatalf("FusedWith not cross-filled: %v", got.Ranked)
	}
}

// TestMontyFuseMonotonicDeterministic 确定性：输入顺序不影响结果（无随机源 / map 迭代序依赖）。
func TestMontyFuseMonotonicDeterministic(t *testing.T) {
	ctx := context.Background()
	a := montyCand("m1", "A")
	b := montyCand("m2", "B")
	c := montyCand("m3", "C")
	rf := newMontyRF(montyFixedSignal("s1", 1.0, map[string]float64{"m1": 1.0, "m2": 0.9, "m3": 0.9}))

	r1 := rf.FuseMonotonic(ctx, "q", []RecalledCandidate{a, b, c}, MonotonicConfig{})
	r2 := rf.FuseMonotonic(ctx, "q", []RecalledCandidate{c, a, b}, MonotonicConfig{})
	if len(r1.Ranked) != len(r2.Ranked) {
		t.Fatalf("order-dependent length: %d vs %d", len(r1.Ranked), len(r2.Ranked))
	}
	for i := range r1.Ranked {
		if r1.Ranked[i].MemoryID != r2.Ranked[i].MemoryID || r1.Ranked[i].Score != r2.Ranked[i].Score {
			t.Fatalf("order-dependent result at %d: %v vs %v", i, r1.Ranked[i], r2.Ranked[i])
		}
	}
}

// TestMontyFuseMonotonicDoesNotTouchDefaultPath
// 默认路径不变：调用 FuseMonotonic 前后，RankFusion.Rank（RRF）输出逐位一致。
func TestMontyFuseMonotonicDoesNotTouchDefaultPath(t *testing.T) {
	ctx := context.Background()
	cands := []RecalledCandidate{
		montyCand("m1", "A"),
		montyCand("m2", "B"),
		montyCand("m3", "C"),
	}
	rf := DefaultRankFusion()
	rf.Now = montyFixedNow()
	before := rf.Rank(ctx, "查询", cands)

	_ = rf.FuseMonotonic(ctx, "查询", cands, MonotonicConfig{})

	after := rf.Rank(ctx, "查询", cands)
	if len(before) != len(after) {
		t.Fatalf("default path length changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].MemoryID != after[i].MemoryID || before[i].Score != after[i].Score {
			t.Fatalf("default path changed at %d: %v -> %v", i, before[i], after[i])
		}
	}
	// 空输入不 panic。
	if got := rf.FuseMonotonic(ctx, "q", nil, MonotonicConfig{}); len(got.Ranked) != 0 {
		t.Fatalf("nil cands must return empty, got %v", got.Ranked)
	}
	// 无信号不 panic。
	empty := &RankFusion{Weights: map[string]float64{}}
	if got := empty.FuseMonotonic(ctx, "q", cands, MonotonicConfig{}); len(got.Ranked) != 0 {
		t.Fatalf("no signals must return empty, got %v", got.Ranked)
	}
}
