// node_rpc 单测（R17 S3）：纯 gRPC 本地回环（127.0.0.1:0），不依赖真机网络。
// 覆盖：注册中心全方法、撤销流式广播（含租户隔离 / 全局 / 取消）、
// 预过滤、跨节点召回（含 Unimplemented）、profile 接线默认关（红线④）。
package kernel

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodepb "github.com/lightmem/kernel/proto"
)

// startTestNodeServer 在随机回环端口启动 gRPC 服务端，t.Cleanup 自动停止。
func startTestNodeServer(t *testing.T, opts ...NodeRPCServerOption) (*NodeRPCServer, string) {
	t.Helper()
	s := NewNodeRPCServer(opts...)
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start node rpc server: %v", err)
	}
	t.Cleanup(s.Stop)
	return s, addr
}

// staticPreFilter 测试用静态预过滤器。
type staticPreFilter struct {
	set map[string]bool
	err error
}

func (f *staticPreFilter) CandidateIDs(ctx context.Context, tenantID, queryText string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.set, nil
}

func TestGRPCNodeRegistryLoopback(t *testing.T) {
	_, addr := startTestNodeServer(t)
	r := NewGRPCNodeRegistry(addr)
	t.Cleanup(func() { _ = r.Close() })

	// Register 经 RPC 校验（空 ID / 空角色 → InvalidArgument）
	if err := r.Register(NodeInfo{}); err == nil {
		t.Fatal("expected register error for empty node")
	}
	if err := r.Register(NodeInfo{ID: "n1"}); err == nil {
		t.Fatal("expected register error for empty role")
	}

	if err := r.Register(NodeInfo{ID: "n1", Role: RoleWorker, Host: "10.0.10.11"}); err != nil {
		t.Fatalf("register n1: %v", err)
	}
	if err := r.Register(NodeInfo{ID: "n2", Role: RoleCoordinator, Host: "10.0.10.1"}); err != nil {
		t.Fatalf("register n2: %v", err)
	}

	// Get
	n, ok := r.Get("n1")
	if !ok || n.Role != RoleWorker || n.Host != "10.0.10.11" {
		t.Fatalf("get n1 mismatch: %#v %v", n, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("should not find unregistered node")
	}

	// List
	ws := r.List(RoleWorker)
	if len(ws) != 1 || ws[0].ID != "n1" {
		t.Fatalf("list workers: %#v", ws)
	}
	all := r.List("")
	if len(all) != 2 {
		t.Fatalf("list all: %#v", all)
	}

	// Heartbeat
	hbAt := time.Now().UTC().Add(-time.Second)
	if err := r.Heartbeat(Heartbeat{NodeID: "n1", At: hbAt}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	n, _ = r.Get("n1")
	if n == nil || n.LastSeen.IsZero() {
		t.Fatalf("last_seen should be updated: %#v", n)
	}
	if err := r.Heartbeat(Heartbeat{NodeID: "nope"}); err == nil {
		t.Fatal("expected heartbeat error for unknown node")
	} else if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v (%v)", status.Code(err), err)
	}

	// PurgeStale：注册一个 2h 未心跳的节点
	if err := r.Register(NodeInfo{ID: "n3", Role: RoleWorker, LastSeen: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("register n3: %v", err)
	}
	removed := r.PurgeStale(time.Hour)
	if len(removed) != 1 || removed[0] != "n3" {
		t.Fatalf("purge expected [n3], got %v", removed)
	}
	n, ok = r.Get("n3")
	if !ok || n.Status != StatusOffline {
		t.Fatalf("n3 should be offline: %#v %v", n, ok)
	}

	// Unregister
	if err := r.Unregister("nope"); err == nil {
		t.Fatal("expected unregister error for unknown node")
	} else if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", status.Code(err))
	}
	if err := r.Unregister("n1"); err != nil {
		t.Fatalf("unregister n1: %v", err)
	}
	if _, ok := r.Get("n1"); ok {
		t.Fatal("n1 should be removed after unregister")
	}
}

func TestGRPCNodeRegistryEmptyAddr(t *testing.T) {
	r := NewGRPCNodeRegistry("")
	if err := r.Register(NodeInfo{ID: "n1", Role: RoleWorker}); err == nil {
		t.Fatal("expected error when coordinator addr empty")
	}
	if _, ok := r.Get("n1"); ok {
		t.Fatal("get should fail with empty coordinator")
	}
	if got := r.List(""); got != nil {
		t.Fatalf("list should be nil with empty coordinator: %#v", got)
	}
	if got := r.PurgeStale(time.Second); got != nil {
		t.Fatalf("purge should be nil with empty coordinator: %#v", got)
	}
}

func TestGRPCBroadcasterLoopback(t *testing.T) {
	_, addr := startTestNodeServer(t)
	sub := NewGRPCBroadcaster(addr)
	t.Cleanup(func() { _ = sub.Close() })
	pub := NewGRPCBroadcaster(addr)
	t.Cleanup(func() { _ = pub.Close() })

	ch1 := make(chan Revocation, 8)
	cancel1 := sub.Subscribe("t1", func(r Revocation) { ch1 <- r })
	defer cancel1()

	chGlobal := make(chan Revocation, 8)
	cancelGlobal := sub.Subscribe("", func(r Revocation) { chGlobal <- r })
	defer cancelGlobal()

	// 等待流式订阅真正建立（回环下 500ms 足够）
	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()

	// 广播到 t1：t1 订阅者 + 全局订阅者都收到
	if err := pub.Revoke(ctx, Revocation{MemoryIDs: []string{"m1"}, TenantID: "t1", IssuedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("revoke m1: %v", err)
	}
	got := recvRev(t, ch1, "m1")
	if got.TenantID != "t1" {
		t.Fatalf("tenant mismatch: %#v", got)
	}
	recvRev(t, chGlobal, "m1")

	// 广播到 t2：t1 订阅者不应收到，全局订阅者收到
	if err := pub.Revoke(ctx, Revocation{MemoryIDs: []string{"m2"}, TenantID: "t2", IssuedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("revoke m2: %v", err)
	}
	recvRev(t, chGlobal, "m2")
	assertNoRev(t, ch1, 300*time.Millisecond)

	// 全局撤销（空租户）：所有订阅者都收到
	if err := pub.Revoke(ctx, Revocation{MemoryIDs: []string{"m3"}, IssuedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("revoke m3: %v", err)
	}
	recvRev(t, ch1, "m3")
	recvRev(t, chGlobal, "m3")

	// 取消后不再收到
	cancel1()
	cancelGlobal()
	time.Sleep(200 * time.Millisecond)
	if err := pub.Revoke(ctx, Revocation{MemoryIDs: []string{"m4"}, TenantID: "t1", IssuedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("revoke m4: %v", err)
	}
	assertNoRev(t, ch1, 300*time.Millisecond)
	assertNoRev(t, chGlobal, 300*time.Millisecond)
}

func recvRev(t *testing.T, ch <-chan Revocation, wantMemID string) Revocation {
	t.Helper()
	select {
	case r := <-ch:
		if len(r.MemoryIDs) == 0 || r.MemoryIDs[0] != wantMemID {
			t.Fatalf("expected revocation %s, got %#v", wantMemID, r)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for revocation %s", wantMemID)
		return Revocation{}
	}
}

func assertNoRev(t *testing.T, ch <-chan Revocation, wait time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected revocation: %#v", r)
	case <-time.After(wait):
	}
}

func TestGRPCPreFilterLoopback(t *testing.T) {
	// 启用静态预过滤的服务端
	_, addr := startTestNodeServer(t, WithPreFilter(&staticPreFilter{
		set: map[string]bool{"m1": true, "m2": true},
	}))
	f := NewGRPCPreFilter(addr)
	t.Cleanup(func() { _ = f.Close() })

	ids, err := f.CandidateIDs(context.Background(), "t1", "query")
	if err != nil {
		t.Fatalf("prefilter: %v", err)
	}
	if len(ids) != 2 || !ids["m1"] || !ids["m2"] {
		t.Fatalf("candidate set mismatch: %#v", ids)
	}

	// 默认 NoopPreFilter 服务端：返回 nil（全量放行）
	_, noopAddr := startTestNodeServer(t)
	noop := NewGRPCPreFilter(noopAddr)
	t.Cleanup(func() { _ = noop.Close() })
	ids, err = noop.CandidateIDs(context.Background(), "t1", "query")
	if err != nil {
		t.Fatalf("noop prefilter: %v", err)
	}
	if ids != nil {
		t.Fatalf("noop prefilter should return nil set: %#v", ids)
	}
}

func TestGRPCQueryLoopback(t *testing.T) {
	occurred := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	superseded := "01HNEWVER"
	var gotParams QueryParams
	_, addr := startTestNodeServer(t, WithQueryHandler(func(ctx context.Context, q QueryParams) ([]QueryResult, error) {
		gotParams = q
		return []QueryResult{
			{
				MemoryID:     "m1",
				Fact:         "fact-1",
				Score:        0.91,
				Source:       "test",
				OccurredAt:   &occurred,
				RecordedAt:   time.Now().UTC(),
				SupersededBy: &superseded,
				ValidFrom:    "2026-10-01",
			},
			{MemoryID: "m2", Fact: "fact-2", Score: 0.42, RecordedAt: time.Now().UTC()},
		}, nil
	}))
	r := NewGRPCNodeRegistry(addr)
	t.Cleanup(func() { _ = r.Close() })

	q := QueryParams{
		TenantID:    "t1",
		Viewer:      "alice",
		UserID:      "u1",
		Text:        "hello",
		TopK:        5,
		MemoryKinds: []MemoryKind{KindFact, KindEvent},
		Visibility:  []Visibility{VisTenant},
	}
	results, err := r.QueryRemote(context.Background(), addr, q)
	if err != nil {
		t.Fatalf("query remote: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %#v", len(results), results)
	}
	r1 := results[0]
	if r1.MemoryID != "m1" || r1.Fact != "fact-1" || r1.Score != 0.91 || r1.Source != "test" {
		t.Fatalf("result1 mismatch: %#v", r1)
	}
	if r1.OccurredAt == nil || !r1.OccurredAt.Equal(occurred) {
		t.Fatalf("occurred_at mismatch: %#v", r1.OccurredAt)
	}
	if r1.SupersededBy == nil || *r1.SupersededBy != superseded {
		t.Fatalf("superseded_by mismatch: %#v", r1.SupersededBy)
	}
	if r1.ValidFrom != "2026-10-01" {
		t.Fatalf("valid_from mismatch: %#v", r1)
	}

	// 服务端收到的参数完整
	if gotParams.TenantID != "t1" || gotParams.Viewer != "alice" || gotParams.Text != "hello" ||
		gotParams.TopK != 5 || len(gotParams.MemoryKinds) != 2 || len(gotParams.Visibility) != 1 {
		t.Fatalf("server received bad params: %#v", gotParams)
	}

	// 未配置召回处理器的节点 → Unimplemented
	_, noopAddr := startTestNodeServer(t)
	r2 := NewGRPCNodeRegistry(noopAddr)
	t.Cleanup(func() { _ = r2.Close() })
	_, err = r2.QueryRemote(context.Background(), noopAddr, QueryParams{Text: "x"})
	if err == nil {
		t.Fatal("expected Unimplemented error")
	}
	if code := status.Code(err); code != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v (%v)", code, err)
	}
}

func TestGRPCRevokeRemote(t *testing.T) {
	s, addr := startTestNodeServer(t)
	// 服务端本地订阅（内存广播器），验证 RevokeRemote 扇出
	ch := make(chan Revocation, 4)
	cancel := s.Broadcaster().Subscribe("t1", func(r Revocation) { ch <- r })
	defer cancel()

	r := NewGRPCNodeRegistry(addr)
	t.Cleanup(func() { _ = r.Close() })
	err := r.RevokeRemote(context.Background(), addr, Revocation{
		MemoryIDs: []string{"m9"},
		TenantID:  "t1",
		Reason:    "gdpr",
		TTL:       60,
		IssuedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("revoke remote: %v", err)
	}
	got := recvRev(t, ch, "m9")
	if got.Reason != "gdpr" || got.TTL != 60 {
		t.Fatalf("revocation fields mismatch: %#v", got)
	}
}

func TestNodeRPCWiringDefaultOff(t *testing.T) {
	// 默认关：nil profile / features 缺失 / 开关 false 一律走 InMemory，
	// 不产生任何 gRPC 客户端（红线④）。
	cases := []*Profile{
		nil,
		{},
		{Features: &FeaturesConfig{}},
		{Features: &FeaturesConfig{CrossNodeRankMerge: false}},
	}
	for i, p := range cases {
		switch NewNodeRegistry(p).(type) {
		case *InMemoryNodeRegistry:
		default:
			t.Fatalf("case %d: expected InMemoryNodeRegistry for %#v", i, p)
		}
		switch NewCrossNodeBroadcaster(p).(type) {
		case *InMemoryBroadcaster:
		default:
			t.Fatalf("case %d: expected InMemoryBroadcaster for %#v", i, p)
		}
	}

	// 开启但无任何 coordinator 地址：gRPC 客户端，地址为空（懒连接）
	enabled := &Profile{Features: &FeaturesConfig{CrossNodeRankMerge: true}}
	reg := NewNodeRegistry(enabled)
	gr, ok := reg.(*GRPCNodeRegistry)
	if !ok {
		t.Fatalf("expected GRPCNodeRegistry when enabled, got %T", reg)
	}
	if gr.CoordinatorAddr() != "" {
		t.Fatalf("expected empty coordinator addr, got %q", gr.CoordinatorAddr())
	}
	_ = gr.Close()
	bc := NewCrossNodeBroadcaster(enabled).(*GRPCBroadcaster)
	if bc.addr != "" {
		t.Fatalf("expected empty broadcaster addr, got %q", bc.addr)
	}
	_ = bc.Close()

	// 开启且 network.roles.coordinator 指定 host：host + 默认端口
	t.Setenv(EnvNodeGRPCAddr, "")
	t.Setenv(EnvNodeGRPCPort, "")
	withNet := &Profile{
		Features: &FeaturesConfig{CrossNodeRankMerge: true},
		Network: &NetworkConfig{Roles: map[string][]RoleSpec{
			string(RoleCoordinator): {{Host: "10.0.10.1"}},
		}},
	}
	if got := CoordinatorGRPCAddr(withNet); got != "10.0.10.1:9443" {
		t.Fatalf("coordinator addr default port: got %q", got)
	}
	t.Setenv(EnvNodeGRPCPort, "19443")
	if got := CoordinatorGRPCAddr(withNet); got != "10.0.10.1:19443" {
		t.Fatalf("coordinator addr env port: got %q", got)
	}

	// env LIGHTMEM_NODE_GRPC_ADDR 最高优先级
	t.Setenv(EnvNodeGRPCAddr, "10.0.10.2:9443")
	reg2 := NewNodeRegistry(withNet).(*GRPCNodeRegistry)
	if reg2.CoordinatorAddr() != "10.0.10.2:9443" {
		t.Fatalf("env addr override: got %q", reg2.CoordinatorAddr())
	}
	_ = reg2.Close()
}

// 编译期保证三个 gRPC 实现满足既有接口（替换内存骨架的契约）。
var (
	_ NodeRegistry             = (*GRPCNodeRegistry)(nil)
	_ CrossNodeBroadcaster     = (*GRPCBroadcaster)(nil)
	_ PreFilter                = (*GRPCPreFilter)(nil)
	_ nodepb.NodeServiceServer = (*NodeRPCServer)(nil)
)

func TestGRPCServerStartTwiceRejected(t *testing.T) {
	s := NewNodeRPCServer()
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if addr == "" {
		t.Fatal("expected non-empty listen addr")
	}
	if _, err := s.Start("127.0.0.1:0"); err == nil {
		t.Fatal("expected second start to be rejected")
	}
	s.Stop()
	// Stop 幂等
	s.Stop()
}
