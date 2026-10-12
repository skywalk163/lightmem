// R4-A：multi-hop 二次 hop 证据重排单测。
// 红线：不调 LLM；不用 time.Now()/map 迭代序；不改 Schema 语义。
package kernel

import (
	"testing"
	"time"
)

// mhMem 构造一条测试用记忆（定死 occurred_at，避免引入随机/当前时间）。
func mhMem(id, fact, user, scene string, when time.Time, entity string) *Memory {
	return &Memory{
		MemoryID:       id,
		TenantID:       "t1",
		UserID:         user,
		MemoryKind:     KindFact,
		Fact:           fact,
		ReferenceFrame: ReferenceFrame{Tenant: "t1", User: user, Scene: scene, Entity: entity},
		OccurredAt:     &when,
		RecordedAt:     when,
		Confidence:     0.9,
		Source:         "chat",
	}
}

func mhFused(m *Memory, score float64) FusedResult {
	return FusedResult{MemoryID: m.MemoryID, Fact: m.Fact, Score: score, Memory: m, Layer: LayerL1}
}

// TestMultiHopReRankSingleHopUnchanged 单跳/默认关闭时行为逐位不变。
func TestMultiHopReRankSingleHopUnchanged(t *testing.T) {
	when := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	a := mhFused(mhMem("a", "客户F的偏好是周一", "u1", "s1", when, "客户F"), 0.9)
	b := mhFused(mhMem("b", "客户G的偏好是周二", "u1", "s1", when, "客户G"), 0.8)
	in := []FusedResult{a, b}

	// 1) 默认配置（Enabled=false）：原序返回，且内容逐位相同（含 Reason 为空）。
	got := MultiHopReRank("客户F偏好什么时间开会", in, DefaultMultiHopConfig())
	if len(got) != 2 || got[0].MemoryID != "a" || got[1].MemoryID != "b" {
		t.Fatalf("default config must keep order, got %v", got)
	}
	if got[0].Reason != "" || got[1].Reason != "" {
		t.Fatalf("disabled rerank must not annotate Reason: %q / %q", got[0].Reason, got[1].Reason)
	}
	// 2) 启用但候选数 <2：同样原序返回。
	cfg := MultiHopConfig{Enabled: true, Depth: 1, MinAnchors: 1}
	one := []FusedResult{a}
	if got := MultiHopReRank("客户F偏好什么时间开会", one, cfg); len(got) != 1 || got[0].MemoryID != "a" {
		t.Fatalf("single candidate must be unchanged, got %v", got)
	}
}

// TestMultiHopReRankDirectAnswer 直接回答证据（L1）能把答案卡提升到 top-1。
func TestMultiHopReRankDirectAnswer(t *testing.T) {
	when := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	// 干扰项：与 query 高度词汇重合、融合分更高（模拟 R3-D 公开的 6 条排序失败）。
	distractor := mhFused(mhMem("d1", "How much money did I raise in total through all the charity events I participated in? => $5,850", "u1", "s1", when, "charity"), 0.97)
	// 目标：原文完整包含 query（LongMemEval question => answer 形态），融合分略低。
	target := mhFused(mhMem("t1", "How much money did I raise for charity in total? => $3,750", "u1", "s1", when, "charity"), 0.95)
	in := []FusedResult{distractor, target}

	cfg := MultiHopConfig{Enabled: true, Depth: 1, MinAnchors: 1}
	q := "How much money did I raise for charity in total?"
	got := MultiHopReRank(q, in, cfg)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(got))
	}
	if got[0].MemoryID != "t1" {
		t.Fatalf("direct-answer candidate must rank first, got %s", got[0].MemoryID)
	}
	if got[0].Reason == "" {
		t.Fatalf("re-ranked candidate must carry hop Reason")
	}
	answers, cover := MultiHopRankKey(q, target, HopAnchors(q))
	if !answers {
		t.Fatalf("target should answer query verbatim")
	}
	if cover < 1.0 {
		t.Fatalf("target coverage should be 1.0, got %.3f", cover)
	}
}

// TestMultiHopReRankDeterministic 重排结果确定性：同输入重复 50 次结果逐位一致。
func TestMultiHopReRankDeterministic(t *testing.T) {
	when := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	in := []FusedResult{
		mhFused(mhMem("m1", "alpha beta gamma delta", "u1", "s1", when, "e1"), 0.9),
		mhFused(mhMem("m2", "beta gamma alpha", "u1", "s1", when.Add(time.Hour), "e2"), 0.8),
		mhFused(mhMem("m3", "gamma delta alpha beta", "u1", "s1", when.Add(2*time.Hour), "e3"), 0.85),
		mhFused(mhMem("m4", "alpha alpha alpha", "u1", "s1", when.Add(3*time.Hour), "e4"), 0.7),
	}
	cfg := MultiHopConfig{Enabled: true, Depth: 2, ContextWindow: 24 * time.Hour, MinAnchors: 1}
	q := "alpha beta gamma"
	first := MultiHopReRank(q, in, cfg)
	want := make([]string, len(first))
	for i, f := range first {
		want[i] = f.MemoryID + "|" + f.Reason
	}
	for k := 0; k < 50; k++ {
		got := MultiHopReRank(q, in, cfg)
		for i, f := range got {
			if f.MemoryID+"|"+f.Reason != want[i] {
				t.Fatalf("non-deterministic at run %d pos %d: %s|%s != %s",
					k, i, f.MemoryID, f.Reason, want[i])
			}
		}
	}
}

// TestMultiHopReRankContextAugment 上下文增强（L3，Depth>=2）能补强锚点覆盖并标注推理链。
func TestMultiHopReRankContextAugment(t *testing.T) {
	when := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	// 目标自身只覆盖一半锚点；同场景、同日内的邻接记忆提供另一半。
	target := mhFused(mhMem("t1", "NovaTech 入职年份", "u1", "s1", when, "NovaTech"), 0.70)
	neighbor := mhFused(mhMem("n1", "NovaTech 公司 4 years and 9 months", "u1", "s1", when.Add(2*time.Hour), "NovaTech"), 0.60)
	other := mhFused(mhMem("o1", "无关记忆 totally unrelated", "u1", "s2", when, "x"), 0.65)
	in := []FusedResult{other, target, neighbor} // 目标不在首位

	q := "NovaTech 4 years and 9 months"
	anchors := HopAnchors(q)
	_, coverBefore := MultiHopRankKey(q, target, anchors)
	cfg := MultiHopConfig{Enabled: true, Depth: 2, ContextWindow: 24 * time.Hour, MinAnchors: 1}
	hop2ID, coverAfter := hopContextAugment(1, coverBefore, anchors, HopFactTokens(target.Fact), in, cfg)
	if hop2ID != "n1" {
		t.Fatalf("context augment should pick same-frame neighbor n1, got %q", hop2ID)
	}
	if coverAfter <= coverBefore {
		t.Fatalf("augmented coverage should grow: %.3f -> %.3f", coverBefore, coverAfter)
	}
	// Depth=1 时不做上下文增强（默认口径）
	if id, cov := hopContextAugment(1, coverBefore, anchors, HopFactTokens(target.Fact), in,
		MultiHopConfig{Enabled: true, Depth: 1, ContextWindow: 24 * time.Hour}); id != "" || cov != coverBefore {
		t.Fatalf("Depth=1 must not augment: id=%q cov=%v", id, cov)
	}
	// 跨场景邻接不得拼链
	cfgCross := MultiHopConfig{Enabled: true, Depth: 2, ContextWindow: 24 * time.Hour}
	if id, _ := hopContextAugment(1, coverBefore, anchors, HopFactTokens(target.Fact),
		[]FusedResult{target, other}, cfgCross); id != "" {
		t.Fatalf("cross-scene neighbor must not be chained, got %q", id)
	}
}

// TestHopAnchorsStopWords 锚点抽取：剔除功能词、去重、字典序确定。
func TestHopAnchorsStopWords(t *testing.T) {
	got := HopAnchors("How many rare items do I have in total?")
	want := []string{"items", "rare", "total"}
	if len(got) != len(want) {
		t.Fatalf("anchors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("anchors[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
	// 中文：单字 + 二元组
	cjk := HopAnchors("客户F偏好什么时间开会")
	if len(cjk) == 0 {
		t.Fatalf("CJK anchors must not be empty")
	}
	if !containsString(cjk, "客户") {
		t.Fatalf("CJK bigram missing: %v", cjk)
	}
}

func containsString(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}
