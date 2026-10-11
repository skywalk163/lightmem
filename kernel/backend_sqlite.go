// SQLite 存储后端（Solo 档，§5.4）
// 最小可运行骨架：内存表结构，仅用于跑通 Family 档装配链路。
// 生产实现应替换为 database/sql + sqlite-vec 扩展（§4.2 异步向量补全）。
package kernel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// sqliteBackend 内存实现，仅骨架验证用。
type sqliteBackend struct {
	name string
	path string
	mu   sync.RWMutex
	data map[string]*Memory // memory_id -> Memory
}

func newSQLiteBackend(_ context.Context, cfg *StorageConfig) (StorageBackend, error) {
	path := "./data/lims.sqlite"
	ext := "sqlite-vec"
	if cfg != nil && cfg.SQLite != nil {
		if cfg.SQLite.Path != "" {
			path = cfg.SQLite.Path
		}
		if cfg.SQLite.VectorExtension != "" {
			ext = cfg.SQLite.VectorExtension
		}
	}
	return &sqliteBackend{
		name: "sqlite-skeleton/" + ext,
		path: path,
		data: make(map[string]*Memory),
	}, nil
}

func (b *sqliteBackend) Name() string { return b.name }

func (b *sqliteBackend) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	// 幂等：同 idempotency_key 已存在则覆盖 version 递增
	if old, ok := b.data[m.MemoryID]; ok && old.IdempotencyKey == m.IdempotencyKey {
		m.Version = old.Version + 1
	}
	b.data[m.MemoryID] = m
	return nil
}

func (b *sqliteBackend) Get(ctx context.Context, memoryID, viewer, tenant string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	m, ok := b.data[memoryID]
	if !ok {
		return nil, fmt.Errorf("memory %s not found", memoryID)
	}
	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not visible to %s/%s", memoryID, viewer, tenant)
	}
	return m, nil
}

func (b *sqliteBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	var res []QueryResult
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	for _, m := range b.data {
		if !m.canView(q.Viewer, q.TenantID) {
			continue
		}
		if len(q.MemoryKinds) > 0 && !containsKind(q.MemoryKinds, m.MemoryKind) {
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
				continue
			}
		} else {
			score = 0.1
		}
		score += m.Confidence * 0.2
		res = append(res, b.toResult(m, score))
	}
	// 次级键：分数相同时按 memory_id 升序。map 遍历无序，缺次级键会让同分项顺序随机。
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

// VectorSearch 骨架暂不提供向量：返回明确错误由调度层降级。
func (b *sqliteBackend) VectorSearch(ctx context.Context, _ QueryParams) ([]QueryResult, error) {
	return nil, fmt.Errorf("vector search not available on %s; downgrade to text (vector_status=pending)", b.name)
}

func (b *sqliteBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var n int64
	for _, m := range b.data {
		if m.MemoryKind == kind && m.TenantID == tenant {
			n++
		}
	}
	return n, nil
}

func (b *sqliteBackend) Close() error { return nil }

// ---- LayerScanner / MemoryScanner / IdempotencyLookup（P2 A1/A3/A4/A6）----
// 说明：SQLite 骨架后端在内存 map 上补齐分层扫描与幂等查找，
// 使 Solo 档与单元测试无需真实 PG 即可跑通 §3.10/§4.1/§3.13 通路。
// 只增不改：不改动既有 Upsert/Get/Query 语义。

// ScanLayer 返回指定层（layer 为空 = 全部层）的记忆，限制 max 条。
// 确定性：先筛出全量匹配项，再按 (recorded_at, memory_id) 升序排序后截断 max；
// 不可在 map 遍历中提前 break —— 那样「取到哪 max 条」由 map 随机序决定，
// 同一层不同次扫描会返回不同子集，导致上层检索结果不可复现。
func (b *sqliteBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 1000
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*Memory, 0, len(b.data))
	for _, m := range b.data {
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
	m, ok := b.data[memoryID]
	if !ok {
		return fmt.Errorf("memory %s not found", memoryID)
	}
	m.Layer = layer
	return nil
}

// CountByLayer 返回指定层（layer 为空 = 全部层）的记忆条数。
func (b *sqliteBackend) CountByLayer(ctx context.Context, layer MemoryLayer, tenant string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var n int64
	for _, m := range b.data {
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

// ScanAll 跨租户全量扫描（整合 worker 用，§3.13）。
// 确定性同 ScanLayer：先全量收集，再按 (recorded_at, memory_id) 升序截断。
func (b *sqliteBackend) ScanAll(ctx context.Context, max int) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 5000
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*Memory, 0, len(b.data))
	for _, m := range b.data {
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

// FindByIdempotencyKey 按 (tenant,user,idempotency_key) 查已存在记忆（§4.1）。
// 确定性：命中多条时取 memory_id 最小者，避免返回结果随 map 遍历序变化。
func (b *sqliteBackend) FindByIdempotencyKey(ctx context.Context, tenant, user, key string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var best *Memory
	for _, m := range b.data {
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
