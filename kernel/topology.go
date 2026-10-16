// 节点拓扑（S2，§5.4 联邦骨架）
// 真源：lightmem.md §5.4 存储后端可插拔（联邦/多节点）；NetworkConfig.Roles、ScalingConfig。
//
// R16 S2 红线：
//   - 只增不改：Node 字段只新增；不修改既有 profile.go 配置结构。
//   - 骨架优先：只做内存实现（不引入 gRPC/真实网络），真机接 nats/gRPC 在后续轮次。
//   - 撤销存储：跨节点 revocation 骨架化，TTL 到期后自清，不进主线。
package kernel

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Node 拓扑节点（骨架，仅内存）。
type Node struct {
	ID       NodeID     `json:"id"`
	Role     NodeRole   `json:"role"`
	Host     string     `json:"host,omitempty"`
	Port     int        `json:"port,omitempty"`
	Status   NodeStatus `json:"status"`
	Parent   NodeID     `json:"parent,omitempty"` // 上级节点（coordinator 通常为空）
	Shards   []string   `json:"shards,omitempty"` // 负责的租户分片
	LastSeen time.Time  `json:"last_seen"`
}

// Edge 拓扑边：从 From 到 To 的有向连接（骨架：单边表示「可路由」）。
type Edge struct {
	From NodeID `json:"from"`
	To   NodeID `json:"to"`
	Hop  int    `json:"hop"` // 从 root 起跳数（用于 pre-filter 的扇出顺序）
}

// Topology 内存拓扑骨架。
// 约定：
//   - 由 Coordinator 节点 root 起始；
//   - AddNode / RemoveNode / Recompute 后 hop 自动重算；
//   - 全部并发安全（R10-A 红线）。
type Topology struct {
	mu    sync.RWMutex
	nodes map[NodeID]*Node
	edges []Edge
	root  NodeID
	rev   *RevocationStore
}

// NewTopology 新建以 root 为根的内存拓扑。
func NewTopology(root NodeID) *Topology {
	return &Topology{
		nodes: map[NodeID]*Node{},
		root:  root,
		rev:   NewRevocationStore(),
	}
}

// Root 返回根节点 ID。
func (t *Topology) Root() NodeID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.root
}

// AddNode 加入节点并重算 hop（幂等：已存在则仅更新字段）。
func (t *Topology) AddNode(n *Node) error {
	if n == nil || n.ID == "" {
		return fmt.Errorf("node id required")
	}
	if n.Role == "" {
		return fmt.Errorf("node role required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := *n
	if cp.LastSeen.IsZero() {
		cp.LastSeen = time.Now().UTC()
	}
	t.nodes[cp.ID] = &cp
	t.recomputeLocked()
	return nil
}

// RemoveNode 移除节点及其所有入边，返回被删节点列表（空 = 未找到）。
func (t *Topology) RemoveNode(id NodeID) []NodeID {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[id]; !ok {
		return nil
	}
	delete(t.nodes, id)
	// 清空所有指向该节点的边
	filtered := t.edges[:0]
	for _, e := range t.edges {
		if e.From != id && e.To != id {
			filtered = append(filtered, e)
		}
	}
	t.edges = filtered
	// 孤儿节点挂回 root
	for _, n := range t.nodes {
		if n.Parent != "" && n.Parent == id {
			n.Parent = t.root
		}
	}
	t.recomputeLocked()
	return []NodeID{id}
}

// Connect 建立 From->To 边，若已存在则更新 Hop。
func (t *Topology) Connect(from, to NodeID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[from]; !ok {
		return fmt.Errorf("from node %s not present", from)
	}
	if _, ok := t.nodes[to]; !ok {
		return fmt.Errorf("to node %s not present", to)
	}
	t.nodes[to].Parent = from
	// 幂等边
	for i := range t.edges {
		if t.edges[i].From == from && t.edges[i].To == to {
			t.recomputeLocked()
			return nil
		}
	}
	t.edges = append(t.edges, Edge{From: from, To: to})
	t.recomputeLocked()
	return nil
}

// Get 取节点（拷贝）。
func (t *Topology) Get(id NodeID) (*Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[id]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

// All 返回全部节点快照（按角色、ID 排序）。
func (t *Topology) All() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []*Node
	for _, n := range t.nodes {
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

// Edges 返回全部边的拷贝。
func (t *Topology) Edges() []Edge {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Edge, len(t.edges))
	copy(out, t.edges)
	return out
}

// Revocations 返回撤销存储（用于广播消费侧查询）。
func (t *Topology) Revocations() *RevocationStore {
	return t.rev
}

// recomputeLocked BFS 从 root 重算 hop（调用方须已持锁）。
func (t *Topology) recomputeLocked() {
	for _, n := range t.nodes {
		n.Parent = ""
	}
	adj := map[NodeID][]NodeID{}
	for _, e := range t.edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	// 若 root 不在图中，选择最早加入的 coordinator
	root := t.root
	if _, ok := t.nodes[root]; !ok {
		for id, n := range t.nodes {
			if n.Role == RoleCoordinator {
				root = id
				break
			}
		}
		if root == "" {
			return
		}
	}
	hop := map[NodeID]int{root: 0}
	queue := []NodeID{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if _, seen := hop[next]; seen {
				continue
			}
			hop[next] = hop[cur] + 1
			if n, ok := t.nodes[next]; ok {
				n.Parent = cur
			}
			queue = append(queue, next)
		}
	}
	// 未连通的节点 hop 按 role 给缺省（仅用于骨架展示）
	for i := range t.edges {
		e := &t.edges[i]
		if h, ok := hop[e.From]; ok {
			e.Hop = h
		} else {
			e.Hop = 0
		}
	}
}

// ---------------------------------------------------------------------
// 跨节点撤销存储（骨架：内存 + TTL 自清）
// ---------------------------------------------------------------------

// RevocationEntry 撤销记录（含 TTL）。
type RevocationEntry struct {
	Revocation Revocation `json:"revocation"`
	ExpireAt   time.Time  `json:"expire_at"`
}

// RevocationStore 撤销存储骨架。
type RevocationStore struct {
	mu   sync.RWMutex
	recs map[string]RevocationEntry
}

func NewRevocationStore() *RevocationStore {
	return &RevocationStore{recs: map[string]RevocationEntry{}}
}

// Add 记录撤销事件（以 memory_id 为键；TTL=0 表示永久）。
func (s *RevocationStore) Add(memoryID string, r Revocation) {
	if memoryID == "" {
		return
	}
	if r.IssuedAt.IsZero() {
		r.IssuedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := time.Time{}
	if r.TTL > 0 {
		exp = r.IssuedAt.Add(time.Duration(r.TTL) * time.Second)
	}
	s.recs[memoryID] = RevocationEntry{Revocation: r, ExpireAt: exp}
}

// Has 判断某 memory_id 是否在撤销集中（含 TTL 检查）。
func (s *RevocationStore) Has(memoryID, tenant string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.recs[memoryID]
	if !ok {
		return false
	}
	if !e.ExpireAt.IsZero() && time.Now().UTC().After(e.ExpireAt) {
		return false
	}
	if e.Revocation.TenantID != "" && e.Revocation.TenantID != tenant {
		return false
	}
	return true
}

// PurgeExpired 清理已过期的撤销记录，返回清理数量。
func (s *RevocationStore) PurgeExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	n := 0
	for id, e := range s.recs {
		if !e.ExpireAt.IsZero() && now.After(e.ExpireAt) {
			delete(s.recs, id)
			n++
		}
	}
	return n
}

// All 返回全部撤销记录的拷贝。
func (s *RevocationStore) All() []Revocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Revocation
	for _, e := range s.recs {
		out = append(out, e.Revocation)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].IssuedAt.Equal(out[j].IssuedAt) {
			return out[i].IssuedAt.Before(out[j].IssuedAt)
		}
		return false
	})
	return out
}
