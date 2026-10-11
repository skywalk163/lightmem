// 光明记忆（LIMS）统一记忆 Schema（§4.1）
// 字段名以 lightmem.md §4.1 单一真源为准。
package kernel

import "time"

// MemoryKind 记忆内容类型（§2.3）
type MemoryKind string

const (
	KindEvent      MemoryKind = "event"
	KindFact       MemoryKind = "fact"
	KindPreference MemoryKind = "preference"
	KindProcedure  MemoryKind = "procedure"
	KindGoal       MemoryKind = "goal"
)

// Visibility 可见范围（§3.2，硬过滤）
type Visibility string

const (
	VisPrivate     Visibility = "private"
	VisSharedUser  Visibility = "shared_user"
	VisTenant      Visibility = "tenant"
	VisCrossTenant Visibility = "cross_tenant"
)

// Provenance 来源标记（§7.5）
type Provenance string

const (
	ProvNormal           Provenance = "normal"
	ProvInjectionSuspect Provenance = "prompt_injection_suspect"
)

// MemoryLayer 温度分层（§2.2）
type MemoryLayer string

const (
	LayerL0 MemoryLayer = "L0" // 工作记忆：分钟~小时
	LayerL1 MemoryLayer = "L1" // 动态记忆：天~月
	LayerL2 MemoryLayer = "L2" // 静态记忆：年~数十年
	LayerL3 MemoryLayer = "L3" // 归档记忆：合规保留，可恢复
)

// VectorStatus 向量补全状态（§4.2）
type VectorStatus string

const (
	VectorPending    VectorStatus = "pending"
	VectorVectorized VectorStatus = "vectorized"
)

// FeedbackStatus 反馈语义（§3.11）
type FeedbackStatus string

const (
	FbAdopted  FeedbackStatus = "adopted"
	FbRejected FeedbackStatus = "rejected"
	FbEdited   FeedbackStatus = "edited"
)

// Permission 权限三元组（§7.1）
type Permission struct {
	Owner   string   `json:"owner"`
	Writers []string `json:"writers,omitempty"`
	Readers []string `json:"readers,omitempty"`
}

// ReferenceFrame 参考系坐标（§3.2）
type ReferenceFrame struct {
	Tenant   string `json:"tenant,omitempty"`
	User     string `json:"user,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Project  string `json:"project,omitempty"`
	Entity   string `json:"entity,omitempty"`
	Scene    string `json:"scene,omitempty"`
	Task     string `json:"task,omitempty"`
	Category string `json:"category,omitempty"`
}

// SourceEvidence 溯源信息（§7.5）
type SourceEvidence struct {
	Origin  string `json:"origin"`
	Snippet string `json:"snippet"` // ≤200 字符，按 permission_level 脱敏
}

// GoalDetail goal 特有语义（§3.8）
type GoalDetail struct {
	Goal        string   `json:"goal"`
	KPI         *KPI     `json:"kpi,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	Status      string   `json:"status"` // active / paused / completed / expired
	Progress    float64  `json:"progress,omitempty"`
	Deadline    string   `json:"deadline,omitempty"`
}

// KPI 目标衡量指标（§3.8）
type KPI struct {
	Metric  string `json:"metric"`
	Target  int64  `json:"target"`
	Cadence string `json:"cadence"`
}

// FeedbackRecord 反馈记录（§3.11）
type FeedbackRecord struct {
	Status   FeedbackStatus `json:"status"`
	Time     time.Time      `json:"time"`
	Operator string         `json:"operator"`
}

// Memory 统一 Schema 记忆条目（§4.1 单一真源）
type Memory struct {
	MemoryID          string         `json:"memory_id"` // ULID
	IdempotencyKey    string         `json:"idempotency_key"`
	TenantID          string         `json:"tenant_id"`
	UserID            string         `json:"user_id"`
	AgentID           string         `json:"agent_id,omitempty"`
	ProjectID         string         `json:"project_id,omitempty"`
	Visibility        Visibility     `json:"visibility"`
	SharedWithTenants []string       `json:"shared_with_tenants,omitempty"`
	Permission        Permission     `json:"permission"`
	PermissionLevel   string         `json:"permission_level"` // public/internal/机密（返回前密级过滤）
	MemoryKind        MemoryKind     `json:"memory_kind"`
	Fact              string         `json:"fact"`
	ReferenceFrame    ReferenceFrame `json:"reference_frame,omitempty"`

	OccurredAt   *time.Time `json:"occurred_at,omitempty"`
	RecordedAt   time.Time  `json:"recorded_at"`           // 唯一主字段，只增不改
	ValidFrom    string     `json:"valid_from,omitempty"`  // date-only，UTC 日界
	ValidUntil   string     `json:"valid_until,omitempty"` // date-only
	SupersededBy *string    `json:"superseded_by,omitempty"`

	Confidence        float64    `json:"confidence"`
	Source            string     `json:"source"`
	Version           int        `json:"version"`
	Importance        float64    `json:"importance,omitempty"`
	RecentAccessScore float64    `json:"recent_access_score,omitempty"`
	AccessCount       int64      `json:"access_count,omitempty"` // 仅展示
	ExpireAt          *time.Time `json:"expire_at,omitempty"`

	Provenance     Provenance      `json:"provenance"`
	SourceEvidence *SourceEvidence `json:"source_evidence,omitempty"`
	VectorStatus   VectorStatus    `json:"vector_status"`
	IsHistorical   bool            `json:"is_historical"` // 仅显式 as_of 回溯置真

	// Layer 温度分层（§2.2）；由 LayeredStore 维护，写入时 ClassifyLayer 决定。
	// 只增不改既有字段语义（§4.1 只增原则）。
	Layer MemoryLayer `json:"layer,omitempty"`

	FeedbackRecords []FeedbackRecord `json:"feedback_records,omitempty"`
	VersionHistory  []int            `json:"version_history,omitempty"`
	ExternalRef     *string          `json:"external_ref,omitempty"`
	GoalDetail      *GoalDetail      `json:"goal_detail,omitempty"`
	// IsCore 核心记忆块（P4）：高频访问记忆常驻上下文，优先返回。
	IsCore          bool             `json:"is_core,omitempty"`
}

// QueryResult 单条检索结果（§4.4 响应）
type QueryResult struct {
	MemoryID     string     `json:"memory_id"`
	Fact         string     `json:"fact"`
	Score        float64    `json:"score"`
	Source       string     `json:"source"`
	OccurredAt   *time.Time `json:"occurred_at,omitempty"`
	RecordedAt   time.Time  `json:"recorded_at"`
	ValidFrom    string     `json:"valid_from,omitempty"`
	ValidUntil   string     `json:"valid_until,omitempty"`
	SupersededBy *string    `json:"superseded_by,omitempty"`
	IsHistorical bool       `json:"is_historical"`
}
