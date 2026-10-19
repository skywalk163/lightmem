// StorageBackend 存储后端抽象（§5.4）
// 核心不变：统一 Schema 单真源、双时态、幂等、可见性硬过滤。
// 装配档位通过 profile 切换具体实现，不修改业务层。
package kernel

import "context"

// StorageBackend 是所有存储后端的统一接口。
// 约定：
//   - Upsert 为幂等写，IdempotencyKey 冲突时返回已存在记录（§4.1）。
//   - 查询层必须先做 visibility 硬过滤，再做 relevance 排序（§3.2）。
//   - 向量检索失败必须降级为文本检索并回填 vector_status=pending（§4.2）。
type StorageBackend interface {
	// Name 返回后端标识（solo/sqlite / family/pgvector / ...）
	Name() string

	// Ping 探活
	Ping(ctx context.Context) error

	// Upsert 幂等写入一条记忆
	Upsert(ctx context.Context, m *Memory) error

	// Get 按 memory_id 取单条（含 visibility 校验）
	Get(ctx context.Context, memoryID, viewer, tenant string) (*Memory, error)

	// Query 文本检索（§4.4 /search）
	Query(ctx context.Context, q QueryParams) ([]QueryResult, error)

	// VectorSearch 向量检索（§4.4 /search/vector）；失败时返回 err，
	// 由调度层降级为 Query。
	VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error)

	// CountByKind 按 memory_kind 计数（§3.6 指标）
	CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error)

	// Close 释放资源
	Close() error
}

// QueryParams 统一检索参数（§4.4）
type QueryParams struct {
	TenantID    string
	Viewer      string
	UserID      string
	Visibility  []Visibility // 空 = 默认按 viewer 角色推导
	MemoryKinds []MemoryKind
	Text        string
	Vector      []float32
	TopK        int
	AsOf        *string // YYYY-MM-DD 显式回溯
	// IncludeSuperseded 是否返回已被取代的历史版本（R24-B · L-226）。
	// 默认 false = 只返回当前生效版本，与 recall 路径（router_mem.go）既有语义对齐；
	// 置 true 时才带出 superseded_by IS NOT NULL 的行。
	// AsOf != nil（显式历史回溯）时，调用方需同时置 true 才能取到历史版本。
	IncludeSuperseded bool
}
