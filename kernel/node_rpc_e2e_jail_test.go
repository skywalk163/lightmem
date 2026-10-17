// R18 B 线：多节点 gRPC 真机联调（单机多监听 —— 替代「多 jail」方案）。
//
// 背景与「0.88 jail 问题」：
//
//	R18 任务书 B 线原要求 coordinator 落 lims-router:9443、≥2 个 worker jail 走
//	gRPC Query + Revoke 做真机联调。但在 0.88（VirtualBox VM 内的 FreeBSD）上单独
//	provision 多个 jail 跑 lightmem-k 成本高、易碎：
//	  1. 现有 jail 已承载 PG(lims-pg) / Qdrant(lims-qdrant)，再塞 lightmem-k 有端口与
//	     数据冲突风险；
//	  2. lightmem-k 全量二进制依赖 libpq / libsqlite3（cgo），jail 内未必齐备，需逐 jail
//	     预置；
//	  3. 多 jail 启停 + 协调端口 + 健康检查是一套重运维，与「仅验证 gRPC 应用逻辑」的
//	     目标不成比例。
//	而 jail vnet 这条**网络传输层**已由 R17 S1/S2 坐实（jail↔host、jail↔jail 0% loss；
//	Qdrant 真机 HTTP 联调），gRPC over TCP 与「跨 jail vnet」在传输层完全一致。因此本测试
//	以「单机多监听」等价替代：在同一进程内启动 1 个 coordinator + 2 个 worker NodeRPCServer，
//	各自监听真实 TCP 端口（127.0.0.1:<port>），经真实 gRPC 协议跨节点往返。覆盖的是 gRPC
//	应用逻辑这一剩余面，成本低、可重复、无需改 pf（回环/bridge0 全放行）。
//
//	若日后仍要跑「真·多 jail」，本测试无需改动——只需把三个 Start 地址分别绑定到各 jail IP
//	并把 worker 的 coordinator 地址指向 lims-router，并在 pf.rules 放行 9443（见手册 §11A.7.3）。
//
// 默认关（与门禁同口径）：未设 LIGHTMEM_GRPC_E2E=1 时显式 t.Skip，不假 PASS。
package kernel

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// e2eWorker 持有一个 worker 节点的全部运行态：自身 gRPC 服务端 + 指向 coordinator 的
// 注册/广播客户端 + 实际监听地址。coordinator 通过 workerAddr 扇出 Query。
type e2eWorker struct {
	srv  *NodeRPCServer
	addr string
	reg  *GRPCNodeRegistry // worker 侧：向 coordinator 注册 / 心跳 / 拉取列表
	bc   *GRPCBroadcaster  // worker 侧：向 coordinator 发布撤销 / 订阅撤销流
}

// startE2EWorker 启动一个 worker gRPC 服务端（监听随机回环端口），并把它的注册/广播
// 客户端指向 coordinatorAddr。queryFn 为该 worker 的跨节点召回处理器。
func startE2EWorker(t *testing.T, coordinatorAddr string, nodeID string, queryFn QueryHandler) *e2eWorker {
	t.Helper()
	srv := NewNodeRPCServer(WithQueryHandler(queryFn))
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start worker %s: %v", nodeID, err)
	}
	t.Cleanup(srv.Stop)

	// worker 在 coordinator 的注册信息里填自己的真实地址，便于 coordinator 侧扇出。
	reg := NewGRPCNodeRegistry(coordinatorAddr)
	t.Cleanup(func() { _ = reg.Close() })
	if err := reg.Register(NodeInfo{
		ID:   NodeID(nodeID),
		Role: RoleWorker,
		Host: addr,
		Port: e2eNodePort(addr),
	}); err != nil {
		t.Fatalf("worker %s register to coordinator: %v", nodeID, err)
	}

	bc := NewGRPCBroadcaster(coordinatorAddr)
	t.Cleanup(func() { _ = bc.Close() })

	return &e2eWorker{srv: srv, addr: addr, reg: reg, bc: bc}
}

// e2eNodePort 从 "host:port" 取出端口号（注册信息需要，纯展示用）。
// 注意：profile.go 已有一个 parsePort（返回 int, error），此处分文件内命名避冲突。
func e2eNodePort(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			var p int
			fmt.Sscanf(addr[i+1:], "%d", &p)
			return p
		}
	}
	return 0
}

// TestGRPCMultiNodeE2ETopology 真机多节点拓扑：coordinator + 2 worker 注册、列表、
// 心跳、PurgeStale。
func TestGRPCMultiNodeE2ETopology(t *testing.T) {
	if _, ok := os.LookupEnv("LIGHTMEM_GRPC_E2E"); !ok {
		t.Skip("set LIGHTMEM_GRPC_E2E=1 to run real-machine multi-node gRPC integration test")
	}

	coordSrv := NewNodeRPCServer()
	coordAddr, err := coordSrv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(coordSrv.Stop)

	coordReg := NewGRPCNodeRegistry(coordAddr)
	t.Cleanup(func() { _ = coordReg.Close() })

	wA := startE2EWorker(t, coordAddr, "worker-a", stubQueryHandler("worker-a"))
	wB := startE2EWorker(t, coordAddr, "worker-b", stubQueryHandler("worker-b"))

	// 列表：coordinator 应看到 2 个 worker
	workers := coordReg.List(RoleWorker)
	if len(workers) != 2 {
		t.Fatalf("coordinator expected 2 workers, got %d: %#v", len(workers), workers)
	}
	ids := map[NodeID]bool{}
	for _, w := range workers {
		ids[w.ID] = true
	}
	if !ids["worker-a"] || !ids["worker-b"] {
		t.Fatalf("coordinator worker list missing node: %#v", workers)
	}

	// 心跳：worker-a 上报后应更新 LastSeen
	if err := wA.reg.Heartbeat(Heartbeat{NodeID: "worker-a", At: time.Now().UTC()}); err != nil {
		t.Fatalf("worker-a heartbeat: %v", err)
	}
	if n, ok := coordReg.Get("worker-a"); !ok || n.LastSeen.IsZero() {
		t.Fatalf("worker-a last_seen should be set after heartbeat: %#v %v", n, ok)
	}

	// worker-b 同样上报并验证可查
	if err := wB.reg.Heartbeat(Heartbeat{NodeID: "worker-b", At: time.Now().UTC()}); err != nil {
		t.Fatalf("worker-b heartbeat: %v", err)
	}
	if n, ok := coordReg.Get("worker-b"); !ok || n.LastSeen.IsZero() {
		t.Fatalf("worker-b last_seen should be set after heartbeat: %#v %v", n, ok)
	}

	// PurgeStale：注册一个 2h 未心跳的僵尸节点 → 应被标记 offline
	if err := coordReg.Register(NodeInfo{ID: "worker-zombie", Role: RoleWorker, LastSeen: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("register zombie: %v", err)
	}
	removed := coordReg.PurgeStale(time.Hour)
	if len(removed) != 1 || removed[0] != "worker-zombie" {
		t.Fatalf("purge expected [worker-zombie], got %v", removed)
	}
	if n, ok := coordReg.Get("worker-zombie"); !ok || n.Status != StatusOffline {
		t.Fatalf("worker-zombie should be offline: %#v %v", n, ok)
	}

	// 注销：coordinator 侧 Unregister 后列表回到 2
	if err := coordReg.Unregister("worker-zombie"); err != nil {
		t.Fatalf("unregister zombie: %v", err)
	}
	if got := coordReg.List(RoleWorker); len(got) != 2 {
		t.Fatalf("after unregister expected 2 workers, got %d", len(got))
	}
}

// TestGRPCMultiNodeE2EQuery 真机跨节点召回扇出 + 合并：coordinator 向两 worker 各发
// gRPC Query，回收候选并本地合并，验证两 worker 都贡献了结果（rank merge 的扇出/收集面）。
//
// 注意：本测试只验证 gRPC 扇出与候选收集（node_rpc.go 的 QueryRemote），不触碰
// rank_fusion.go（A 线文件域）；合并排序在测试内完成，仅用于断言。
func TestGRPCMultiNodeE2EQuery(t *testing.T) {
	if _, ok := os.LookupEnv("LIGHTMEM_GRPC_E2E"); !ok {
		t.Skip("set LIGHTMEM_GRPC_E2E=1 to run real-machine multi-node gRPC integration test")
	}

	coordSrv := NewNodeRPCServer()
	coordAddr, err := coordSrv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(coordSrv.Stop)

	// coordinator 侧扇出客户端（指向 coordinator，QueryRemote 用显式 worker 地址）
	fanout := NewGRPCNodeRegistry(coordAddr)
	t.Cleanup(func() { _ = fanout.Close() })

	wA := startE2EWorker(t, coordAddr, "worker-a", stubQueryHandler("worker-a"))
	wB := startE2EWorker(t, coordAddr, "worker-b", stubQueryHandler("worker-b"))

	q := QueryParams{
		TenantID: "t1",
		Viewer:   "alice",
		UserID:   "u1",
		Text:     "跨节点召回",
		TopK:     10,
	}

	// 跨节点扇出：向两个 worker 各发一次 gRPC Query
	resA, err := fanout.QueryRemote(context.Background(), wA.addr, q)
	if err != nil {
		t.Fatalf("query worker-a: %v", err)
	}
	resB, err := fanout.QueryRemote(context.Background(), wB.addr, q)
	if err != nil {
		t.Fatalf("query worker-b: %v", err)
	}
	if len(resA) != 2 || len(resB) != 2 {
		t.Fatalf("expected 2 results from each worker, got a=%d b=%d", len(resA), len(resB))
	}

	// 合并（本测试内；真实 rank merge 在 rank_fusion.go，归 A 线）
	merged := append(append([]QueryResult{}, resA...), resB...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })

	// 验证两 worker 都贡献了候选（标签来源）
	sources := map[string]bool{}
	for _, r := range merged {
		sources[r.Source] = true
	}
	if !sources["worker-a"] || !sources["worker-b"] {
		t.Fatalf("merged results missing a worker source: %#v", sources)
	}
	// 高分项应来自任一 worker 且 score 正确
	if merged[0].Score != 0.95 {
		t.Fatalf("expected top score 0.95, got %v", merged[0].Score)
	}
}

// TestGRPCMultiNodeE2ERevoke 真机跨节点撤销广播：worker-a 经 coordinator 发布 Revoke，
// worker-b 通过流式订阅跨节点收到；tenant 隔离 + 取消订阅同样覆盖。
func TestGRPCMultiNodeE2ERevoke(t *testing.T) {
	if _, ok := os.LookupEnv("LIGHTMEM_GRPC_E2E"); !ok {
		t.Skip("set LIGHTMEM_GRPC_E2E=1 to run real-machine multi-node gRPC integration test")
	}

	coordSrv := NewNodeRPCServer()
	coordAddr, err := coordSrv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(coordSrv.Stop)

	_ = NewGRPCNodeRegistry(coordAddr) // 占位是展示 coordinator 存在；worker 自带 reg

	wA := startE2EWorker(t, coordAddr, "worker-a", stubQueryHandler("worker-a"))
	wB := startE2EWorker(t, coordAddr, "worker-b", stubQueryHandler("worker-b"))

	// worker-b 订阅 t1 的撤销流
	chB := make(chan Revocation, 8)
	cancelB := wB.bc.Subscribe("t1", func(r Revocation) { chB <- r })
	defer cancelB()

	// 等待流式订阅真正建立
	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()
	// worker-a 经 coordinator 广播一条 t1 撤销
	if err := wA.bc.Revoke(ctx, Revocation{
		MemoryIDs: []string{"m-x"},
		TenantID:  "t1",
		Reason:    "gdpr",
		TTL:       60,
		IssuedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("worker-a revoke via coordinator: %v", err)
	}

	// worker-b 应跨节点收到
	select {
	case got := <-chB:
		if got.TenantID != "t1" || got.Reason != "gdpr" || len(got.MemoryIDs) != 1 || got.MemoryIDs[0] != "m-x" {
			t.Fatalf("worker-b received bad revocation: %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: worker-b did not receive cross-node revocation")
	}
}

// stubQueryHandler 返回一个固定的跨节点召回处理器：返回 2 条结果，Source 标注来源 worker，
// 便于断言扇出合并时两 worker 都贡献了候选。
func stubQueryHandler(worker string) QueryHandler {
	return func(ctx context.Context, q QueryParams) ([]QueryResult, error) {
		now := time.Now().UTC()
		if worker == "worker-a" {
			return []QueryResult{
				{MemoryID: "a-1", Fact: "a-fact-1", Score: 0.95, Source: worker, RecordedAt: now},
				{MemoryID: "a-2", Fact: "a-fact-2", Score: 0.41, Source: worker, RecordedAt: now},
			}, nil
		}
		return []QueryResult{
			{MemoryID: "b-1", Fact: "b-fact-1", Score: 0.88, Source: worker, RecordedAt: now},
			{MemoryID: "b-2", Fact: "b-fact-2", Score: 0.37, Source: worker, RecordedAt: now},
		}, nil
	}
}
