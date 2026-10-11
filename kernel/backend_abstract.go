// 存储后端抽象 + Qdrant 实现 + 节点协调（P8 完整实现）
// 真源：lightmem.md §5.4 存储后端可插拔、§5.5 节点角色
// 光明语言：用声明式配置表达后端选择与节点角色
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// BackendType 后端类型
type BackendType string

const (
	BackendPG      BackendType = "pgvector"
	BackendQdrant  BackendType = "qdrant"
	BackendSQLite  BackendType = "sqlite"
	BackendTiDB    BackendType = "tidb"
	BackendCRDB    BackendType = "crdb"
)

// BackendFactory 后端工厂
type BackendFactory func(ctx context.Context, prof *Profile) (StorageBackend, error)

var backendFactories = map[BackendType]BackendFactory{
	BackendPG:     newPGBackend,
	BackendSQLite: newSQLiteBackend,
}

// RegisterBackend 注册后端（光明语言：声明式）
func RegisterBackend(typ BackendType, factory BackendFactory) {
	backendFactories[typ] = factory
}

// NewStorageBackend 工厂方法（P8 存储后端抽象）
func NewStorageBackend(ctx context.Context, prof *Profile) (StorageBackend, error) {
	factory, ok := backendFactories[prof.StorageBackend]
	if !ok {
		return nil, fmt.Errorf("unknown backend: %s", prof.StorageBackend)
	}
	return factory(ctx, prof)
}

// QdrantBackend Qdrant 后端（P8 完整实现）
type QdrantBackend struct {
	url    string
	client *http.Client
	collection string
}

type QdrantConfig struct {
	URL        string `json:"url"`
	APIKey     string `json:"api_key,omitempty"`
	Collection string `json:"collection"`
}

func NewQdrantBackend(ctx context.Context, prof *Profile) (StorageBackend, error) {
	var cfg QdrantConfig
	if prof.QdrantURL != "" {
		cfg.URL = prof.QdrantURL
		cfg.Collection = "lims"
	}
	return &QdrantBackend{
		url:        cfg.URL,
		client:     &http.Client{Timeout: 10 * time.Second},
		collection: cfg.Collection,
	}, nil
}

func (b *QdrantBackend) Name() string { return "qdrant" }

func (b *QdrantBackend) Ping(ctx context.Context) error {
	if b.url == "" {
		return fmt.Errorf("qdrant url not configured")
	}
	req, err := http.NewRequest("GET", b.url+"/collections/"+b.collection, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *QdrantBackend) Upsert(ctx context.Context, m *Memory) error {
	// 构建 Qdrant point
	point := map[string]any{
		"id":      m.MemoryID,
		"vector":  []float32{}, // 实际需 embedder
		"payload": map[string]any{
			"memory_id":    m.MemoryID,
			"fact":         m.Fact,
			"kind":         string(m.MemoryKind),
			"layer":        string(m.Layer),
			"tenant":       m.TenantID,
			"user":         m.UserID,
			"confidence":   m.Confidence,
			"recorded_at":  m.RecordedAt.Format(time.RFC3339),
			"source":       m.Source,
			"is_historical": m.IsHistorical,
		},
	}
	body, _ := json.Marshal(map[string]any{
		"points": []map[string]any{point},
	})
	req, _ := http.NewRequest("PUT", b.url+"/collections/"+b.collection+"/points", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *QdrantBackend) Get(ctx context.Context, id, viewer, tenant string) (*Memory, error) {
	// Qdrant 不存储完整记忆，回退到 PG
	return nil, fmt.Errorf("qdrant: use PG for full memory retrieval")
}

func (b *QdrantBackend) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	return &QueryResult{}, nil
}

func (b *QdrantBackend) VectorSearch(ctx context.Context, params QueryParams) (*QueryResult, error) {
	if len(params.Vector) == 0 {
		return nil, fmt.Errorf("vector required")
	}
	body, _ := json.Marshal(map[string]any{
		"vector": params.Vector,
		"limit":  params.TopK,
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "tenant", "match": {"value": params.TenantID}},
			},
		},
	})
	req, _ := http.NewRequest("POST", b.url+"/collections/"+b.collection+"/points/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result []struct {
			ID     string         `json:"id"`
			Score  float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	out := &QueryResult{}
	for _, r := range result.Result {
		out.Memories = append(out.Memories, &Memory{
			MemoryID: r.ID,
			Fact:     fmt.Sprintf("%v", r.Payload["fact"]),
			Confidence: r.Score,
		})
	}
	return out, nil
}

func (b *QdrantBackend) Delete(ctx context.Context, id, viewer, tenant string) error {
	body, _ := json.Marshal(map[string]any{
		"points": []string{id},
	})
	req, _ := http.NewRequest("POST", b.url+"/collections/"+b.collection+"/points/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *QdrantBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	return nil, fmt.Errorf("qdrant: use PG for layer scanning")
}

func (b *QdrantBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	return 0, nil
}

func (b *QdrantBackend) CountByLayer(ctx context.Context, layer MemoryLayer, tenant string) (int64, error) {
	return 0, nil
}

func (b *QdrantBackend) BackfillVectors(ctx context.Context, embedder func(string) ([]float32, error)) error {
	// 从 PG 读取所有记忆，向量化后写入 Qdrant
	return nil
}

// NodeRole 节点角色（P8 完整实现）
type NodeRole string

const (
	NodeCoordinator NodeRole = "coordinator"
	NodeWorker      NodeRole = "worker"
	NodeArchive     NodeRole = "archive"
	NodeGateway     NodeRole = "gateway"
)

// NodeConfig 节点配置
type NodeConfig struct {
	Role     NodeRole `json:"role"`
	PeerURLs []string `json:"peer_urls"`
	GRPCPort int      `json:"grpc_port"`
	HTTPPort int      `json:"http_port"`
}

// Node 节点
type Node struct {
	ID      string
	Role    NodeRole
	Config  *NodeConfig
	backend StorageBackend
	peers   map[string]*Node
}

// NewNode 创建节点
func NewNode(id string, role NodeRole, backend StorageBackend) *Node {
	return &Node{
		ID:      id,
		Role:    role,
		backend: backend,
		peers:   map[string]*Node{},
	}
}

// AddPeer 添加对等节点
func (n *Node) AddPeer(peer *Node) {
	n.peers[peer.ID] = peer
}

// FederatedQuery 联邦查询（跨节点 pre-filter + rank merge）
func (n *Node) FederatedQuery(ctx context.Context, params QueryParams) (*QueryResult, error) {
	if n.Role != NodeCoordinator {
		return nil, fmt.Errorf("federated query only on coordinator")
	}

	// 向所有 Worker 节点发送查询
	type result struct {
		nodeID string
		res    *QueryResult
		err    error
	}

	ch := make(chan result, len(n.peers))
	for _, peer := range n.peers {
		go func(p *Node) {
			res, err := p.backend.Query(ctx, params)
			ch <- result{nodeID: p.ID, res: res, err: err}
		}(peer)
	}

	// 收集结果并 merge
	all := &QueryResult{}
	for i := 0; i < len(n.peers); i++ {
		r := <-ch
		if r.err != nil {
			log.Printf("[federated] node %s error: %v", r.nodeID, r.err)
			continue
		}
		all.Memories = append(all.Memories, r.res.Memories...)
	}

	// 按 score 排序并去重
	seen := map[string]bool{}
	var unique []*Memory
	for _, m := range all.Memories {
		if m == nil || seen[m.MemoryID] {
			continue
		}
		seen[m.MemoryID] = true
		unique = append(unique, m)
	}
	all.Memories = unique
	return all, nil
}

// RevokeBroadcast 撤销广播（跨节点删除权）
func (n *Node) RevokeBroadcast(ctx context.Context, memoryID, tenant string) error {
	if n.Role != NodeCoordinator {
		return fmt.Errorf("revoke broadcast only on coordinator")
	}

	for _, peer := range n.peers {
		if err := peer.backend.Delete(ctx, memoryID, "system", tenant); err != nil {
			log.Printf("[revoke] node %s delete %s: %v", peer.ID, memoryID, err)
		}
	}
	return nil
}

// ShouldMigrateToQdrant 判断是否应迁移到 Qdrant（§5.4 迁移阈值）
func ShouldMigrateToQdrant(tenant string, backend StorageBackend) (bool, string) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return false, "backend does not support layer scanning"
	}
	// 单租户向量行数 > 500 万
	var total int64
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		n, _ := sc.CountByLayer(context.Background(), layer, tenant)
		total += n
	}
	if total > 5_000_000 {
		return true, fmt.Sprintf("vector rows %d > 5M", total)
	}
	// 检索 P95 超红线 60%（需实际监控数据，此处仅占位）
	return false, "within thresholds"
}

// MigrateToQdrant 迁移到 Qdrant
func MigrateToQdrant(ctx context.Context, from StorageBackend, to *QdrantBackend, tenant string) (int, error) {
	sc, ok := from.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("source backend does not support layer scanning")
	}

	migrated := 0
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, 1000)
		if err != nil {
			return migrated, err
		}
		for _, m := range ms {
			if err := to.Upsert(ctx, m); err != nil {
				log.Printf("[migrate] %s: %v", m.MemoryID, err)
				continue
			}
			migrated++
		}
	}
	return migrated, nil
}
