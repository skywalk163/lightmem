// Profile 加载与校验（§2.6 可缩放模式）
// 四档 profile 通过 profiles/lims-*.profile.yaml 声明装配参数；
// 由本包读取并装配 StorageBackend。
//
// 注意：结构严格对齐 profiles/lims.profile.schema.json（draft-07，字段真源）。
// storage 为嵌套子配置（sqlite / pg / qdrant / tidb / zfs / archive / embedding），
// 不是扁平字段；与 YAML 实际形态一一对应。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProfileTier 四档档位（与 schema 的 profile enum 对齐）
type ProfileTier string

const (
	TierSolo       ProfileTier = "solo"
	TierFamily     ProfileTier = "family"
	TierSMB        ProfileTier = "smb"
	TierEnterprise ProfileTier = "enterprise"
)

// StorageBackendKind 与 schema 的 storage_backend enum 对齐
type StorageBackendKind string

const (
	BackendSQLite   StorageBackendKind = "sqlite"
	BackendPgVector StorageBackendKind = "pgvector"
	BackendQdrant   StorageBackendKind = "qdrant"
	BackendPgShard  StorageBackendKind = "pgvector+shard"
)

// Profile 是 lims-*.profile.yaml 的解析结构（字段真源 = JSON Schema）。
// 注意：cache / mq 在 Schema 中是字符串枚举（组件选择器），不是对象；
// redis / rabbitmq / kafka 才是具体对象配置。
type Profile struct {
	Profile        ProfileTier        `yaml:"profile"`
	StorageBackend StorageBackendKind `yaml:"storage_backend"`
	Storage        *StorageConfig     `yaml:"storage"`
	Cache          string             `yaml:"cache"`
	Redis          *RedisConfig       `yaml:"redis,omitempty"`
	MQ             string             `yaml:"mq"`
	RabbitMQ       *RabbitMQConfig    `yaml:"rabbitmq,omitempty"`
	Kafka          *KafkaConfig       `yaml:"kafka,omitempty"`
	Network        *NetworkConfig     `yaml:"network,omitempty"`
	Security       *SecurityConfig    `yaml:"security,omitempty"`
	Features       *FeaturesConfig    `yaml:"features,omitempty"`
	Scaling        *ScalingConfig     `yaml:"scaling,omitempty"`
}

// StorageConfig 存储子配置（嵌套，§4.1/schema）
type StorageConfig struct {
	SQLite    *SQLiteConfig    `yaml:"sqlite,omitempty"`
	PG        *PGConfig        `yaml:"pg,omitempty"`
	Qdrant    *QdrantConfig    `yaml:"qdrant,omitempty"`
	TiDB      *TiDBConfig      `yaml:"tidb,omitempty"`
	ZFS       *ZFSConfig       `yaml:"zfs,omitempty"`
	Archive   *ArchiveConfig   `yaml:"archive,omitempty"`
	Embedding *EmbeddingConfig `yaml:"embedding,omitempty"`
}

// SQLiteConfig Solo 档
type SQLiteConfig struct {
	Path            string `yaml:"path"`
	VectorExtension string `yaml:"vector_extension"`
	WALMode         bool   `yaml:"wal_mode"`
	FKEnabled       bool   `yaml:"fk_enabled"`
	// R29-T0（S0 接缝）：SQLite pragma 调优字段。
	// 零值 = 完全不执行对应 pragma = 与现状逐位一致（E 线 backend_sqlite.go 消费）。
	Synchronous       string `yaml:"synchronous"`         // "" = 不改（默认现状）
	BusyTimeoutMS     int    `yaml:"busy_timeout_ms"`     // 0 = 不执行 PRAGMA busy_timeout
	WALAutocheckpoint int    `yaml:"wal_autocheckpoint"`  // 0 = 不改检查点阈值
	CheckpointOnClose bool   `yaml:"checkpoint_on_close"` // false = 关闭时不强制 checkpoint
}

// PGConfig PostgreSQL + pgvector
type PGConfig struct {
	Version  string       `yaml:"version"`
	Host     string       `yaml:"host"`
	Port     int          `yaml:"port"`
	Database string       `yaml:"database"`
	User     string       `yaml:"user"`
	Password string       `yaml:"password"`
	PoolSize int          `yaml:"pool_size"`
	SSLMode  string       `yaml:"sslmode"`
	Primary  *PGEndpoint  `yaml:"primary,omitempty"`
	Replica  *ReplicaSpec `yaml:"replica,omitempty"`
	Sharding *ShardSpec   `yaml:"sharding,omitempty"`
	Vector   *VectorSpec  `yaml:"vector,omitempty"`
}

// PGEndpoint 主端点
type PGEndpoint struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// ReplicaSpec 备副本
type ReplicaSpec struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Replication string `yaml:"replication"`
	SyncMode    string `yaml:"sync_mode"`
}

// ShardSpec 分片（Enterprise）
type ShardSpec struct {
	Strategy string        `yaml:"strategy"`
	Shards   []ShardMember `yaml:"shards"`
	Routing  string        `yaml:"routing"`
}

// ShardMember 单个分片
type ShardMember struct {
	ID   string `yaml:"id"`
	Host string `yaml:"host"`
}

// VectorSpec 向量索引
type VectorSpec struct {
	IndexType      string `yaml:"index_type"`
	M              int    `yaml:"m"`
	EFConstruction int    `yaml:"ef_construction"`
	EFSearch       int    `yaml:"ef_search"`
}

// QdrantConfig 独立向量库（SMB/Enterprise 阈值触发）
type QdrantConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	GRPCPort int    `yaml:"grpc_port"`
	HTTPS    bool   `yaml:"https"`
	APIKey   string `yaml:"api_key"`
}

// TiDBConfig 元数据（Enterprise）
type TiDBConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// ZFSConfig FreeBSD ZFS dataset
type ZFSConfig struct {
	Enabled  bool         `yaml:"enabled"`
	Pool     string       `yaml:"pool"`
	Datasets []ZFSDataset `yaml:"datasets"`
	KeyFile  string       `yaml:"keyfile"`
}

// ZFSDataset 单个 dataset
type ZFSDataset struct {
	Name        string `yaml:"name"`
	Mountpoint  string `yaml:"mountpoint"`
	Recordsize  string `yaml:"recordsize"`
	Compression string `yaml:"compression"`
	Sync        string `yaml:"sync"`
	Logbias     string `yaml:"logbias"`
	Encryption  string `yaml:"encryption"`
	Keylocation string `yaml:"keylocation"`
}

// ArchiveConfig 归档后端
type ArchiveConfig struct {
	Path     string `yaml:"path"`
	Backend  string `yaml:"backend"`
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
}

// EmbeddingConfig 向量化
type EmbeddingConfig struct {
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
	Device     string `yaml:"device"`
}

// RedisConfig Redis（Schema 真源：host/port/cluster/ttl_seconds/core_memory_ttl）
type RedisConfig struct {
	Host          string         `yaml:"host"`
	Port          int            `yaml:"port"`
	Cluster       []RedisCluster `yaml:"cluster"`
	ClusterMode   string         `yaml:"cluster_mode"`
	SentinelHosts []string       `yaml:"sentinel_hosts"`
	TTLSeconds    int            `yaml:"ttl_seconds"`
	CoreMemoryTTL int            `yaml:"core_memory_ttl"`
}

// RedisCluster 单个 Redis 集群节点
type RedisCluster struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// RabbitMQConfig
type RabbitMQConfig struct {
	Host   string   `yaml:"host"`
	Port   int      `yaml:"port"`
	VHost  string   `yaml:"vhost"`
	HAMode string   `yaml:"ha_mode"`
	Queues []string `yaml:"queues"`
}

// KafkaConfig
type KafkaConfig struct {
	Brokers []KafkaBroker `yaml:"brokers"`
	Topics  []string      `yaml:"topics"`
}

// KafkaBroker 单个 Kafka broker
type KafkaBroker struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// NetworkConfig 网络与拓扑
type NetworkConfig struct {
	Bind       string                `yaml:"bind"`
	APIPort    int                   `yaml:"api_port"`
	VNetSubnet string                `yaml:"vnet_subnet"`
	Components map[string]string     `yaml:"components"`
	Subnets    map[string]string     `yaml:"subnets"`
	Roles      map[string][]RoleSpec `yaml:"roles"`
	PF         *PFConfig             `yaml:"pf"`
}

// RoleSpec 集合体节点角色
type RoleSpec struct {
	Host string `yaml:"host"`
}

// PFConfig pf 防火墙
type PFConfig struct {
	Enabled       bool     `yaml:"enabled"`
	ExposedPorts  []int    `yaml:"exposed_ports"`
	ManagementSSH bool     `yaml:"management_ssh"`
	IPSecPeers    []string `yaml:"ipsec_peers"`
}

// SecurityConfig 安全开关（§5.7）
type SecurityConfig struct {
	Jail              bool   `yaml:"jail"`
	ZFSEncryption     bool   `yaml:"zfs_encryption"`
	MAC               bool   `yaml:"mac"`
	MACPolicy         string `yaml:"mac_policy"`
	ACL               bool   `yaml:"acl"`
	IPSec             bool   `yaml:"ipsec"`
	PkgAudit          bool   `yaml:"pkg_audit"`
	ProcessAccounting bool   `yaml:"process_accounting"`
	MinimalPrivilege  bool   `yaml:"minimal_privilege"`
	TLS               string `yaml:"tls"`
	KeyManagement     string `yaml:"key_management"`
	KMSEndpoint       string `yaml:"kms_endpoint"`
}

// FeaturesConfig 功能开关（poison_guard/audit/delete_rights 不可裁剪）
type FeaturesConfig struct {
	PoisonGuard          bool `yaml:"poison_guard"`
	Audit                bool `yaml:"audit"`
	DeleteRights         bool `yaml:"delete_rights"`
	CoreMemory           bool `yaml:"core_memory"`
	Consolidation        bool `yaml:"consolidation"`
	MultiSignalRetrieval bool `yaml:"multi_signal_retrieval"`
	FeedbackLoop         bool `yaml:"feedback_loop"`
	GoalMemory           bool `yaml:"goal_memory"`
	ExternalConnectors   bool `yaml:"external_connectors"`
	SharedVisibility     bool `yaml:"shared_visibility"`
	MultiTenant          bool `yaml:"multi_tenant"`
	WritePolicyPlugins   bool `yaml:"write_policy_plugins"`
	HumanCorrection      bool `yaml:"human_correction"`
	Federated            bool `yaml:"federated"`
	CrossNodeRankMerge   bool `yaml:"cross_node_rank_merge"`
}

// ScalingConfig 部署形态与扩容
type ScalingConfig struct {
	Mode        string         `yaml:"mode"`
	Nodes       int            `yaml:"nodes"`
	NodesDetail map[string]int `yaml:"nodes_detail"`
	Horizontal  bool           `yaml:"horizontal"`
	Rebalance   bool           `yaml:"rebalance"`
	UpgradePath string         `yaml:"upgrade_path"`
}

// LoadProfile 读取并解析 profile YAML。
// envLookup 非空时，PG/Qdrant 密码可被环境变量覆盖（避免明文）。
func LoadProfile(path string, envLookup func(string) string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load profile: %w", err)
	}
	var p Profile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}
	// R29-T0（B1 预备）：先解析凭据/连接参数覆盖（env），再 validate。
	// 这样 LIMS_PG_HOST 等环境变量注入的 host 也能被下方 validate 的 B1 看到，
	// 不会因「YAML 没写 host 但 env 给了」而误报 fail-fast。
	p.resolveSecrets(envLookup)
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("validate profile %s: %w", path, err)
	}
	return &p, nil
}

func (p *Profile) validate() error {
	if p.Profile == "" {
		return fmt.Errorf("missing profile tier")
	}
	switch p.Profile {
	case TierSolo, TierFamily, TierSMB, TierEnterprise:
	default:
		return fmt.Errorf("unknown profile tier %q", p.Profile)
	}
	if p.StorageBackend == "" {
		return fmt.Errorf("missing storage_backend")
	}
	if p.Storage == nil {
		return fmt.Errorf("missing storage config")
	}
	// B1（R29-T0，行为变更）：pg 档（pgvector / pgvector+shard）必须显式声明连接 host。
	// 缺失 → fail-fast（明确报错），避免 newPgBackend 在 host 空时产生不直观的连接错误，
	// 也防止误用未接线的 primary/sharding 子节点充当主连接地址。
	if p.StorageBackend == BackendPgVector || p.StorageBackend == BackendPgShard {
		if p.Storage.PG == nil {
			return fmt.Errorf("storage_backend=%s requires storage.pg config", p.StorageBackend)
		}
		if p.Storage.PG.Host == "" {
			return fmt.Errorf("storage_backend=%s requires storage.pg.host (pg 档不得依赖未接线的 primary/sharding 子节点)", p.StorageBackend)
		}
	}
	// 核心边界：三个不可裁剪特性必须为 true（§2.6）
	if p.Features != nil {
		if !p.Features.PoisonGuard {
			return fmt.Errorf("features.poison_guard must be true (not croppable, §2.6)")
		}
		if !p.Features.Audit {
			return fmt.Errorf("features.audit must be true (not croppable, §2.6)")
		}
		if !p.Features.DeleteRights {
			return fmt.Errorf("features.delete_rights must be true (not croppable, §2.6)")
		}
	}
	return nil
}

// resolveSecrets 用环境变量覆盖 PG/Qdrant 连接参数。
// 部署实例属性（host/port/user）与安全敏感值（password）都支持环境变量覆盖，
// 不修改 profile 真源。
func (p *Profile) resolveSecrets(env func(string) string) {
	if env == nil {
		return
	}
	if p.Storage == nil {
		return
	}
	if p.Storage.PG != nil {
		if v := env("LIMS_PG_HOST"); v != "" {
			p.Storage.PG.Host = v
		}
		if v := env("LIMS_PG_PORT"); v != "" {
			if port, err := parsePort(v); err == nil {
				p.Storage.PG.Port = port
			}
		}
		if v := env("LIMS_PG_USER"); v != "" {
			p.Storage.PG.User = v
		}
		if v := env("LIMS_PG_DB"); v != "" {
			p.Storage.PG.Database = v
		}
		if v := env("LIMS_PG_PASSWORD"); v != "" {
			p.Storage.PG.Password = v
		}
	}
	if p.Storage.Qdrant != nil {
		if v := env("LIMS_QDRANT_API_KEY"); v != "" {
			p.Storage.Qdrant.APIKey = v
		}
	}
	if p.Storage.ZFS != nil {
		if v := env("LIMS_ZFS_POOL"); v != "" {
			p.Storage.ZFS.Pool = v
		}
	}
}

func parsePort(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty port")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid port %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// NewStorageBackend 根据 profile 装配 StorageBackend（§2.6 核心不变、档位切换）。
func NewStorageBackend(ctx context.Context, p *Profile) (StorageBackend, error) {
	switch p.StorageBackend {
	case BackendSQLite:
		return newSQLiteBackend(ctx, p.Storage)
	case BackendPgVector, BackendPgShard:
		return newPgBackend(ctx, p.Storage)
	case BackendQdrant:
		// R16 S1：Qdrant 档位正式接线（§5.4 多后端可插拔）。
		// 未配置 Qdrant 时 newQdrantBackend 返回明确 error（不静默降级，R15 口径）。
		return newQdrantBackend(ctx, p.Storage)
	default:
		return nil, fmt.Errorf("storage backend %q not yet wired in kernel skeleton; see docs §5.4", p.StorageBackend)
	}
}

// crossNodeEnabled 判断 profile 是否开启跨节点 rank merge（R17 S3 红线④）。
// profile / features 为 nil 时一律视为关闭（默认关）。
func (p *Profile) crossNodeEnabled() bool {
	return p != nil && p.Features != nil && p.Features.CrossNodeRankMerge
}

// CoordinatorGRPCAddr 解析 coordinator 节点的 gRPC 端点（host:port）。
// 优先级：env LIGHTMEM_NODE_GRPC_ADDR > network.roles.coordinator[0].host
// +（env LIGHTMEM_NODE_GRPC_PORT > DefaultNodeGRPCPort）；无法解析时返回 ""。
func CoordinatorGRPCAddr(p *Profile) string {
	if v := os.Getenv(EnvNodeGRPCAddr); v != "" {
		return v
	}
	host := ""
	if p != nil && p.Network != nil && p.Network.Roles != nil {
		if specs := p.Network.Roles[string(RoleCoordinator)]; len(specs) > 0 {
			host = specs[0].Host
		}
	}
	if host == "" {
		return ""
	}
	port := DefaultNodeGRPCPort
	if v := os.Getenv(EnvNodeGRPCPort); v != "" {
		if n, err := parsePort(v); err == nil {
			port = n
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// NewNodeRegistry 按 profile 装配节点注册中心（R17 S3）。
//
// 接线默认关（红线④）：features.cross_node_rank_merge 未开启（含
// profile/features 为 nil）时返回 InMemoryNodeRegistry，不创建任何
// gRPC 连接；开启时返回 GRPCNodeRegistry（懒连接，首次 RPC 才拨号），
// coordinator 端点由 CoordinatorGRPCAddr 解析。
func NewNodeRegistry(p *Profile) NodeRegistry {
	if !p.crossNodeEnabled() {
		return NewInMemoryNodeRegistry()
	}
	return NewGRPCNodeRegistry(CoordinatorGRPCAddr(p))
}

// NewCrossNodeBroadcaster 按 profile 装配跨节点广播器（R17 S3）。
// 默认关口径与 NewNodeRegistry 一致：未开启时返回 InMemoryBroadcaster。
func NewCrossNodeBroadcaster(p *Profile) CrossNodeBroadcaster {
	if !p.crossNodeEnabled() {
		return NewInMemoryBroadcaster()
	}
	return NewGRPCBroadcaster(CoordinatorGRPCAddr(p))
}

// ProfileJSON 输出脱敏后的 JSON 形态（供 /profile 端点）
func (p *Profile) ProfileJSON() string {
	c := *p
	// 深拷贝 storage 再脱敏，避免污染原始 Profile
	if p.Storage != nil {
		sc := *p.Storage
		if p.Storage.PG != nil {
			pg := *p.Storage.PG
			if pg.Password != "" {
				pg.Password = "***"
			}
			sc.PG = &pg
		}
		if p.Storage.Qdrant != nil {
			qd := *p.Storage.Qdrant
			if qd.APIKey != "" {
				qd.APIKey = "***"
			}
			sc.Qdrant = &qd
		}
		c.Storage = &sc
	}
	b, _ := json.MarshalIndent(&c, "", "  ")
	return string(b)
}

// NormalizeTier 规范化 tier 名
func NormalizeTier(s string) (ProfileTier, error) {
	t := ProfileTier(strings.ToLower(strings.TrimSpace(s)))
	switch t {
	case TierSolo, TierFamily, TierSMB, TierEnterprise:
		return t, nil
	}
	return "", fmt.Errorf("invalid tier %q (want solo|family|smb|enterprise)", s)
}
