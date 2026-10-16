// node_role 单测（S2）
package kernel

import (
	"context"
	"testing"
	"time"
)

func TestNodeRoleRegistry(t *testing.T) {
	r := NewInMemoryNodeRegistry()

	// 空 ID / 空 Role 报错
	if err := r.Register(NodeInfo{}); err == nil {
		t.Fatal("expected error for empty node")
	}
	if err := r.Register(NodeInfo{ID: "n1"}); err == nil {
		t.Fatal("expected error for empty role")
	}

	// 正常注册
	err := r.Register(NodeInfo{ID: "n1", Role: RoleWorker, Host: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Register(NodeInfo{ID: "n2", Role: RoleCoordinator, Host: "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}

	// Get
	n, ok := r.Get("n1")
	if !ok || n.Role != RoleWorker || n.Host != "127.0.0.1" {
		t.Fatalf("get mismatch: %#v %v", n, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("should not find unregistered")
	}

	// List by role
	ws := r.List(RoleWorker)
	if len(ws) != 1 || ws[0].ID != "n1" {
		t.Fatalf("list worker: %#v", ws)
	}
	all := r.List("")
	if len(all) != 2 {
		t.Fatalf("list all: %#v", all)
	}
	// 排序：coordinator < worker（按字符串）
	if all[0].Role != RoleCoordinator || all[1].Role != RoleWorker {
		t.Fatalf("list order wrong: %v %v", all[0].Role, all[1].Role)
	}

	// Heartbeat
	err = r.Heartbeat(Heartbeat{NodeID: "n1", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Heartbeat(Heartbeat{NodeID: "nope"}); err == nil {
		t.Fatal("expected heartbeat error for unknown")
	}

	// Unregister
	if err := r.Unregister("nope"); err == nil {
		t.Fatal("expected unregister error")
	}
	if err := r.Unregister("n1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("n1"); ok {
		t.Fatal("should be removed")
	}
}

func TestNodeRegistryPurgeStale(t *testing.T) {
	r := NewInMemoryNodeRegistry()
	_ = r.Register(NodeInfo{ID: "old", Role: RoleWorker, LastSeen: time.Now().UTC().Add(-2 * time.Hour)})
	_ = r.Register(NodeInfo{ID: "fresh", Role: RoleWorker, LastSeen: time.Now().UTC()})

	removed := r.PurgeStale(time.Hour)
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("purge expected [old], got %v", removed)
	}
	n, ok := r.Get("old")
	if !ok || n.Status != StatusOffline {
		t.Fatalf("old node should be offline: %#v %v", n, ok)
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Fatal("fresh should survive")
	}
}

func TestNodeBroadcaster(t *testing.T) {
	b := NewInMemoryBroadcaster()
	ctx := context.Background()

	var got []Revocation
	cancel := b.Subscribe("t1", func(r Revocation) { got = append(got, r) })
	defer cancel()

	// 广播到 t1
	err := b.Revoke(ctx, Revocation{MemoryIDs: []string{"m1"}, TenantID: "t1", IssuedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MemoryIDs[0] != "m1" {
		t.Fatalf("got wrong: %#v", got)
	}

	// 广播到 t2 不应触发 t1
	err = b.Revoke(ctx, Revocation{MemoryIDs: []string{"m2"}, TenantID: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("t2 should not trigger t1 sub: %#v", got)
	}

	// 广播到空 tenant（全局）应触发所有订阅
	err = b.Revoke(ctx, Revocation{MemoryIDs: []string{"m3"}, TenantID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("global should trigger: %#v", got)
	}

	// cancel 后不再触发
	cancel()
	err = b.Revoke(ctx, Revocation{MemoryIDs: []string{"m4"}, TenantID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("canceled sub should not fire: %#v", got)
	}
}

func TestNoopPreFilterAndBuildTopology(t *testing.T) {
	// NoopPreFilter 全部放行
	pf := NoopPreFilter{}
	ids, err := pf.CandidateIDs(context.Background(), "t1", "query")
	if err != nil || ids != nil {
		t.Fatalf("noop filter: %v %v", ids, err)
	}

	// BuildTopology 空节点 -> nil
	if got := BuildTopology(map[string][]RoleSpec{}, 0); got != nil {
		t.Fatalf("empty topology: %#v", got)
	}

	// BuildTopology 多角色
	nodes := BuildTopology(map[string][]RoleSpec{
		"coordinator": []RoleSpec{{Host: "10.0.0.1"}},
		"worker":      []RoleSpec{{Host: "10.0.0.2"}, {Host: "10.0.0.3"}},
		"archive":     []RoleSpec{{Host: "10.0.0.4"}},
	}, 4)
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes: %#v", nodes)
	}
	// 排序：archive < coordinator < worker
	if nodes[0].Role != RoleArchive || nodes[1].Role != RoleCoordinator {
		t.Fatalf("order wrong: %v %v", nodes[0].Role, nodes[1].Role)
	}
	for _, n := range nodes {
		if n.Status != StatusActive {
			t.Fatalf("status should be active: %#v", n)
		}
		if n.LastSeen.IsZero() {
			t.Fatalf("last_seen should be set: %#v", n)
		}
	}
}
