// R14-C1：位移边持久化（§5.4.1）。
//
// 真源：lightmem.md §5.4.1；承接 R13-B 遗留事项（EdgeStore 仅进程内）。
//
// 背景：R13 的 kernel/displacement_edge.go 提供进程内 EdgeStore，重启即丢。
// 本文件在**不修改 EdgeStore 既有符号语义**的前提下补齐持久化：
// 落盘格式为 JSON（stdlib，零新依赖），写盘走临时文件 + 原子替换，
// 读盘严格保真（created_at / invalidated_at 逐字节往返）。
//
// 实现口径说明（与任务书草案的差异，已在交付报告登记）：
//   - 本仓库的 SQLite 后端 kernel/backend_sqlite.go 是**内存骨架**（注释自述
//     「最小可运行骨架：内存表结构」），并无真实 database/sql 驱动；本文件
//     因此提供**文件落盘的真实往返**，而非 SQLite 真实库；
//   - 仍导出 SQLite / PostgreSQL 两段 DDL 文本（静态、不执行），作为未来
//     接入真实数据库的落表依据；pg 真机往返留 R15。
//
// 红线遵守：
//   - 只增不改：全新文件，不修改 kernel/displacement_edge.go、任何既有
//     Schema 字段语义、任何默认检索 / 融合 / 写入路径；
//   - 新表不 ALTER：DDL 只含 CREATE TABLE / CREATE INDEX，绝不含 ALTER；
//   - 软删保留：invalidated_at 双向保真，永不物理删除；
//   - 幂等：重复 Save 产出**逐字节相同**的文件（记录按三元组字典序输出）；
//   - 确定性：无随机、无时间依赖（不读 time.Now）、无 LLM。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// edgePersistVersion 落盘格式版本，预留前向兼容。
const edgePersistVersion = 1

// edgePersistFile 落盘文件顶层结构。
type edgePersistFile struct {
	Version int                 `json:"version"`
	MaxOut  int                 `json:"max_out"`
	Records []edgePersistRecord `json:"records"`
}

// edgePersistRecord 单条边的落盘记录。
//
// 时间统一按 unix nanosecond 存 int64，保证跨平台逐位保真。
type edgePersistRecord struct {
	From    string       `json:"from"`
	To      string       `json:"to"`
	Rel     EdgeRelation `json:"rel"`
	W       float64      `json:"w"`
	Created int64        `json:"created"`
	Invalid *int64       `json:"invalid,omitempty"`
}

// EdgePersistence 位移边持久化契约。
type EdgePersistence interface {
	// Load 读出并重建 EdgeStore；文件不存在时返回空 store 且不报错（幂等启动）。
	Load(ctx context.Context) (*EdgeStore, error)
	// Save 把 EdgeStore 全量写盘；并发安全，重复调用幂等且逐字节一致。
	Save(ctx context.Context, store *EdgeStore) error
	// DDL 返回建表语句文本（静态，本实现不执行）。
	DDL() string
}

// edgePersistZeroClock 确定性时钟：Load 重建时不会调用 AddEdge，故不影响数据。
func edgePersistZeroClock() time.Time { return time.Unix(0, 0) }

// FileEdgePersistence 基于 JSON 文件的持久化实现（stdlib，零新依赖）。
type FileEdgePersistence struct {
	path   string
	maxOut int
	mu     sync.Mutex
}

// NewFileEdgePersistence 构造文件持久化后端。
//
// path 必填；maxOut<=0 时取 DefaultMaxOutEdges。构造不触碰磁盘。
func NewFileEdgePersistence(path string, maxOut int) (*FileEdgePersistence, error) {
	if path == "" {
		return nil, fmt.Errorf("edge persist: path required")
	}
	if maxOut <= 0 {
		maxOut = DefaultMaxOutEdges
	}
	return &FileEdgePersistence{path: path, maxOut: maxOut}, nil
}

// Path 返回落盘路径（只读，便于测试与运维）。
func (p *FileEdgePersistence) Path() string { return p.path }

// Load 读盘重建 EdgeStore。
//
// 重建时直接填充 store.edges / store.outIdx，**绕过 AddEdge 的时钟与容量治理**：
// 落盘数据本就是治理后的结果，重放会改变 created_at 与淘汰顺序。
func (p *FileEdgePersistence) Load(ctx context.Context) (*EdgeStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	b, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return NewEdgeStore(p.maxOut, edgePersistZeroClock), nil
	}
	if err != nil {
		return nil, fmt.Errorf("edge persist read %s: %w", p.path, err)
	}
	var f edgePersistFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("edge persist decode %s: %w", p.path, err)
	}
	maxOut := f.MaxOut
	if maxOut <= 0 {
		maxOut = p.maxOut
	}
	store := NewEdgeStore(maxOut, edgePersistZeroClock)
	for _, r := range f.Records {
		e := DisplacementEdge{
			FromMemoryID: r.From,
			ToMemoryID:   r.To,
			Relation:     r.Rel,
			Weight:       r.W,
			CreatedAt:    time.Unix(0, r.Created).UTC(),
		}
		if r.Invalid != nil {
			ts := time.Unix(0, *r.Invalid).UTC()
			e.InvalidatedAt = &ts
		}
		key := edgeTriple(r.From, r.To, r.Rel)
		store.edges[key] = e
		store.outIdx[r.From] = append(store.outIdx[r.From], key)
	}
	return store, nil
}

// Save 全量写盘：采集 → 排序 → 序列化 → 临时文件 → 原子替换。
//
// 记录按三元组字典序输出，保证重复 Save 逐字节一致（幂等且可 diff）。
// 并发调用由 mutex 串行化；store 为 nil 时等价于保存空表。
func (p *FileEdgePersistence) Save(ctx context.Context, store *EdgeStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if store == nil {
		store = NewEdgeStore(p.maxOut, edgePersistZeroClock)
	}
	maxOut := store.maxOut
	if maxOut <= 0 {
		maxOut = p.maxOut
	}

	keys := make([]string, 0, len(store.edges))
	for k := range store.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	records := make([]edgePersistRecord, 0, len(keys))
	for _, k := range keys {
		e := store.edges[k]
		// 时间统一规整为 UTC：unixNano 是绝对时刻，但 time.Time 的 location
		// 字段参与 ==/DeepEqual 比较。若直接取本机时区，跨时区机器上同一条
		// 边会序列化成不同字面量，破坏「同输入→同输出」的确定性。
		rec := edgePersistRecord{
			From:    e.FromMemoryID,
			To:      e.ToMemoryID,
			Rel:     e.Relation,
			W:       e.Weight,
			Created: e.CreatedAt.UTC().UnixNano(),
		}
		if e.InvalidatedAt != nil {
			v := e.InvalidatedAt.UTC().UnixNano()
			rec.Invalid = &v
		}
		records = append(records, rec)
	}

	f := edgePersistFile{Version: edgePersistVersion, MaxOut: maxOut, Records: records}
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("edge persist encode: %w", err)
	}
	buf = append(buf, '\n')

	if dir := filepath.Dir(p.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("edge persist mkdir %s: %w", dir, err)
		}
	}
	// 原子写回：同目录 .tmp + os.Rename。POSIX rename() 在同一文件系统内
	// 是原子覆盖，崩溃最坏情况是残留 .tmp，目标文件始终是完整旧内容，不会半写。
	// 注意：Go 标准库没有 os.Replace，原子替换统一走 os.Rename。
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("edge persist write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("edge persist rename %s: %w", p.path, err)
	}
	return nil
}

// DDL 返回 SQLite 建表语句（静态文本，本实现不执行）。
func (p *FileEdgePersistence) DDL() string { return EdgePersistenceDDLSQLite() }

// EdgePersistenceDDLSQLite 位移边建表语句（SQLite 方言）。
//
// 只增新表：三元组唯一索引建在**有效边**上（partial index），软删行可累积、
// 不占用唯一性，符合「软删不物理删、保留审计」的契约。
func EdgePersistenceDDLSQLite() string {
	return `-- displacement_edges（§5.4.1）：只增新表，禁止 ALTER 既有表/索引
CREATE TABLE IF NOT EXISTS displacement_edges (
  id             INTEGER PRIMARY KEY,
  from_memory_id TEXT    NOT NULL,
  to_memory_id   TEXT    NOT NULL,
  relation       TEXT    NOT NULL CHECK (relation IN ('entity-alias','causal','task-flow','temporal')),
  weight         REAL    NOT NULL CHECK (weight >= 0 AND weight <= 1),
  created_at     INTEGER NOT NULL,
  invalidated_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_disp_edge ON displacement_edges
  (from_memory_id, to_memory_id, relation) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_disp_edge_from ON displacement_edges (from_memory_id);
CREATE INDEX IF NOT EXISTS idx_disp_edge_to ON displacement_edges (to_memory_id);
`
}

// EdgePersistenceDDLPostgres 位移边建表语句（PostgreSQL 方言，待 R15 接真机）。
func EdgePersistenceDDLPostgres() string {
	return `-- displacement_edges（§5.4.1）：只增新表，禁止 ALTER 既有表/索引
CREATE TABLE IF NOT EXISTS displacement_edges (
  id             BIGSERIAL PRIMARY KEY,
  from_memory_id TEXT    NOT NULL,
  to_memory_id   TEXT    NOT NULL,
  relation       TEXT    NOT NULL CHECK (relation IN ('entity-alias','causal','task-flow','temporal')),
  weight         DOUBLE PRECISION NOT NULL CHECK (weight >= 0 AND weight <= 1),
  created_at     TIMESTAMPTZ NOT NULL,
  invalidated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_disp_edge ON displacement_edges
  (from_memory_id, to_memory_id, relation) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_disp_edge_from ON displacement_edges (from_memory_id);
CREATE INDEX IF NOT EXISTS idx_disp_edge_to ON displacement_edges (to_memory_id);
`
}
