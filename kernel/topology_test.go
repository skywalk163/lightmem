// topology 单测（S2）
package kernel

import (
	"testing"
	"time"
)

func TestTopologyBasic(t *testing.T) {
	tp := NewTopology("root")
	if tp.Root() != "root" {
		t.Fatalf("root mismatch: %v", tp.Root())
	}

	// 空 ID / 空 Role 报错
	if err := tp.AddNode(&Node{}); err == nil {
		t.Fatal("expected error for empty node")
	}
	if err := tp.AddNode(&Node{ID: "n1"}); err == nil {
		t.Fatal("expected error for empty role")
	}

	// 添加 root + children
	err := tp.AddNode(&Node{ID: "root", Role: RoleCoordinator, Host: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	err = tp.AddNode(&Node{ID: "w1", Role: RoleWorker, Host: "10.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	err = tp.AddNode(&Node{ID: "w2", Role: RoleWorker, Host: "10.0.0.3"})
	if err != nil {
		t.Fatal(err)
	}

	// Connect + hop 重算
	if err := tp.Connect("root", "w1"); err != nil {
		t.Fatal(err)
	}
	if err := tp.Connect("w1", "w2"); err != nil {
		t.Fatal(err)
	}

	edges := tp.Edges()
	if len(edges) != 2 {
		t.Fatalf("edges: %#v", edges)
	}
	// w1 的 hop = 1；w2 的 hop = 2
	n, ok := tp.Get("w1")
	if !ok || n.Parent != "root" {
		t.Fatalf("w1 parent: %#v", n)
	}
	n2, _ := tp.Get("w2")
	if n2.Parent != "w1" {
		t.Fatalf("w2 parent: %v", n2.Parent)
	}

	// All 排序
	all := tp.All()
	if len(all) != 3 {
		t.Fatalf("all: %#v", all)
	}
	if all[0].Role != RoleCoordinator {
		t.Fatalf("all[0] role: %v", all[0].Role)
	}

	// RemoveNode
	removed := tp.RemoveNode("w2")
	if len(removed) != 1 || removed[0] != "w2" {
		t.Fatalf("removed: %#v", removed)
	}
	if _, ok := tp.Get("w2"); ok {
		t.Fatal("w2 should be gone")
	}
	// w2 的子节点孤儿挂 root
	err = tp.AddNode(&Node{ID: "w3", Role: RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	_ = tp.Connect("w2", "w3") // w2 已删除 -> 报错
	if err := tp.Connect("root", "w3"); err != nil {
		t.Fatal(err)
	}
}

func TestTopologyRemoveNonExistent(t *testing.T) {
	tp := NewTopology("root")
	removed := tp.RemoveNode("nope")
	if removed != nil {
		t.Fatalf("expected nil for non-existent: %#v", removed)
	}
}

func TestRevocationStore(t *testing.T) {
	s := NewRevocationStore()

	// 空 ID 忽略
	s.Add("", Revocation{TenantID: "t1"})
	if len(s.All()) != 0 {
		t.Fatalf("empty id should be ignored: %#v", s.All())
	}

	// 添加永久撤销
	s.Add("m1", Revocation{MemoryIDs: []string{"m1"}, TenantID: "t1", IssuedAt: time.Now().UTC()})
	if !s.Has("m1", "t1") {
		t.Fatal("m1/t1 should be revoked")
	}
	if s.Has("m1", "t2") {
		t.Fatal("m1/t2 should not be revoked (tenant mismatch)")
	}
	if s.Has("m9", "t1") {
		t.Fatal("m9 should not be revoked")
	}

	// 添加 TTL 撤销（10ms 过期）
	s.Add("m2", Revocation{MemoryIDs: []string{"m2"}, TenantID: "t1", TTL: 0, IssuedAt: time.Now().UTC().Add(-time.Second)})
	s.Add("m3", Revocation{MemoryIDs: []string{"m3"}, TenantID: "t1", TTL: 1, IssuedAt: time.Now().UTC()})
	if !s.Has("m3", "t1") {
		t.Fatal("m3 should be revoked (TTL alive)")
	}
	// 手工过期
	time.Sleep(5 * time.Millisecond)
	s.mu.Lock()
	e := s.recs["m3"]
	e.ExpireAt = time.Now().UTC().Add(-time.Second)
	s.recs["m3"] = e
	s.mu.Unlock()

	purged := s.PurgeExpired()
	if purged == 0 {
		t.Fatal("expected purge >= 1")
	}
	if s.Has("m3", "t1") {
		t.Fatal("m3 should be purged")
	}
}
