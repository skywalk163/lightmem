// 存储后端抽象 + Qdrant 桩（P8）
// 真源：lightmem.md §5.4 存储后端可插拔、§5.5 节点角色
package kernel

import (
	"context"
	"fmt"
	"log"
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

// QdrantBackend Qdrant 桩（P8，未实现，仅占位）
type QdrantBackend struct {
	url    string
	client interface{}
}

func newQdrantBackend(ctx context.Context, prof *Profile) (StorageBackend, error) {
	return &QdrantBackend{url: prof.PGURL}, nil
}

func (b *QdrantBackend) Name() string { return "qdrant" }

func (b *QdrantBackend) Ping(ctx context.Context) error {
	return nil // 桩
}

func (b *QdrantBackend) Upsert(ctx context.Context, m *Memory) error {
	log.Printf("[qdrant] upsert %s (stub)", m.MemoryID)
	return nil
}

func (b *QdrantBackend) Get(ctx context.Context, id, viewer, tenant string) (*Memory, error) {
	return nil, fmt.Errorf("qdrant not implemented")
}

func (b *QdrantBackend) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	return &QueryResult{}, nil
}

func (b *QdrantBackend) VectorSearch(ctx context.Context, params QueryParams) (*QueryResult, error) {
	return &QueryResult{}, nil
}

func (b *QdrantBackend) Delete(ctx context.Context, id, viewer, tenant string) error {
	return nil
}

func (b *QdrantBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	return nil, nil
}

func (b *QdrantBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	return 0, nil
}

func (b *QdrantBackend) CountByLayer(ctx context.Context, layer MemoryLayer, tenant string) (int64, error) {
	return 0, nil
}

func (b *QdrantBackend) BackfillVectors(ctx context.Context, embedder func(string) ([]float32, error)) error {
	return nil
}

// NodeRole 节点角色（P8）
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
