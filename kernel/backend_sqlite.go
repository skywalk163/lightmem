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
}

// newSQLiteBackend 装配 SQLite 后端。
// path 为空（含 cfg==nil）→ :memory:（隔离、无文件残留）；否则落盘到该路径。
func newSQLiteBackend(_ context.Context, cfg *StorageConfig) (StorageBackend, error) {
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

	return &sqliteBackend{
		name: "sqlite", // R19-B：不再含 "skeleton"
		path: path,
		db:   db,
	}, nil
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
	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not visible to %s/%s", memoryID, viewer, tenant)
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

// VectorSearch 暂不提供向量：返回明确错误由调度层降级（与原骨架一致，无 sqlite-vec）。
func (b *sqliteBackend) VectorSearch(ctx context.Context, _ QueryParams) ([]QueryResult, error) {
	return nil, fmt.Errorf("vector search not available on %s; downgrade to text (vector_status=pending)", b.name)
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
		return viewer != "" && m.UserID == viewer
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
