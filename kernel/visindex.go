// R28-C1（L-239 续）· SQLite 可见性内存索引。
//
// 背景（业主裁定一 2026-10-20）：
//
//	「800 行扫描本身不该是 22.92ms 的主因——瓶颈大概率在 JSON 解析和每行反序列化。」
//	「预加载热数据：如果向量集合规模可控（800 行说明不大），启动时把 data 字段
//	 解析一次放入内存索引，查询走内存而非每次反序列化。」
//
// 做法：把 (memory_id → 可见性字段) 解析一次放内存；每查询遍历内存条目做判定，
// 消除**每次查询 800 次 json.Unmarshal**。
//
// 安全铁律（与 R27-E 注释同一口径）：
//   - 索引只提供**候选集**，最终判定**仍走 Memory.canView**——canView 是唯一真源，
//     禁止在索引里另造一套可见性判断（R21-B 的 G8 分叉教训：两侧分叉 = 假绿）。
//   - 索引条目包含 canView 所需的**全部**字段（tenant/user/visibility/
//     shared_with_tenants/permission），不裁剪、不推断。
//   - 写路径（Upsert / DeleteByID / DeleteByScope）同步维护，且有测试覆盖
//     「写入后立即查询」「删除后立即查询」。
//
// 零依赖、不改 schema（红线⑦/③）。
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
)

// visIndex 是 (memory_id → 可见性字段) 的内存索引。
type visIndex struct {
	mu     sync.Mutex
	items  map[string]r27VisibilityFields
	ready  bool // 是否已构建
	builds int  // 构建次数（诊断用）
}

func newVisIndex() *visIndex {
	return &visIndex{items: make(map[string]r27VisibilityFields, 1024)}
}

// ensureFromDB 首次使用时从 DB 全量构建（调用方须串行化 DB 访问）。
// 幂等：已构建则直接返回；脏行跳过（与 loadAll / 原 visibleMemoryIDs 同一容错口径）。
func (idx *visIndex) ensureFromDB(ctx context.Context, db *sql.DB) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.ready {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT memory_id, data FROM memories`)
	if err != nil {
		return fmt.Errorf("vis index query: %w", err)
	}
	defer rows.Close()
	items := make(map[string]r27VisibilityFields, 1024)
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return fmt.Errorf("vis index scan: %w", err)
		}
		var v r27VisibilityFields
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			continue // 脏行跳过，不因单条坏数据中断检索
		}
		v.MemoryID = id
		items[id] = v
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("vis index rows: %w", err)
	}
	idx.items = items
	idx.ready = true
	idx.builds++
	return nil
}

// put 写入/更新一条（Upsert 成功后调用）。
func (idx *visIndex) put(v r27VisibilityFields) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.ready {
		return // 尚未构建：ensure 会全量重建，无需增量维护
	}
	if v.MemoryID == "" {
		return
	}
	idx.items[v.MemoryID] = v
}

// remove 删除一条（DeleteByID 成功后调用）。
func (idx *visIndex) remove(memoryID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.ready {
		return
	}
	delete(idx.items, memoryID)
}

// removeMany 批量删除（DeleteByScope 成功后调用）。
func (idx *visIndex) removeMany(ids []string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.ready {
		return
	}
	for _, id := range ids {
		delete(idx.items, id)
	}
}

// visible 遍历索引产出可见 memory_id 集合；判定仍走 canView（单一真源）。
func (idx *visIndex) visible(viewer, tenant string) map[string]bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	set := make(map[string]bool, len(idx.items))
	for id, v := range idx.items {
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
	return set
}

// stats 返回诊断计数（构建次数、条目数）。
func (idx *visIndex) stats() (builds, size int) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.builds, len(idx.items)
}
