// SQLite 存储后端（Solo 档，§5.4）
// R19-B 方案 a：以纯 Go 驱动 modernc.org/sqlite（无 cgo）实现真正的磁盘持久化，
// 取代原内存骨架（kernel/edge_persist.go 记载的「sqlite-skeleton」）。
// 写入 → 关闭进程 → 重开同路径 → 数据仍在（见 backend_sqlite_persist_test.go）。
//
// 设计要点（与 R160 I-15 根因闭环）：
//   - 完整 Memory 以 JSON 存入单行 data 列，主键 memory_id；所有过滤/排序在 Go 内完成，
//     与原内存骨架的遍历/打分/排序语义逐字节一致，保证既有单测零回归。
//   - 不引入第二真源：visibility/layer/kind 等全部从 JSON 还原，避免列与 JSON 漂移。
//   - nil/空路径 → :memory:（各测试实例天然隔离、零临时文件残留，仍是真实 SQLite）；
//     显式路径 → 文件（生产持久化，profile storage.sqlite.path）。
//   - 单连接（MaxOpenConns=1）+ 全方法 mu 互斥，杜绝 "database is locked" 与竞态。
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteBackend 真实 SQLite 持久化后端（Solo 档，R19-B 方案 a）。
type sqliteBackend struct {
	name string
	path string
	db   *sql.DB
	mu   sync.Mutex // 串行化所有 DB 访问（Solo 量级的简单正确方案）

	// vec 是 solo 档纯 Go 向量存储（R25-B · L-232）。与后端共享同一 *sql.DB 与
	// mu，不另开连接、不引入第二真源；nil 仅在极端装配失败时出现（此处必非 nil）。
	vec *SQLiteVectorStore
}

// newSQLiteBackend 装配 SQLite 后端。
// path 为空（含 cfg==nil）→ :memory:（隔离、无文件残留）；否则落盘到该路径。
func newSQLiteBackend(ctx context.Context, cfg *StorageConfig) (StorageBackend, error) {
	path := ""
	if cfg != nil && cfg.SQLite != nil {
		path = cfg.SQLite.Path
	}

	useFile := path != ""
	dsn := path
	if !useFile {
		dsn = ":memory:"
	}

	if useFile {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite mkdir %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// Solo 单进程：单连接即可，配合 mu 彻底避免并发写锁冲突。
	db.SetMaxOpenConns(1)

	if useFile {
		if cfg != nil && cfg.SQLite != nil && cfg.SQLite.WALMode {
			if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
				db.Close()
				return nil, fmt.Errorf("sqlite wal pragma: %w", err)
			}
		}
		if cfg != nil && cfg.SQLite != nil && cfg.SQLite.FKEnabled {
			if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				db.Close()
				return nil, fmt.Errorf("sqlite fk pragma: %w", err)
			}
		}
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		memory_id TEXT PRIMARY KEY,
		data       TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite create table: %w", err)
	}

	b := &sqliteBackend{
		name: "sqlite", // R19-B：不再含 "skeleton"
		path: path,
		db:   db,
	}
	// R25-B · L-232：装配 solo 档纯 Go 向量存储（共享同一 *sql.DB 与 mu）。
	b.vec = newSQLiteVectorStore(b.db, &b.mu)
	if err := b.vec.ensureSchema(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite vector schema: %w", err)
	}

	return b, nil
}

func (b *sqliteBackend) Name() string { return b.name }

// DB 暴露底层 *sql.DB，供 R22.3 管理 API 复用同一连接（auth 表与记忆表同库）。
// 注：sqliteBackend 内部用 mu 串行化写；调用方（AdminStore）只做 auth 表的
// 点查与写入，与后端写在同一 SQLite 连接上会由 database/sql 天然串行化。
func (b *sqliteBackend) DB() *sql.DB { return b.db }

func (b *sqliteBackend) Ping(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db.PingContext(ctx)
}

// loadAll 读出全部记忆（反序列化为独立 struct，天然深拷贝）。
func (b *sqliteBackend) loadAll(ctx context.Context) ([]*Memory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows, err := b.db.QueryContext(ctx, `SELECT data FROM memories`)
	if err != nil {
		return nil, fmt.Errorf("sqlite select: %w", err)
	}
	defer rows.Close()
	var out []*Memory
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("sqlite scan: %w", err)
		}
		var m Memory
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil, fmt.Errorf("sqlite unmarshal: %w", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (b *sqliteBackend) Upsert(ctx context.Context, m *Memory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.MemoryID == "" {
		return fmt.Errorf("memory_id required")
	}
	if m.RecordedAt.IsZero() {
		m.RecordedAt = time.Now().UTC()
	}
	if m.Layer == "" {
		m.Layer = ClassifyLayer(m)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 幂等：同 idempotency_key 已存在 → version 递增（与原骨架语义一致）。
	// version / idempotency_key 存于 JSON data 列，需读出后判断。
	var existingVer int
	var existingIdem string
	var dataStr string
	row := b.db.QueryRowContext(ctx, `SELECT data FROM memories WHERE memory_id=?`, m.MemoryID)
	if err := row.Scan(&dataStr); err == nil {
		var em Memory
		if json.Unmarshal([]byte(dataStr), &em) == nil {
			existingVer = em.Version
			existingIdem = em.IdempotencyKey
		}
		if existingIdem == m.IdempotencyKey {
			m.Version = existingVer + 1
		}
	} else if m.Version == 0 {
		m.Version = 1
	}

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("sqlite marshal: %w", err)
	}
	_, err = b.db.ExecContext(ctx,
		`INSERT INTO memories (memory_id, data) VALUES (?, ?)
		 ON CONFLICT(memory_id) DO UPDATE SET data=excluded.data`,
		m.MemoryID, string(data))
	if err != nil {
		return fmt.Errorf("sqlite upsert: %w", err)
	}
	return nil
}

func (b *sqliteBackend) Get(ctx context.Context, memoryID, viewer, tenant string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var data string
	err := b.db.QueryRowContext(ctx, `SELECT data FROM memories WHERE memory_id=?`, memoryID).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory %s not found", memoryID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite get: %w", err)
	}
	var m Memory
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, fmt.Errorf("sqlite unmarshal: %w", err)
	}
	// R27-A（业主裁定规则 1）：不可见与不存在必须**完全不可区分**——同错误类型、
	// 同消息体。旧的 "not visible to %s/%s" 会让调用方区分出「资源存在但你无权看」，
	// 从而泄漏资源归属/存在性；跨租户读取一律对外表现为 not found。
	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not found", memoryID)
	}
	return &m, nil
}

func (b *sqliteBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}

	var res []QueryResult
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	for _, m := range mems {
		if !m.canView(q.Viewer, q.TenantID) {
			continue
		}
		if len(q.MemoryKinds) > 0 && !containsKind(q.MemoryKinds, m.MemoryKind) {
			continue
		}
		// R24-B · L-226：默认只返回当前生效版本（与 recall 路径既有语义对齐）。
		// IncludeSuperseded=true 才带出被取代的历史版本。
		if !q.IncludeSuperseded && m.SupersededBy != nil {
			continue
		}
		if q.AsOf != nil && !m.visibleAt(*q.AsOf) {
			continue
		}
		score := 0.0
		if needle != "" {
			fact := strings.ToLower(m.Fact)
			if fact == needle {
				score = 1.0
			} else if strings.Contains(fact, needle) {
				score = 0.7
			} else {
				// 词元回退：查询与事实的分词重合度（与 router_mem.go 的 recallScore 一致）。
				qTokens := tokenize(needle)
				if len(qTokens) > 0 {
					fSet := tokenSet(m.Fact)
					hit := 0
					for _, t := range qTokens {
						if fSet[t] {
							hit++
						}
					}
					if hit > 0 {
						score = 0.5 * float64(hit) / float64(len(qTokens))
					} else {
						continue
					}
				} else {
					continue
				}
			}
		} else {
			score = 0.1
		}
		score += m.Confidence * 0.2
		res = append(res, b.toResult(m, score))
	}
	// 次级键：分数相同时按 memory_id 升序，保证确定性。
	sort.Slice(res, func(i, j int) bool {
		if res[i].Score != res[j].Score {
			return res[i].Score > res[j].Score
		}
		return res[i].MemoryID < res[j].MemoryID
	})
	if q.TopK > 0 && len(res) > q.TopK {
		res = res[:q.TopK]
	}
	return res, nil
}

// VectorSearch 向量检索（R25-B · L-232）：solo 档纯 Go 暴力余弦。
//
// 降级契约（§4.2 / 判据 4 零回归）：
//   - 向量表为空（尚未 BackfillVectors）→ 返回与 6cd26e0 逐位一致的降级错误
//     （"downgrade to text"），调度层据此回落文本检索；不返回垃圾结果。
//   - 有向量但全部不可见 → 同样返回降级错误，避免泄露不可见记忆。
//   - 仅当向量表非空且存在可见命中时，返回按余弦排序的 QueryResult（Source 取记忆
//     自身 source 字段，标明其落库档位）。
//
// 线程安全：store 各方法各自持 mu，本方法在 store 调用间隙用不持锁的 lookupMemory
// 取记忆 JSON，避免对 b.mu 的重复进入（sync.Mutex 非可重入）。
func (b *sqliteBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("vector search requires embedding vector")
	}
	n, err := b.vec.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite vector count: %w", err)
	}
	if n == 0 {
		// 与 6cd26e0 行为逐位一致：solo 档尚无向量数据 → 降级文本检索。
		return nil, fmt.Errorf("vector search not available on %s: no vectors stored; run POST /vector/backfill to populate, then downgrade to text (vector_status=pending)", b.name)
	}
	// B 线 B1（L-235）：余弦前按 (viewer, tenant) 可见性预筛。
	// 单租户裸查（无 viewer/tenant 上下文）→ allowedIDs=nil → 退回原 Search 全局路径
	// （判据 3 short-circuit，行为与 82d31e0 逐位一致，零额外开销）。
	// 多租户场景：先算可见 memory_id 集合，再在其内做余弦 top-k，避免少数派被多数派挤占。
	var allowed map[string]bool
	if q.Viewer != "" || q.TenantID != "" {
		allowed, err = b.visibleMemoryIDs(ctx, q.Viewer, q.TenantID)
		if err != nil {
			return nil, err
		}
	}
	var hits []VectorHit
	if allowed == nil {
		hits, err = b.vec.Search(ctx, q.Vector, q.TopK)
	} else {
		hits, err = b.vec.SearchAllowed(ctx, q.Vector, q.TopK, allowed)
	}
	if err != nil {
		return nil, err
	}
	var res []QueryResult
	for _, h := range hits {
		m, err := b.lookupMemory(ctx, h.MemoryID)
		if err != nil {
			continue // 记忆已被删：跳过
		}
		if !m.canView(q.Viewer, q.TenantID) {
			continue // 防御性：预筛后仍校验可见性（与 Query 的硬过滤一致）
		}
		res = append(res, b.toResult(m, h.Score))
	}
	if len(res) == 0 {
		// 所有命中都不可见，或命中记忆均已删除：返回降级提示，不返回空结果冒充成功。
		return nil, fmt.Errorf("vector search returned no visible results on %s; downgrade to text", b.name)
	}
	return res, nil
}

// visibleMemoryIDs 返回对 (viewer, tenant) 可见的 memory_id 集合（B 线 B1，L-235）。
// 复用既有 Memory.canView 可见性逻辑，不另造权限判断。供 VectorSearch 做余弦前预筛。
// 返回 nil 表示「无租户过滤上下文」（由调用方据此 short-circuit 到全局 Search）。
// r27VisibilityFields 只含可见性判定所需字段（R27-E · L-239）。
//
// 目的：避免每查询把整条记忆正文（fact / version_history / feedback_records …）
// 全部反序列化——R26-E 同机对拍实测那部分开销约 15ms/查询，占 `/search/vector`
// P95 的六成。判定逻辑仍**复用 Memory.canView**，只是喂给它一个最小载体。
type r27VisibilityFields struct {
	MemoryID          string     `json:"memory_id"`
	TenantID          string     `json:"tenant_id"`
	UserID            string     `json:"user_id"`
	Visibility        Visibility `json:"visibility"`
	SharedWithTenants []string   `json:"shared_with_tenants,omitempty"`
	Permission        Permission `json:"permission"`
}

func (b *sqliteBackend) visibleMemoryIDs(ctx context.Context, viewer, tenant string) (map[string]bool, error) {
	// R27-E（L-239）：定向取 memory_id + data，只反序列化可见性字段。
	// ⚠️ 可见性判定**必须**仍走 canView（与 Query/Get 同一真源）——禁止在 SQL 侧
	// 用 json_extract 另造可见性判断，那是 R21-B 的 G8 分叉教训（两侧分叉 = 假绿）。
	rows, err := b.db.QueryContext(ctx, `SELECT memory_id, data FROM memories`)
	if err != nil {
		return nil, fmt.Errorf("sqlite visible ids: %w", err)
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("sqlite visible ids scan: %w", err)
		}
		var v r27VisibilityFields
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			continue // 脏行跳过（与 loadAll 的容错口径一致，不因单条坏数据中断检索）
		}
		m := Memory{
			MemoryID:          id,
			TenantID:          v.TenantID,
			UserID:            v.UserID,
			Visibility:        v.Visibility,
			SharedWithTenants: v.SharedWithTenants,
			Permission:        v.Permission,
		}
		if m.canView(viewer, tenant) {
			set[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite visible ids rows: %w", err)
	}
	return set, nil
}

// lookupMemory 不持锁读取单条记忆 JSON（调用方已不在 b.mu 临界区内）。
// 与 Get 的区别：不做 visibility 校验、不重复进入 b.mu（VectorSearch 已释放 mu）。
// DB 单连接（MaxOpenConns=1）在 SQLite 层天然串行化，故无锁读取安全。
func (b *sqliteBackend) lookupMemory(ctx context.Context, memoryID string) (*Memory, error) {
	var data string
	err := b.db.QueryRowContext(ctx, `SELECT data FROM memories WHERE memory_id=?`, memoryID).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory %s not found", memoryID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite get: %w", err)
	}
	var m Memory
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, fmt.Errorf("sqlite unmarshal: %w", err)
	}
	return &m, nil
}

// BackfillVectors 实现 VectorBackfiller（R25-B · L-232）：补全 solo 档缺失的向量。
//
// 语义：扫描全部记忆，对尚无向量（memory_vectors 表无对应行）者用 embedder 生成向量
// 并 Put。幂等：已有向量者跳过（existingIDs 一次性加载）；第二次运行 Vectorized 不再增长。
// 与 PG/Qdrant 的同名方法签名一致（VectorBackfiller 冻结接口，§9）。
func (b *sqliteBackend) BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) (BackfillReport, error) {
	var rep BackfillReport
	mems, err := b.loadAll(ctx) // loadAll 内部持锁读全量后释放，避免与下方 vec 调用死锁
	if err != nil {
		return rep, err
	}
	have, err := b.vec.existingIDs(ctx)
	if err != nil {
		return rep, err
	}
	// B 线 B2（L-236）：攒批 → PutBatch 单事务批量落盘，避免逐条 Put 在 >800 条语料下
	// 反复提交超服务端 30s ctx（R25 bench 须靠幂等重试多次调用才收敛）。批大小 50 实测定；
	// 幂等：已有向量者跳过（existingIDs 一次性加载），二次 backfill Vectorized 不再增长。
	const batchSize = 50
	batch := make([]VectorPutItem, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := b.vec.PutBatch(ctx, batch); err != nil {
			return err
		}
		rep.Vectorized += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for _, m := range mems {
		if m == nil {
			continue
		}
		rep.Scanned++
		if have[m.MemoryID] {
			continue
		}
		if embed == nil {
			rep.Failed++
			continue
		}
		vec, err := embed(m.Fact)
		if err != nil {
			rep.Failed++
			continue
		}
		batch = append(batch, VectorPutItem{MemoryID: m.MemoryID, Vec: vec})
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				rep.Failed += int64(len(batch))
				return rep, err
			}
		}
	}
	if err := flush(); err != nil {
		rep.Failed += int64(len(batch))
		return rep, err
	}
	return rep, nil
}

func (b *sqliteBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, m := range mems {
		if m.MemoryKind == kind && m.TenantID == tenant {
			n++
		}
	}
	return n, nil
}

func (b *sqliteBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db.Close()
}

// ------------------------------------------------------------------
// ComplianceBackend（P7-A GDPR，§7.3/§7.4/§7.5）
// 在真实 SQLite 后端上补齐删除/导出（确定性：导出/返回 ID 按 (recorded_at, memory_id) 升序）。
// ------------------------------------------------------------------

// ExportByScope 返回 (tenant, userID) 作用域下的全部记忆（深拷贝，避免调用方篡改）。
func (b *sqliteBackend) ExportByScope(ctx context.Context, tenant, userID string) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(mems))
	for _, m := range mems {
		if m.TenantID == tenant && m.UserID == userID {
			cp := *m
			out = append(out, &cp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out, nil
}

// DeleteByScope 级联删除 (tenant, userID) 作用域下全部记忆，返回被删 ID（升序）。
func (b *sqliteBackend) DeleteByScope(ctx context.Context, tenant, userID string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range mems {
		if m.TenantID == tenant && m.UserID == userID {
			ids = append(ids, m.MemoryID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Strings(ids)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		if _, err := b.db.ExecContext(ctx, `DELETE FROM memories WHERE memory_id=?`, id); err != nil {
			return nil, fmt.Errorf("sqlite delete by scope: %w", err)
		}
	}
	return ids, nil
}

// DeleteByID 物理删除单条记忆（被遗忘权）。不存在时视为已遗忘（返回 nil，幂等）。
func (b *sqliteBackend) DeleteByID(ctx context.Context, memoryID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.db.ExecContext(ctx, `DELETE FROM memories WHERE memory_id=?`, memoryID); err != nil {
		return fmt.Errorf("sqlite delete by id: %w", err)
	}
	return nil
}

// ---- LayerScanner / MemoryScanner / IdempotencyLookup（P2 A1/A3/A4/A6）----

// ScanLayer 返回指定层（layer 为空 = 全部层）的记忆，限制 max 条。
// 确定性：先全量收集，再按 (recorded_at, memory_id) 升序截断。
func (b *sqliteBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 1000
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(mems))
	for _, m := range mems {
		if tenant != "" && m.TenantID != tenant {
			continue
		}
		if layer != "" && m.Layer != layer {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// UpdateLayer 更新单条记忆的 layer 字段（只增不改：不动 recorded_at）。
func (b *sqliteBackend) UpdateLayer(ctx context.Context, memoryID string, layer MemoryLayer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var data string
	err := b.db.QueryRowContext(ctx, `SELECT data FROM memories WHERE memory_id=?`, memoryID).Scan(&data)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memory %s not found", memoryID)
	}
	if err != nil {
		return fmt.Errorf("sqlite get for update layer: %w", err)
	}
	var m Memory
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return fmt.Errorf("sqlite unmarshal: %w", err)
	}
	m.Layer = layer
	newData, err := json.Marshal(&m)
	if err != nil {
		return fmt.Errorf("sqlite marshal: %w", err)
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE memories SET data=? WHERE memory_id=?`, string(newData), memoryID); err != nil {
		return fmt.Errorf("sqlite update layer: %w", err)
	}
	return nil
}

// CountByLayer 返回指定层（layer 为空 = 全部层）的记忆条数。
func (b *sqliteBackend) CountByLayer(ctx context.Context, layer MemoryLayer, tenant string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, m := range mems {
		if tenant != "" && m.TenantID != tenant {
			continue
		}
		if layer != "" && m.Layer != layer {
			continue
		}
		n++
	}
	return n, nil
}

// ScanAll 跨租户全量扫描（整合 worker 用，§3.13）。按 (recorded_at, memory_id) 升序截断。
func (b *sqliteBackend) ScanAll(ctx context.Context, max int) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 5000
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(mems))
	out = append(out, mems...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// FindByIdempotencyKey 按 (tenant,user,idempotency_key) 查已存在记忆（§4.1）。
// 命中多条时取 memory_id 最小者，避免返回结果随遍历序变化。
func (b *sqliteBackend) FindByIdempotencyKey(ctx context.Context, tenant, user, key string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, nil
	}
	mems, err := b.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	var best *Memory
	for _, m := range mems {
		if m.TenantID != tenant || m.UserID != user || m.IdempotencyKey != key {
			continue
		}
		if best == nil || m.MemoryID < best.MemoryID {
			best = m
		}
	}
	return best, nil
}

// ---- 可见性硬过滤（§3.2）----

func (m *Memory) canView(viewer, tenant string) bool {
	switch m.Visibility {
	case VisPrivate:
		// R27-A（L-238b，业主裁定）：private 仅在**归属租户内**对 owner 可见——
		// 两个租户存在同名 user_id 时，A 租户不得读到 B 租户该 user 的 private 记忆。
		//
		// 用**等值**比较（含两侧同为 "" 的默认租户），刻意不要求 tenant 非空：
		// solo 档与「未传租户上下文」下两侧同为空即同一默认租户，若要求非空会把
		// solo 的 private 记忆整体打成不可见（生产级回归）。跨租户（值不同）一律不可见。
		return viewer != "" && m.UserID == viewer && m.TenantID == tenant
	case VisSharedUser:
		return viewer != "" && (m.UserID == viewer || contains(m.Permission.Readers, viewer))
	case VisTenant:
		return tenant != "" && m.TenantID == tenant
	case VisCrossTenant:
		return tenant != "" && contains(m.SharedWithTenants, tenant)
	case VisPublic:
		// R23.2：公开记忆跨租户可读（任何带身份的 viewer）；写/改/删仍走 owner 校验。
		return viewer != ""
	}
	return false
}

func (m *Memory) visibleAt(date string) bool {
	if m.ValidFrom != "" && date < m.ValidFrom {
		return false
	}
	if m.ValidUntil != "" && date > m.ValidUntil {
		return false
	}
	return m.SupersededBy == nil
}

func (b *sqliteBackend) toResult(m *Memory, score float64) QueryResult {
	return QueryResult{
		MemoryID:     m.MemoryID,
		Fact:         m.Fact,
		Score:        score,
		Source:       m.Source,
		OccurredAt:   m.OccurredAt,
		RecordedAt:   m.RecordedAt,
		ValidFrom:    m.ValidFrom,
		ValidUntil:   m.ValidUntil,
		SupersededBy: m.SupersededBy,
		IsHistorical: m.IsHistorical,
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsKind(kinds []MemoryKind, k MemoryKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}
