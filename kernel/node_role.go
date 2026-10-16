// 节点角色（S2，§5.4 联邦/多节点骨架）
// 真源：lightmem.md §5.4 存储后端可插拔 + NetworkConfig.Roles / ScalingConfig；
// profiles/lims.profile.schema.json scaling.nodes / network.components。
//
// R16 S2 红线：
//   - 只增不改：本文件只新增，不修改既有 profile.go / router 语义。
//   - 只骨架优先：只定义接口 + 内存实现，不引入真实网络库（gRPC/nats 由后续轮次接入）。
//   - 接线默认关：CrossNodeRankMerge 由 profile.features 控制，默认不进入主线（R16 红线④）。
package kernel

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// NodeRole 节点在联邦拓扑中的角色（§5.4）。
// 与 NetworkConfig.Roles map 的 key 语义对齐：每类角色承载一组职责。
type NodeRole string

const (
	// RoleCoordinator 协调者：路由 + 结果融合 + 心跳监控。
	RoleCoordinator NodeRole = "coordinator"
	// RoleWorker 工作节点：承担 recall 与融合信号计算。
	RoleWorker NodeRole = "worker"
	// RoleArchive 归档节点：L3 冻结层、合规导出、被遗忘权。
	RoleArchive NodeRole = "archive"
	// RoleGateway 网关节点：外部连接器 / 请求入口，不持有主数据。
	RoleGateway NodeRole = "gateway"
)

// NodeStatus 节点运行状态。
type NodeStatus string

const (
	StatusUnknown  NodeStatus = "unknown"
	StatusActive   NodeStatus = "active"
	StatusDegraded NodeStatus = "degraded"
	StatusOffline  NodeStatus = "offline"
)

// NodeID 节点标识（与 topology 中的 Node.ID 对应）。
type NodeID string

// NodeInfo 单节点运行时快照。
type NodeInfo struct {
	ID           NodeID     `json:"id"`
	Role         NodeRole   `json:"role"`
	Host         string     `json:"host,omitempty"`
	Port         int        `json:"port,omitempty"`
	Status       NodeStatus `json:"status"`
	TenantShards []string   `json:"tenant_shards,omitempty"` // 该节点负责的租户分片（空 = 全局）
	LastSeen     time.Time  `json:"last_seen"`
	Load         float64    `json:"load,omitempty"` // 0..1，仅用于排序参考
}

// Heartbeat 心跳报文（内存骨架中仅作时间戳；真机接 gRPC/WS 后承载租约）。
type Heartbeat struct {
	NodeID NodeID    `json:"node_id"`
	At     time.Time `json:"at"`
}

// Revocation 跨节点撤销广播（R16 S2 骨架：内存订阅 + 过滤链）。
type Revocation struct {
	MemoryIDs []string  `json:"memory_ids"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	TTL       int       `json:"ttl_seconds,omitempty"` // 0 = 永久
}

// PreFilter 跨节点预过滤（骨架：仅签名，真机接分布式 Bloom / 布隆过滤器）。
// 语义：在跨节点 rank merge 之前，先做一遍粗滤，只放行可能命中的候选集。
type PreFilter interface {
	// CandidateIDs 返回某节点在给定租户/查询文本下可能命中的 memory_id 候选集。
	// 骨架实现：空集合 = 全部放行（未启用）。
	CandidateIDs(ctx context.Context, tenantID, queryText string) (map[string]bool, error)
}

// NodeRegistry 节点注册中心（骨架：纯内存，默认关闭不参与主线）。
type NodeRegistry interface {
	Register(node NodeInfo) error
	Unregister(id NodeID) error
	Get(id NodeID) (*NodeInfo, bool)
	List(role NodeRole) []*NodeInfo
	Heartbeat(hb Heartbeat) error
	// PurgeStale 剔除 last_seen 超过 timeout 的节点（由调用方周期触发）。
	PurgeStale(timeout time.Duration) []NodeID
}

// CrossNodeBroadcaster 跨节点广播（骨架：内存订阅 + 撤销事件扇出）。
type CrossNodeBroadcaster interface {
	// Subscribe 订阅 revocation 事件，返回取消函数。
	Subscribe(tenant string, fn func(Revocation)) (cancel func())
	// Revoke 扇出一条撤销事件给匹配租户的订阅者。
	Revoke(ctx context.Context, r Revocation) error
}

// InMemoryNodeRegistry 内存版注册中心（骨架，单测用；默认不进主线）。
type InMemoryNodeRegistry struct {
	mu    sync.RWMutex
	nodes map[NodeID]*NodeInfo
}

func NewInMemoryNodeRegistry() *InMemoryNodeRegistry {
	return &InMemoryNodeRegistry{nodes: map[NodeID]*NodeInfo{}}
}

func (r *InMemoryNodeRegistry) Register(n NodeInfo) error {
	if n.ID == "" {
		return fmt.Errorf("node id required")
	}
	if n.Role == "" {
		return fmt.Errorf("node role required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := n
	if cp.LastSeen.IsZero() {
		cp.LastSeen = time.Now().UTC()
	}
	r.nodes[cp.ID] = &cp
	return nil
}

func (r *InMemoryNodeRegistry) Unregister(id NodeID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[id]; !ok {
		return fmt.Errorf("node %s not registered", id)
	}
	delete(r.nodes, id)
	return nil
}

func (r *InMemoryNodeRegistry) Get(id NodeID) (*NodeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

func (r *InMemoryNodeRegistry) List(role NodeRole) []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*NodeInfo
	for _, n := range r.nodes {
		if role != "" && n.Role != role {
			continue
		}
		cp := *n
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (r *InMemoryNodeRegistry) Heartbeat(hb Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[hb.NodeID]
	if !ok {
		return fmt.Errorf("node %s not registered", hb.NodeID)
	}
	n.LastSeen = hb.At
	if hb.At.IsZero() {
		n.LastSeen = time.Now().UTC()
	}
	return nil
}

// PurgeStale 标记 last_seen 超过 timeout 的节点为 offline（保留在注册表，便于审计），
// 返回被标记节点 ID（升序）。调用方可再 Unregister 彻底移除。
func (r *InMemoryNodeRegistry) PurgeStale(timeout time.Duration) []NodeID {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().UTC().Add(-timeout)
	var removed []NodeID
	for id, n := range r.nodes {
		if n.LastSeen.Before(cutoff) {
			n.Status = StatusOffline
			removed = append(removed, id)
		}
	}
	sort.SliceStable(removed, func(i, j int) bool { return removed[i] < removed[j] })
	return removed
}

// InMemoryBroadcaster 内存版广播器（骨架：订阅 + 扇出）。
type InMemoryBroadcaster struct {
	mu   sync.RWMutex
	subs map[string]map[int64]func(Revocation) // tenant -> subID -> fn
	seq  int64
}

func NewInMemoryBroadcaster() *InMemoryBroadcaster {
	return &InMemoryBroadcaster{subs: map[string]map[int64]func(Revocation){}}
}

func (b *InMemoryBroadcaster) Subscribe(tenant string, fn func(Revocation)) (cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := b.seq
	if b.subs[tenant] == nil {
		b.subs[tenant] = map[int64]func(Revocation){}
	}
	b.subs[tenant][id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if m, ok := b.subs[tenant]; ok {
				delete(m, id)
				if len(m) == 0 {
					delete(b.subs, tenant)
				}
			}
		})
	}
}

func (b *InMemoryBroadcaster) Revoke(ctx context.Context, r Revocation) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	// 空租户（全局撤销）：所有租户的订阅者都收到。
	// 具体租户：该租户的订阅者 + 全局订阅者（全局订阅对任意事件都感兴趣）。
	var slots []map[int64]func(Revocation)
	if r.TenantID == "" {
		for _, m := range b.subs {
			slots = append(slots, m)
		}
	} else {
		if m, ok := b.subs[r.TenantID]; ok {
			slots = append(slots, m)
		}
		if m, ok := b.subs[""]; ok {
			slots = append(slots, m)
		}
	}
	for _, m := range slots {
		for _, fn := range m {
			fn(r)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// 组装侧（骨架：跨节点 rank merge 的 pre-filter 占位）
// ---------------------------------------------------------------------

// NoopPreFilter 空实现：全部放行（默认关闭，R16 红线④）。
type NoopPreFilter struct{}

func (NoopPreFilter) CandidateIDs(ctx context.Context, tenantID, queryText string) (map[string]bool, error) {
	return nil, nil
}

// BuildTopology 从 profile.ScalingConfig + NetworkConfig 构建内存拓扑。
// 仅返回注册成功的节点；scaling 未启用时返回空集（不进主线）。
func BuildTopology(roles map[string][]RoleSpec, nodes int) []*NodeInfo {
	if nodes <= 0 {
		return nil
	}
	var out []*NodeInfo
	for roleName, specs := range roles {
		role := NodeRole(roleName)
		for _, s := range specs {
			out = append(out, &NodeInfo{
				Role:     role,
				Host:     s.Host,
				Status:   StatusActive,
				LastSeen: time.Now().UTC(),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Host < out[j].Host
	})
	return out
}
