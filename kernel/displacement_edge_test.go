package kernel

import (
	"testing"
	"time"
)

func mustEdge(t *testing.T, store *EdgeStore, from, to string, rel EdgeRelation, w float64) {
	t.Helper()
	if _, isNew := store.AddEdge(from, to, rel, w); !isNew {
		t.Fatalf("expected new edge %s->%s(%s)", from, to, rel)
	}
}

func TestEdgeStoreAddAndIdempotent(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	clock := t0
	store := NewEdgeStore(DefaultMaxOutEdges, func() time.Time { return clock })

	if _, isNew := store.AddEdge("m1", "m2", RelCausal, 0.5); !isNew {
		t.Fatal("first add should be new")
	}
	// 同三元组重复写入：不新增行、不更新权重。
	if _, isNew := store.AddEdge("m1", "m2", RelCausal, 0.9); isNew {
		t.Fatal("duplicate triple must not add a row")
	}
	if got := store.EdgeCount(); got != 1 {
		t.Fatalf("EdgeCount=%d, want 1 (idempotent)", got)
	}
	got := store.OutEdges("m1")
	if len(got) != 1 || got[0].Weight != 0.5 {
		t.Fatalf("duplicate add must not overwrite weight: %+v", got)
	}
}

func TestEdgeStoreSoftDeleteNotPhysical(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	clock := t0
	store := NewEdgeStore(DefaultMaxOutEdges, func() time.Time { return clock })
	mustEdge(t, store, "m1", "m2", RelTemporal, 0.3)

	if !store.InvalidateEdge("m1", "m2", RelTemporal) {
		t.Fatal("invalidate should succeed")
	}
	// 软删后不参与召回。
	if got := store.OutEdges("m1"); len(got) != 0 {
		t.Fatalf("soft-deleted edge must not be returned: %+v", got)
	}
	// 但行保留在表中（可审计），不物理删除。
	if store.EdgeCount() != 1 {
		t.Fatalf("row must be retained for audit, EdgeCount=%d", store.EdgeCount())
	}
	if store.AliveEdgeCount() != 0 {
		t.Fatalf("alive count should drop to 0")
	}
	// 重复软删返回 false。
	if store.InvalidateEdge("m1", "m2", RelTemporal) {
		t.Fatal("second invalidate must return false")
	}
}

func TestEdgeStoreCapacityEvictionReal(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	clock := t0
	store := NewEdgeStore(3, func() time.Time { return clock }) // 出边上 3

	weights := []float64{0.8, 0.2, 0.5, 0.1}
	for i, w := range weights {
		clock = t0.Add(time.Duration(i) * time.Minute)
		mustEdge(t, store, "m1", "to"+string(rune('A'+i)), RelTemporal, w)
	}
	// 上限 3：第 4 条加入后应淘汰 weight 最低（0.1=toD）。
	alive := store.OutEdges("m1")
	if len(alive) != 3 {
		t.Fatalf("alive out-edges=%d, want 3", len(alive))
	}
	for _, e := range alive {
		if e.ToMemoryID == "toD" {
			t.Fatal("lowest-weight edge toD must be soft-deleted")
		}
	}
	// 被淘汰行仍在表中（软删，不物理删）。
	if store.EdgeCount() != 4 {
		t.Fatalf("evicted row must be retained, EdgeCount=%d", store.EdgeCount())
	}
	if store.AliveEdgeCount() != 3 {
		t.Fatalf("alive=%d, want 3", store.AliveEdgeCount())
	}
}

func TestBuildEdgesHeuristicRules(t *testing.T) {
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	mk := func(id, tenant, user, entity, task, scene, fact string, layer MemoryLayer, at time.Time) Memory {
		return Memory{
			MemoryID:   id,
			TenantID:   tenant,
			UserID:     user,
			MemoryKind: KindFact,
			Fact:       fact,
			Layer:      layer,
			RecordedAt: at,
			ReferenceFrame: ReferenceFrame{
				Tenant: tenant, User: user, Entity: entity, Task: task, Scene: scene,
			},
		}
	}

	mems := []Memory{
		// entity-alias：同归一化实体（大小写/标点差异）。
		mk("e1", "tn", "u1", "GoLand", "task1", "s", "x", LayerL2, base),
		mk("e2", "tn", "u1", "goland.", "task1", "s", "y", LayerL2, base.Add(time.Hour)),
		// task-flow：同 task_id。
		mk("t1", "tn", "u1", "entX", "task9", "s", "x", LayerL2, base),
		mk("t2", "tn", "u1", "entX", "task9", "s", "y", LayerL2, base.Add(2*time.Hour)),
		// temporal：同 scene、间隔 1 天。
		mk("tp1", "tn", "u1", "entY", "", "sceneZ", "x", LayerL2, base),
		mk("tp2", "tn", "u1", "entY", "", "sceneZ", "y", LayerL2, base.Add(24*time.Hour)),
		// causal：前条 entity 非空，后条 fact 含指代词。
		mk("c1", "tn", "u1", "Redis", "", "s", "redis cached", LayerL2, base),
		mk("c2", "tn", "u1", "Other", "", "s", "它很快", LayerL2, base.Add(3*time.Hour)),
		// L1：不应建边。
		mk("l1", "tn", "u1", "GoLand", "task1", "s", "x", LayerL1, base),
		mk("l2", "tn", "u1", "goland", "task1", "s", "y", LayerL1, base.Add(time.Hour)),
		// 零时间：temporal 不建边。
		mk("z1", "tn", "u1", "entZ", "", "sceneZ", "x", LayerL2, time.Time{}),
		mk("z2", "tn", "u1", "entZ", "", "sceneZ", "y", LayerL2, time.Time{}),
	}

	clock := base
	store := NewEdgeStore(DefaultMaxOutEdges, func() time.Time { return clock })
	added := BuildEdgesHeuristic(mems, store)
	if added == 0 {
		t.Fatal("expected some heuristic edges")
	}

	// 按起点统计关系。
	got := map[EdgeRelation]int{}
	for _, e := range store.OutEdges("e1") {
		got[e.Relation]++
	}
	// e1->e2 应为 entity-alias。
	found := false
	for _, e := range store.OutEdges("e1") {
		if e.ToMemoryID == "e2" && e.Relation == RelEntityAlias && e.Weight == EdgeWeightEntityAlias {
			found = true
		}
	}
	if !found {
		t.Fatal("expected e1->e2 entity-alias edge")
	}
	_ = got

	// L1 记忆绝不建边：l1 不应有任何出边。
	if es := store.OutEdges("l1"); len(es) != 0 {
		t.Fatalf("L1 memory must not get edges: %+v", es)
	}
	// 零时间对不产生 temporal 边。
	for _, e := range store.OutEdges("z1") {
		if e.Relation == RelTemporal {
			t.Fatal("zero-recorded pair must not produce temporal edge")
		}
	}
	// causal 边存在。
	causalFound := false
	for _, e := range store.OutEdges("c1") {
		if e.ToMemoryID == "c2" && e.Relation == RelCausal {
			causalFound = true
		}
	}
	if !causalFound {
		t.Fatal("expected c1->c2 causal edge")
	}
}

func TestNormalizeEntity(t *testing.T) {
	cases := map[string]string{
		"GoLand.":  "goland",
		"  Redis ": "redis",
		"K8s!":     "k8s",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeEntity(in); got != want {
			t.Fatalf("normalizeEntity(%q)=%q, want %q", in, got, want)
		}
	}
}
