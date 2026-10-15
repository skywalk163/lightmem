package kernel

import (
	"testing"
	"time"
)

func newTestEdgeStore(t *testing.T) *EdgeStore {
	t.Helper()
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	return NewEdgeStore(DefaultMaxOutEdges, func() time.Time { return now })
}

func TestDisplacementRecallSoftRankingAndHopCap(t *testing.T) {
	store := newTestEdgeStore(t)
	// s -> a (w0.8); a -> b (w0.5); b -> c (w0.9)
	mustEdge(t, store, "s", "a", RelEntityAlias, 0.8)
	mustEdge(t, store, "a", "b", RelCausal, 0.5)
	mustEdge(t, store, "b", "c", RelTemporal, 0.9)

	acceptAll := func(id string) bool { return true }
	got := DisplacementRecall([]string{"s"}, store, acceptAll, MultihopMontyConfig{MaxHops: 2})

	byID := map[string]DisplacementCandidate{}
	for _, c := range got {
		byID[c.MemoryID] = c
	}
	// hop0 seed
	if _, ok := byID["s"]; !ok {
		t.Fatal("seed s must be included")
	}
	// hop1: s->a weight 0.8 => score = 1.0*0.8*0.5 = 0.4
	ca, ok := byID["a"]
	if !ok || ca.Hop != 1 {
		t.Fatalf("a should be hop1: %+v", byID["a"])
	}
	if abs(ca.Score-0.4) > 1e-9 {
		t.Fatalf("a score=%v, want 0.4", ca.Score)
	}
	// hop2: path s->a->b => 0.8*0.5 * 0.5^2 = 0.4*0.25 = 0.1
	cb, ok := byID["b"]
	if !ok || cb.Hop != 2 {
		t.Fatalf("b should be hop2: %+v", byID["b"])
	}
	if abs(cb.Score-0.1) > 1e-9 {
		t.Fatalf("b score=%v, want 0.1", cb.Score)
	}
	// hop3: c must NOT appear (hop cap = 2).
	if _, ok := byID["c"]; ok {
		t.Fatal("c at hop3 must be pruned by hop cap")
	}
	// 软排序：score 降序 s(1.0) > a(0.4) > b(0.1)
	if got[0].MemoryID != "s" || got[1].MemoryID != "a" || got[2].MemoryID != "b" {
		t.Fatalf("soft ranking wrong: %+v", got)
	}
}

func TestDisplacementRecallHardFilterPrunes(t *testing.T) {
	store := newTestEdgeStore(t)
	mustEdge(t, store, "s", "a", RelEntityAlias, 0.8)
	mustEdge(t, store, "a", "b", RelCausal, 0.5)
	mustEdge(t, store, "b", "c", RelTaskFlow, 0.9)

	// 硬条件：b 与查询不一致 -> 剪枝 b，且不再从 b 扩展到 c。
	accept := func(id string) bool { return id != "b" }
	got := DisplacementRecall([]string{"s"}, store, accept, MultihopMontyConfig{MaxHops: 2})
	for _, c := range got {
		if c.MemoryID == "b" {
			t.Fatal("b hard-filtered must be pruned, not returned")
		}
		if c.MemoryID == "c" {
			t.Fatal("c must not be expanded through pruned b — hard filter boundary not bypassed")
		}
	}
}

func TestDisplacementRecallSeedRejectedNotExpanded(t *testing.T) {
	store := newTestEdgeStore(t)
	mustEdge(t, store, "bad", "good", RelCausal, 0.9)
	mustEdge(t, store, "good", "leaf", RelCausal, 0.9)

	// 种子 bad 硬过滤不通过 -> 不返回，且不从它扩展到 good。
	accept := func(id string) bool { return id != "bad" }
	got := DisplacementRecall([]string{"bad", "good"}, store, accept, MultihopMontyConfig{MaxHops: 2})
	for _, c := range got {
		if c.MemoryID == "bad" {
			t.Fatal("rejected seed must not appear")
		}
	}
	// good 是合法种子(hop0)，并可扩展到 leaf(hop1)。
	byID := map[string]bool{}
	for _, c := range got {
		byID[c.MemoryID] = true
	}
	if !byID["good"] {
		t.Fatal("good seed should be included")
	}
	if !byID["leaf"] {
		t.Fatal("good should reach leaf via edge")
	}
}

func TestDisplacementRecallMaxHopsClamped(t *testing.T) {
	store := newTestEdgeStore(t)
	mustEdge(t, store, "s", "a", RelCausal, 0.5)
	// 传 MaxHops=5 应被收敛到 2，不 panic、不越界。
	got := DisplacementRecall([]string{"s"}, store, func(string) bool { return true }, MultihopMontyConfig{MaxHops: 5})
	if len(got) == 0 {
		t.Fatal("expected recall results")
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
