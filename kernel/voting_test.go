// kernel/voting_test.go — R6-A 投票共识 + 参考系多帧单测。
//
// 覆盖任务书 A.2.1 三个可判定语义：
//   - ConsensusVoter 倒数秩计票的确定性；
//   - 平局按 Source 字典序打破（无随机源）；
//   - Divergence 分歧度：全柱一致 → 0；严重分裂 → 接近 1；
//   - MatchFrames 多帧匹配：任一副帧命中即得分（取代单帧稀释）；
//   - RankFusion.FusionMode=vote 时走 consensusVoteRank，且 rrf 路径逐位不变。
package kernel

import (
	"context"
	"testing"
)

// TestConsensusVoterBasicRank 基本计票：3 路解释、一致 top1。
func TestConsensusVoterBasicRank(t *testing.T) {
	interps := []Interpretation{
		{Source: "entity", TopIDs: []string{"a", "b", "c"}},
		{Source: "keyword", TopIDs: []string{"a", "b"}},
		{Source: "semantic", TopIDs: []string{"a", "c"}},
	}
	v := &ConsensusVoter{}
	res := v.Vote(interps)
	if len(res.Ranking) == 0 || res.Ranking[0] != "a" {
		t.Fatalf("want top1=a, got %v", res.Ranking)
	}
	// a: 1+1+1 = 3；b: 0.5+0.5=1；c: 0.333+0.5=0.833。
	if got := res.Scores["a"]; got != 3.0 {
		t.Fatalf("want score a=3.0, got %.4f", got)
	}
	if got := res.VoteCounts["a"]; got != 3 {
		t.Fatalf("want votes a=3, got %d", got)
	}
	// 3/（3+1+0.833）≈ 0.621；全柱一致时 Consensus 高、Divergence 低。
	if res.Consensus <= 0.6 || res.Consensus > 0.63 {
		t.Fatalf("consensus out of expected: %.4f", res.Consensus)
	}
	if res.Divergence <= 0.36 || res.Divergence > 0.4 {
		t.Fatalf("divergence out of expected: %.4f", res.Divergence)
	}
	if len(res.Interpretations) != 3 {
		t.Fatalf("want 3 interpretations, got %d", len(res.Interpretations))
	}
}

// TestConsensusVoterDeterministicOrder 输入顺序不影响结果（确定性）。
func TestConsensusVoterDeterministicOrder(t *testing.T) {
	base := []Interpretation{
		{Source: "entity", TopIDs: []string{"a", "b", "c"}},
		{Source: "keyword", TopIDs: []string{"a", "b"}},
		{Source: "semantic", TopIDs: []string{"a", "c"}},
	}
	rev := []Interpretation{base[2], base[1], base[0]}
	v := &ConsensusVoter{}
	r1 := v.Vote(base)
	r2 := v.Vote(rev)
	if r1.Ranking[0] != r2.Ranking[0] || r1.Consensus != r2.Consensus {
		t.Fatalf("order-dependent result: %v vs %v", r1.Ranking, r2.Ranking)
	}
	for k, sc := range r1.Scores {
		if r2.Scores[k] != sc {
			t.Fatalf("score differs for %s: %.4f vs %.4f", k, sc, r2.Scores[k])
		}
	}
}

// TestConsensusVoterTieBrokenBySource 平局按 Source 字典序打破（无随机）。
func TestConsensusVoterTieBrokenBySource(t *testing.T) {
	// 两路解释互为镜像：x 的 top1 是 a，y 的 top1 是 b → a/b 平分。
	interps := []Interpretation{
		{Source: "semantic", TopIDs: []string{"a", "b"}},
		{Source: "keyword", TopIDs: []string{"b", "a"}},
	}
	v := &ConsensusVoter{}
	res := v.Vote(interps)
	// 计票：a=1(keyword rank2=0.5) 错了——重算：
	//   semantic: a=1, b=0.5
	//   keyword:  b=1, a=0.5
	// 合计 a=1.5, b=1.5 → 平局 → 按 ID 字典序 a < b → a 胜。
	if res.Ranking[0] != "a" {
		t.Fatalf("want tie broken to a, got %v", res.Ranking)
	}
	if res.Scores["a"] != res.Scores["b"] {
		t.Fatalf("expect tied scores, got a=%.4f b=%.4f", res.Scores["a"], res.Scores["b"])
	}
}

// TestConsensusVoterFullDivergence 全分歧：2 路各选不同候选 → Divergence 高。
func TestConsensusVoterFullDivergence(t *testing.T) {
	interps := []Interpretation{
		{Source: "semantic", TopIDs: []string{"a"}},
		{Source: "keyword", TopIDs: []string{"b"}},
	}
	v := &ConsensusVoter{}
	res := v.Vote(interps)
	// a=1, b=1 → total=2, consensus=1/2=0.5, divergence=0.5。
	if res.Consensus != 0.5 || res.Divergence != 0.5 {
		t.Fatalf("want consensus=divergence=0.5, got %.4f/%.4f", res.Consensus, res.Divergence)
	}
}

// TestConsensusVoterEmpty 空输入返回空结果。
func TestConsensusVoterEmpty(t *testing.T) {
	v := &ConsensusVoter{}
	res := v.Vote(nil)
	if len(res.Ranking) != 0 || res.Consensus != 0 || res.Divergence != 0 {
		t.Fatalf("want empty result, got %+v", res)
	}
}

// TestMatchFramesMultiFrame 多帧匹配：任一副帧命中即得分。
func TestMatchFramesMultiFrame(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Project: "项目1"}
	// 候选 1：两帧，第二帧与查询完全匹配。
	c1 := FrameSet{Frames: []ReferenceFrame{
		{Entity: "客户B", Project: "项目0"},
		{Entity: "客户A", Project: "项目1"},
	}}
	if got := MatchFrames(q, &c1); got != 1.0 {
		t.Fatalf("want 1.0 (second frame full match), got %.4f", got)
	}
	// 候选 2：单帧只命中 1/2 坐标。
	c2 := FrameSet{Frames: []ReferenceFrame{
		{Entity: "客户A", Project: "项目2"},
	}}
	if got := MatchFrames(q, &c2); got != 0.5 {
		t.Fatalf("want 0.5 (1/2 coords), got %.4f", got)
	}
	// 候选 3：完全无关。
	c3 := FrameSet{Frames: []ReferenceFrame{{Entity: "客户Z"}}}
	if got := MatchFrames(q, &c3); got != 0.0 {
		t.Fatalf("want 0.0 (no match), got %.4f", got)
	}
}

// TestMatchFramesEmptyQuery 查询端无坐标 → 0（不给正偏向）。
func TestMatchFramesEmptyQuery(t *testing.T) {
	q := &ReferenceFrame{}
	c := FrameSet{Frames: []ReferenceFrame{{Entity: "客户A"}}}
	if got := MatchFrames(q, &c); got != 0 {
		t.Fatalf("want 0 for empty query frame, got %.4f", got)
	}
}

// TestMatchFramesNil 空指针安全。
func TestMatchFramesNil(t *testing.T) {
	if got := MatchFrames(nil, &FrameSet{}); got != 0 {
		t.Fatalf("want 0, got %.4f", got)
	}
	q := &ReferenceFrame{Entity: "客户A"}
	if got := MatchFrames(q, nil); got != 0 {
		t.Fatalf("want 0, got %.4f", got)
	}
}

// TestVoteFusionMode 投票模式：一致 top1 时 rank 与 rrf 一致，且侧通道字段填充。
func TestVoteFusionMode(t *testing.T) {
	sigs := []NamedSignal{
		fixedSignal("semantic", 12.0, map[string]float64{"c1": 1.0, "c2": 0.5}),
		fixedSignal("keyword", 3.2, map[string]float64{"c1": 1.0}),
		fixedSignal("entity", 2.9, map[string]float64{"c1": 1.0, "c2": 0.9}),
	}
	rf := newTestFusion(sigs...)
	rf.FusionMode = FusionModeVote
	cands := []RecalledCandidate{
		mkCand("c1", "客户A偏好周二开会", "客户A", KindPreference, 1.0),
		mkCand("c2", "客户A偏好周一开会", "客户A", KindPreference, 0.8),
	}
	got := rf.Rank(context.Background(), "客户A 偏好", cands)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].MemoryID != "c1" {
		t.Fatalf("want top1=c1, got %s", got[0].MemoryID)
	}
	if got[0].ConsensusVoteScore <= 0 {
		t.Fatalf("want positive vote score, got %.4f", got[0].ConsensusVoteScore)
	}
	if got[0].VoteDivergence <= 0 || got[0].VoteDivergence >= 1 {
		t.Fatalf("divergence in (0,1): %.4f", got[0].VoteDivergence)
	}
	if len(got[0].VoteSources) != 3 {
		t.Fatalf("want 3 vote sources, got %v", got[0].VoteSources)
	}
}

// TestRRFPathUnchangedByFusionModeDefault 默认 rrf：FusionMode 置空时逐位不变。
func TestRRFPathUnchangedByFusionModeDefault(t *testing.T) {
	sigs := []NamedSignal{
		fixedSignal("semantic", 12.0, map[string]float64{"c1": 1.0, "c2": 0.5}),
		fixedSignal("keyword", 3.2, map[string]float64{"c1": 1.0}),
	}
	rf := newTestFusion(sigs...)
	rf.FusionMode = FusionModeRRF
	cands := []RecalledCandidate{
		mkCand("c1", "客户A偏好周二开会", "客户A", KindPreference, 1.0),
		mkCand("c2", "客户A偏好周一开会", "客户A", KindPreference, 0.8),
	}
	got := rf.Rank(context.Background(), "客户A 偏好", cands)
	if len(got) != 2 || got[0].MemoryID != "c1" {
		t.Fatalf("rrf path broken: %+v", got)
	}
	if got[0].ConsensusVoteScore != 0 || got[0].VoteDivergence != 0 {
		t.Fatalf("rrf must not populate vote side-channels: %+v", got[0])
	}
}
