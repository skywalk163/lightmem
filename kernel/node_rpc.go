// 跨节点 gRPC 真实现（R17 S3，§5.4 联邦 / 多节点）。
//
// 本文件是 R16 stdlib-only 约束的唯一例外（R17 红线⑦）：引入
// google.golang.org/grpc + google.golang.org/protobuf，把 R16 的内存骨架
// （node_role.go 的 InMemoryNodeRegistry / InMemoryBroadcaster / NoopPreFilter）
// 替换为真实 gRPC 通信。
//
// 红线遵守：
//   - 只增不改（红线①）：model.go / storage.go / lifecycle.go / compliance.go
//     未改；本文件只新增，未改既有接口语义。
//   - 接线默认关（红线④）：gRPC 由 profile.features.cross_node_rank_merge
//     控制，默认 false；装配函数见 profile.go 的 NewNodeRegistry /
//     NewCrossNodeBroadcaster，默认关时不创建任何 gRPC 连接。
//   - 安全：默认明文（jail vnet 内网 + pf/IPsec 防护，见手册 §11A.7）；
//     可通过 WithDialOptions / WithGRPCServerOptions 注入 TLS 凭证。
package kernel

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	nodepb "github.com/lightmem/kernel/proto"
)

// DefaultNodeGRPCPort 联邦节点 gRPC 默认监听端口（R17 S3）。
// 部署在 jail vnet 内网（10.0.10.0/24），pf 仅对内网 / 管理网段放行；
// 见手册 §11A.7。
const DefaultNodeGRPCPort = 9443

// gRPC 相关环境变量（与 profile env 覆盖同口径，不写进 profile 真源）。
const (
	// EnvNodeGRPCAddr 直接指定 coordinator 的 host:port（最高优先级）。
	EnvNodeGRPCAddr = "LIGHTMEM_NODE_GRPC_ADDR"
	// EnvNodeGRPCPort 覆盖默认 gRPC 端口（与 network.roles.coordinator 组合）。
	EnvNodeGRPCPort = "LIGHTMEM_NODE_GRPC_PORT"
)

// rpcTimeout 无 ctx 接口方法（NodeRegistry）的默认 RPC 超时。
const rpcTimeout = 10 * time.Second

// rpcTimeFormat 线上时间字段统一用 RFC3339Nano UTC（与 kernel 日期约定一致）。
const rpcTimeFormat = time.RFC3339Nano

func rpcFormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(rpcTimeFormat)
}

func rpcParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(rpcTimeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// ---------------------------------------------------------------------
// 消息转换（domain <-> nodepb）
// ---------------------------------------------------------------------

func nodeInfoToMsg(n *NodeInfo) *nodepb.NodeInfoMsg {
	return &nodepb.NodeInfoMsg{
		Id:           string(n.ID),
		Role:         string(n.Role),
		Host:         n.Host,
		Port:         int32(n.Port),
		Status:       string(n.Status),
		TenantShards: append([]string(nil), n.TenantShards...),
		LastSeen:     rpcFormatTime(n.LastSeen),
		Load:         n.Load,
	}
}

func msgToNodeInfo(m *nodepb.NodeInfoMsg) NodeInfo {
	return NodeInfo{
		ID:           NodeID(m.Id),
		Role:         NodeRole(m.Role),
		Host:         m.Host,
		Port:         int(m.Port),
		Status:       NodeStatus(m.Status),
		TenantShards: append([]string(nil), m.TenantShards...),
		LastSeen:     rpcParseTime(m.LastSeen),
		Load:         m.Load,
	}
}

func revToMsg(r Revocation) *nodepb.RevokeMsg {
	return &nodepb.RevokeMsg{
		MemoryIds:  append([]string(nil), r.MemoryIDs...),
		TenantId:   r.TenantID,
		Reason:     r.Reason,
		IssuedAt:   rpcFormatTime(r.IssuedAt),
		TtlSeconds: int32(r.TTL),
	}
}

func msgToRev(m *nodepb.RevokeMsg) Revocation {
	return Revocation{
		MemoryIDs: append([]string(nil), m.MemoryIds...),
		TenantID:  m.TenantId,
		Reason:    m.Reason,
		IssuedAt:  rpcParseTime(m.IssuedAt),
		TTL:       int(m.TtlSeconds),
	}
}

func queryResultToMsg(r *QueryResult) *nodepb.QueryResultMsg {
	m := &nodepb.QueryResultMsg{
		MemoryId:     r.MemoryID,
		Fact:         r.Fact,
		Score:        r.Score,
		Source:       r.Source,
		RecordedAt:   rpcFormatTime(r.RecordedAt),
		ValidFrom:    r.ValidFrom,
		ValidUntil:   r.ValidUntil,
		IsHistorical: r.IsHistorical,
	}
	if r.OccurredAt != nil {
		m.OccurredAt = rpcFormatTime(*r.OccurredAt)
	}
	if r.SupersededBy != nil {
		m.SupersededBy = *r.SupersededBy
	}
	return m
}

func msgToQueryResult(m *nodepb.QueryResultMsg) QueryResult {
	q := QueryResult{
		MemoryID:     m.MemoryId,
		Fact:         m.Fact,
		Score:        m.Score,
		Source:       m.Source,
		RecordedAt:   rpcParseTime(m.RecordedAt),
		ValidFrom:    m.ValidFrom,
		ValidUntil:   m.ValidUntil,
		IsHistorical: m.IsHistorical,
	}
	if m.OccurredAt != "" {
		t := rpcParseTime(m.OccurredAt)
		q.OccurredAt = &t
	}
	if m.SupersededBy != "" {
		s := m.SupersededBy
		q.SupersededBy = &s
	}
	return q
}

func msgToQueryParams(req *nodepb.QueryReq) QueryParams {
	q := QueryParams{
		TenantID: req.TenantId,
		Viewer:   req.Viewer,
		UserID:   req.UserId,
		Text:     req.Text,
		Vector:   append([]float32(nil), req.Vector...),
		TopK:     int(req.TopK),
	}
	for _, k := range req.MemoryKinds {
		q.MemoryKinds = append(q.MemoryKinds, MemoryKind(k))
	}
	for _, v := range req.Visibility {
		q.Visibility = append(q.Visibility, Visibility(v))
	}
	return q
}

// ---------------------------------------------------------------------
// 服务端
// ---------------------------------------------------------------------

// QueryHandler 服务端跨节点召回处理器（由装配层注入，通常闭包 StorageBackend.Query）。
// 未注入时 Query RPC 返回 codes.Unimplemented。
type QueryHandler func(ctx context.Context, q QueryParams) ([]QueryResult, error)

// NodeRPCServer gRPC 服务端：把节点注册中心 / 撤销广播 / 预过滤 / 召回
// 暴露给联邦内其他节点。默认实现全部复用 R16 内存组件，单进程测试
// 与“单节点兼任 coordinator”部署均可直接使用。
type NodeRPCServer struct {
	nodepb.UnimplementedNodeServiceServer

	registry    NodeRegistry
	broadcaster CrossNodeBroadcaster
	prefilter   PreFilter
	query       QueryHandler

	serverOpts []grpc.ServerOption

	mu   sync.Mutex
	srv  *grpc.Server
	lis  net.Listener
	addr string
}

// NodeRPCServerOption 服务端装配选项。
type NodeRPCServerOption func(*NodeRPCServer)

// WithNodeRegistry 注入注册中心（默认 NewInMemoryNodeRegistry）。
func WithNodeRegistry(r NodeRegistry) NodeRPCServerOption {
	return func(s *NodeRPCServer) { s.registry = r }
}

// WithCrossNodeBroadcaster 注入广播器（默认 NewInMemoryBroadcaster）。
func WithCrossNodeBroadcaster(b CrossNodeBroadcaster) NodeRPCServerOption {
	return func(s *NodeRPCServer) { s.broadcaster = b }
}

// WithPreFilter 注入预过滤器（默认 NoopPreFilter，全部放行）。
func WithPreFilter(pf PreFilter) NodeRPCServerOption {
	return func(s *NodeRPCServer) { s.prefilter = pf }
}

// WithQueryHandler 注入跨节点召回处理器（默认未配置 → Unimplemented）。
func WithQueryHandler(h QueryHandler) NodeRPCServerOption {
	return func(s *NodeRPCServer) { s.query = h }
}

// WithGRPCServerOptions 透传 grpc.ServerOption（如 TLS 凭证 / 拦截器）。
func WithGRPCServerOptions(opts ...grpc.ServerOption) NodeRPCServerOption {
	return func(s *NodeRPCServer) { s.serverOpts = append(s.serverOpts, opts...) }
}

// NewNodeRPCServer 构造 gRPC 服务端（尚未监听，调用 Start 才开始服务）。
func NewNodeRPCServer(opts ...NodeRPCServerOption) *NodeRPCServer {
	s := &NodeRPCServer{
		registry:    NewInMemoryNodeRegistry(),
		broadcaster: NewInMemoryBroadcaster(),
		prefilter:   NoopPreFilter{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Registry 返回服务端持有的注册中心（装配 / 测试用）。
func (s *NodeRPCServer) Registry() NodeRegistry { return s.registry }

// Broadcaster 返回服务端持有的广播器（装配 / 测试用）。
func (s *NodeRPCServer) Broadcaster() CrossNodeBroadcaster { return s.broadcaster }

// Start 监听 addr（如 ":9443" / "127.0.0.1:0"）并异步开始服务，
// 返回实际监听地址。重复调用返回错误；Stop 可优雅停止。
func (s *NodeRPCServer) Start(addr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return "", fmt.Errorf("node rpc server already started on %s", s.addr)
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("node rpc listen %s: %w", addr, err)
	}
	srv := grpc.NewServer(s.serverOpts...)
	nodepb.RegisterNodeServiceServer(srv, s)
	s.lis = lis
	s.srv = srv
	s.addr = lis.Addr().String()
	go func() { _ = srv.Serve(lis) }()
	return s.addr, nil
}

// Addr 返回实际监听地址（Start 之后有效）。
func (s *NodeRPCServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop 优雅停止服务（未启动则空操作）。
func (s *NodeRPCServer) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv != nil {
		srv.GracefulStop()
	}
}

// ---- 注册中心 RPC ----

func (s *NodeRPCServer) RegisterNode(ctx context.Context, m *nodepb.NodeInfoMsg) (*nodepb.Empty, error) {
	if err := s.registry.Register(msgToNodeInfo(m)); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "register node: %v", err)
	}
	return &nodepb.Empty{}, nil
}

func (s *NodeRPCServer) UnregisterNode(ctx context.Context, m *nodepb.NodeIDMsg) (*nodepb.Empty, error) {
	if err := s.registry.Unregister(NodeID(m.Id)); err != nil {
		return nil, status.Errorf(codes.NotFound, "unregister node: %v", err)
	}
	return &nodepb.Empty{}, nil
}

func (s *NodeRPCServer) Heartbeat(ctx context.Context, req *nodepb.HeartbeatReq) (*nodepb.HeartbeatResp, error) {
	if err := s.registry.Heartbeat(Heartbeat{
		NodeID: NodeID(req.NodeId),
		At:     rpcParseTime(req.At),
	}); err != nil {
		return nil, status.Errorf(codes.NotFound, "heartbeat: %v", err)
	}
	return &nodepb.HeartbeatResp{ServerAt: rpcFormatTime(time.Now().UTC())}, nil
}

func (s *NodeRPCServer) GetNode(ctx context.Context, m *nodepb.NodeIDMsg) (*nodepb.GetNodeResp, error) {
	n, ok := s.registry.Get(NodeID(m.Id))
	if !ok {
		return &nodepb.GetNodeResp{Found: false}, nil
	}
	return &nodepb.GetNodeResp{Found: true, Node: nodeInfoToMsg(n)}, nil
}

func (s *NodeRPCServer) ListNodes(ctx context.Context, req *nodepb.ListNodesReq) (*nodepb.ListNodesResp, error) {
	nodes := s.registry.List(NodeRole(req.Role))
	resp := &nodepb.ListNodesResp{Nodes: make([]*nodepb.NodeInfoMsg, 0, len(nodes))}
	for _, n := range nodes {
		resp.Nodes = append(resp.Nodes, nodeInfoToMsg(n))
	}
	return resp, nil
}

func (s *NodeRPCServer) PurgeStale(ctx context.Context, req *nodepb.PurgeStaleReq) (*nodepb.PurgeStaleResp, error) {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	ids := s.registry.PurgeStale(timeout)
	resp := &nodepb.PurgeStaleResp{NodeIds: make([]string, 0, len(ids))}
	for _, id := range ids {
		resp.NodeIds = append(resp.NodeIds, string(id))
	}
	return resp, nil
}

// ---- 撤销广播 RPC ----

func (s *NodeRPCServer) Revoke(ctx context.Context, m *nodepb.RevokeMsg) (*nodepb.Empty, error) {
	if err := s.broadcaster.Revoke(ctx, msgToRev(m)); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke: %v", err)
	}
	return &nodepb.Empty{}, nil
}

// RevocationStream 服务端流式订阅：订阅语义与 InMemoryBroadcaster 一致
// （空租户 = 全局订阅）。事件经缓冲 channel 转交，回调不阻塞广播方；
// 客户端断开（ctx.Done）时正常结束，不返回错误。
func (s *NodeRPCServer) RevocationStream(req *nodepb.StreamReq, stream nodepb.NodeService_RevocationStreamServer) error {
	ctx := stream.Context()
	ch := make(chan *nodepb.RevokeMsg, 32)
	cancel := s.broadcaster.Subscribe(req.Tenant, func(r Revocation) {
		select {
		case ch <- revToMsg(r):
		case <-ctx.Done():
		}
	})
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-ch:
			if err := stream.Send(m); err != nil {
				return err
			}
		}
	}
}

// ---- 预过滤 / 召回 RPC ----

func (s *NodeRPCServer) PreFilter(ctx context.Context, req *nodepb.PreFilterReq) (*nodepb.PreFilterResp, error) {
	ids, err := s.prefilter.CandidateIDs(ctx, req.TenantId, req.QueryText)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "prefilter: %v", err)
	}
	resp := &nodepb.PreFilterResp{}
	if ids != nil {
		resp.Enabled = true
		resp.CandidateIds = make([]string, 0, len(ids))
		for id := range ids {
			resp.CandidateIds = append(resp.CandidateIds, id)
		}
		sort.Strings(resp.CandidateIds)
	}
	return resp, nil
}

func (s *NodeRPCServer) Query(ctx context.Context, req *nodepb.QueryReq) (*nodepb.QueryResp, error) {
	if s.query == nil {
		return nil, status.Error(codes.Unimplemented, "node query handler not configured")
	}
	results, err := s.query(ctx, msgToQueryParams(req))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query: %v", err)
	}
	resp := &nodepb.QueryResp{Results: make([]*nodepb.QueryResultMsg, 0, len(results))}
	for i := range results {
		resp.Results = append(resp.Results, queryResultToMsg(&results[i]))
	}
	return resp, nil
}

// ---------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------

// GRPCOption 客户端装配选项。
type GRPCOption func(*grpcOptions)

type grpcOptions struct {
	dialOpts []grpc.DialOption
}

// WithDialOptions 透传 grpc.DialOption（如 TLS 凭证 / 拦截器）。
// 未指定时默认使用 insecure 明文凭证（仅适用于 pf/IPsec 防护的 jail 内网）。
func WithDialOptions(opts ...grpc.DialOption) GRPCOption {
	return func(o *grpcOptions) { o.dialOpts = append(o.dialOpts, opts...) }
}

func buildGRPCOptions(opts []GRPCOption) *grpcOptions {
	o := &grpcOptions{
		dialOpts: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// GRPCNodeRegistry 实现 NodeRegistry 接口：注册中心调用全部转发到
// coordinator gRPC 端点；同时维护到各 worker 节点的懒连接池，
// 供跨节点 Query / PreFilter / Revoke 扇出。
type GRPCNodeRegistry struct {
	coordinatorAddr string
	dialOpts        []grpc.DialOption

	mu   sync.Mutex
	pool map[string]*grpc.ClientConn
}

// NewGRPCNodeRegistry 构造客户端注册中心（懒连接，首次 RPC 才拨号）。
// coordinatorAddr 为空时，注册中心类方法会返回明确错误（跨节点扇出
// 方法仍可用，因为显式传入了 peer 地址）。
func NewGRPCNodeRegistry(coordinatorAddr string, opts ...GRPCOption) *GRPCNodeRegistry {
	o := buildGRPCOptions(opts)
	return &GRPCNodeRegistry{
		coordinatorAddr: coordinatorAddr,
		dialOpts:        o.dialOpts,
		pool:            map[string]*grpc.ClientConn{},
	}
}

// CoordinatorAddr 返回配置的 coordinator 端点（可能为空）。
func (r *GRPCNodeRegistry) CoordinatorAddr() string { return r.coordinatorAddr }

// clientFor 返回（或懒建立并缓存）到 addr 的 NodeServiceClient。
func (r *GRPCNodeRegistry) clientFor(addr string) (nodepb.NodeServiceClient, error) {
	if addr == "" {
		return nil, fmt.Errorf("node rpc peer address is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if conn, ok := r.pool[addr]; ok {
		return nodepb.NewNodeServiceClient(conn), nil
	}
	conn, err := grpc.NewClient(addr, r.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial node rpc %s: %w", addr, err)
	}
	r.pool[addr] = conn
	return nodepb.NewNodeServiceClient(conn), nil
}

func (r *GRPCNodeRegistry) coordinator() (nodepb.NodeServiceClient, error) {
	return r.clientFor(r.coordinatorAddr)
}

// Register 注册节点（转发 coordinator，校验失败返回 InvalidArgument）。
func (r *GRPCNodeRegistry) Register(n NodeInfo) error {
	c, err := r.coordinator()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = c.RegisterNode(ctx, nodeInfoToMsg(&n))
	return err
}

// Unregister 注销节点（未知节点返回 NotFound）。
func (r *GRPCNodeRegistry) Unregister(id NodeID) error {
	c, err := r.coordinator()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = c.UnregisterNode(ctx, &nodepb.NodeIDMsg{Id: string(id)})
	return err
}

// Get 取节点快照（不存在返回 nil,false）。
func (r *GRPCNodeRegistry) Get(id NodeID) (*NodeInfo, bool) {
	c, err := r.coordinator()
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := c.GetNode(ctx, &nodepb.NodeIDMsg{Id: string(id)})
	if err != nil || !resp.Found || resp.Node == nil {
		return nil, false
	}
	n := msgToNodeInfo(resp.Node)
	return &n, true
}

// List 按角色列出节点（role 为空 = 全部）。
func (r *GRPCNodeRegistry) List(role NodeRole) []*NodeInfo {
	c, err := r.coordinator()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := c.ListNodes(ctx, &nodepb.ListNodesReq{Role: string(role)})
	if err != nil || resp == nil {
		return nil
	}
	out := make([]*NodeInfo, 0, len(resp.Nodes))
	for _, m := range resp.Nodes {
		n := msgToNodeInfo(m)
		out = append(out, &n)
	}
	return out
}

// Heartbeat 上报心跳。
func (r *GRPCNodeRegistry) Heartbeat(hb Heartbeat) error {
	c, err := r.coordinator()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = c.Heartbeat(ctx, &nodepb.HeartbeatReq{
		NodeId: string(hb.NodeID),
		At:     rpcFormatTime(hb.At),
	})
	return err
}

// PurgeStale 要求 coordinator 剔除超时节点，返回被标记 offline 的节点 ID。
func (r *GRPCNodeRegistry) PurgeStale(timeout time.Duration) []NodeID {
	c, err := r.coordinator()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := c.PurgeStale(ctx, &nodepb.PurgeStaleReq{
		TimeoutSeconds: int64(timeout / time.Second),
	})
	if err != nil || resp == nil {
		return nil
	}
	out := make([]NodeID, 0, len(resp.NodeIds))
	for _, id := range resp.NodeIds {
		out = append(out, NodeID(id))
	}
	return out
}

// Close 关闭连接池中的全部 gRPC 连接。
func (r *GRPCNodeRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for addr, conn := range r.pool {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(r.pool, addr)
	}
	return firstErr
}

// QueryRemote 向指定 worker 节点发起跨节点召回（rank merge 的远端输入）。
func (r *GRPCNodeRegistry) QueryRemote(ctx context.Context, addr string, q QueryParams) ([]QueryResult, error) {
	c, err := r.clientFor(addr)
	if err != nil {
		return nil, err
	}
	req := &nodepb.QueryReq{
		TenantId:    q.TenantID,
		Viewer:      q.Viewer,
		UserId:      q.UserID,
		Text:        q.Text,
		Vector:      q.Vector,
		TopK:        int32(q.TopK),
		MemoryKinds: make([]string, 0, len(q.MemoryKinds)),
		Visibility:  make([]string, 0, len(q.Visibility)),
	}
	for _, k := range q.MemoryKinds {
		req.MemoryKinds = append(req.MemoryKinds, string(k))
	}
	for _, v := range q.Visibility {
		req.Visibility = append(req.Visibility, string(v))
	}
	resp, err := c.Query(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]QueryResult, 0, len(resp.Results))
	for _, m := range resp.Results {
		out = append(out, msgToQueryResult(m))
	}
	return out, nil
}

// PreFilterRemote 向指定节点请求预过滤候选集。
// 服务端未启用（NoopPreFilter）时返回 nil 集合，调用方按全量放行。
func (r *GRPCNodeRegistry) PreFilterRemote(ctx context.Context, addr, tenantID, queryText string) (map[string]bool, error) {
	c, err := r.clientFor(addr)
	if err != nil {
		return nil, err
	}
	resp, err := c.PreFilter(ctx, &nodepb.PreFilterReq{
		TenantId:  tenantID,
		QueryText: queryText,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || !resp.Enabled {
		return nil, nil
	}
	set := make(map[string]bool, len(resp.CandidateIds))
	for _, id := range resp.CandidateIds {
		set[id] = true
	}
	return set, nil
}

// RevokeRemote 向指定节点扇出一条撤销事件。
func (r *GRPCNodeRegistry) RevokeRemote(ctx context.Context, addr string, rev Revocation) error {
	c, err := r.clientFor(addr)
	if err != nil {
		return err
	}
	_, err = c.Revoke(ctx, revToMsg(rev))
	return err
}

// GRPCBroadcaster 实现 CrossNodeBroadcaster：Revoke 走 coordinator
// Unary 扇出，Subscribe 走服务端流式 RevocationStream。
type GRPCBroadcaster struct {
	addr     string
	dialOpts []grpc.DialOption

	mu   sync.Mutex
	conn *grpc.ClientConn
}

// NewGRPCBroadcaster 构造 gRPC 广播器客户端（懒连接）。
func NewGRPCBroadcaster(coordinatorAddr string, opts ...GRPCOption) *GRPCBroadcaster {
	o := buildGRPCOptions(opts)
	return &GRPCBroadcaster{addr: coordinatorAddr, dialOpts: o.dialOpts}
}

func (b *GRPCBroadcaster) client() (nodepb.NodeServiceClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		conn, err := grpc.NewClient(b.addr, b.dialOpts...)
		if err != nil {
			return nil, fmt.Errorf("dial node rpc %s: %w", b.addr, err)
		}
		b.conn = conn
	}
	return nodepb.NewNodeServiceClient(b.conn), nil
}

// Revoke 向 coordinator 提交撤销事件（再由其广播给订阅者）。
func (b *GRPCBroadcaster) Revoke(ctx context.Context, r Revocation) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	_, err = c.Revoke(ctx, revToMsg(r))
	return err
}

// Subscribe 建立到 coordinator 的流式订阅，收到的事件回调给 fn。
// 返回的 cancel 必须在不再需要时调用（取消流并关闭独立连接）。
func (b *GRPCBroadcaster) Subscribe(tenant string, fn func(Revocation)) (cancel func()) {
	ctx, cancelCtx := context.WithCancel(context.Background())
	conn, err := grpc.NewClient(b.addr, b.dialOpts...)
	if err != nil {
		cancelCtx()
		return func() {}
	}
	client := nodepb.NewNodeServiceClient(conn)
	stream, err := client.RevocationStream(ctx, &nodepb.StreamReq{Tenant: tenant})
	if err != nil {
		cancelCtx()
		_ = conn.Close()
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			fn(msgToRev(m))
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancelCtx()
			<-done
		})
	}
}

// Close 关闭 Unary 复用连接（不影响已建立的流式订阅）。
func (b *GRPCBroadcaster) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		err := b.conn.Close()
		b.conn = nil
		return err
	}
	return nil
}

// GRPCPreFilter 实现 PreFilter：粗滤请求转发到指定 peer 节点。
type GRPCPreFilter struct {
	addr string
	reg  *GRPCNodeRegistry
}

// NewGRPCPreFilter 构造指向 peer 节点的预过滤客户端。
func NewGRPCPreFilter(peerAddr string, opts ...GRPCOption) *GRPCPreFilter {
	return &GRPCPreFilter{addr: peerAddr, reg: NewGRPCNodeRegistry("", opts...)}
}

// CandidateIDs 远程粗滤；peer 未启用时返回 nil（全量放行）。
func (f *GRPCPreFilter) CandidateIDs(ctx context.Context, tenantID, queryText string) (map[string]bool, error) {
	return f.reg.PreFilterRemote(ctx, f.addr, tenantID, queryText)
}

// Close 释放底层连接池。
func (f *GRPCPreFilter) Close() error { return f.reg.Close() }
