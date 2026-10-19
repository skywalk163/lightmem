// R24-B：元数据为事实源 + 索引可重建（3FS 架构映射）。
//
// 真源：lightmem.md 任务书 R24 §5「B 线」、§9「接口冻结」。
//
// 目标：让「向量索引坏了能被发现、能被重建」成为**所有后端一致的能力**。
// 三条设计原则：
//  1. **元数据为事实源**：索引（向量）是元数据的派生视图，坏了可以重建；
//     对账（Reconcile）只报差异，重建（RebuildIndex）才动手补。
//  2. **不改 StorageBackend 接口**（红线）：新能力一律走「可选接口 + 类型断言」，
//     沿用仓内既有的 LayerScanner / ComplianceBackend 范式。
//  3. **默认不启用任何后台定时器**（红线）：Reconcile / RebuildIndex 只由显式
//     HTTP 调用触发，进程内零自动任务。
//
// 只增不改：本文件是 R24-B 新增文件，不修改任何既有 Schema 字段语义。
package kernel

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// ------------------------------------------------------------------
// B2 · 补向量：从「PG 专属」提升为后端能力接口
// ------------------------------------------------------------------

// BackfillReport 补向量结果计数。
type BackfillReport struct {
	Scanned    int64 `json:"scanned"`
	Vectorized int64 `json:"vectorized"`
	Failed     int64 `json:"failed"`
}

// VectorBackfiller 可选的「补向量」能力接口（B2）。
//
// 只增不改：不进 StorageBackend（保护名单），由调用方类型断言获取；
// 不支持的后端返回明确错误，不再写死 "only on pgvector backend"。
type VectorBackfiller interface {
	BackfillVectors(ctx context.Context, embedder func(string) ([]float32, error)) (BackfillReport, error)
}

// ------------------------------------------------------------------
// B3 · 对账与重建
// ------------------------------------------------------------------

// driftIDCap DriftedIDs 上限（红线：防大租户响应爆炸）。
const driftIDCap = 50

// ReconcileReport 索引-元数据对账报告（§5.1 B3，接口冻结）。
type ReconcileReport struct {
	Backend      string                `json:"backend"`
	Total        int64                 `json:"total"`
	Vectorized   int64                 `json:"vectorized"`
	Pending      int64                 `json:"pending"`
	ZeroVector   int64                 `json:"zero_vector"`   // 有 embedding 但全零
	OrphanIndex  int64                 `json:"orphan_index"`  // 索引有、元数据无
	MissingIndex int64                 `json:"missing_index"` // 元数据有、索引无
	DriftedIDs   []string              `json:"drifted_ids,omitempty"`
	LayerCounts  map[MemoryLayer]int64 `json:"layer_counts"`
	CheckedAt    string                `json:"checked_at"`
}

// RebuildReport 重建索引结果（§5.1 B3）。
type RebuildReport struct {
	Backend    string `json:"backend"`
	Scanned    int64  `json:"scanned"`
	Vectorized int64  `json:"vectorized"`
	Failed     int64  `json:"failed"`
	Changed    int64  `json:"changed"`
	RebuiltAt  string `json:"rebuilt_at"`
}

// IndexAudit 后端特定的向量索引审计结果。
//
// Memory 结构体不携带 Embedding（向量留在后端内部），故「零向量」「孤儿索引」
// 「缺失索引」这三项无法从通用 Memory 判断，必须走后端特定能力。
type IndexAudit struct {
	Indexed    int64    `json:"indexed"`     // 索引中的点数
	ZeroVector int64    `json:"zero_vector"` // 索引中全零向量的点数
	OrphanIDs  []string `json:"orphan_ids"`  // 索引有、元数据无
	MissingIDs []string `json:"missing_ids"` // 元数据有、索引无
}

// VectorIndexAuditor 可选的「向量索引审计」能力（B3 辅助接口，只增）。
// 未实现的后端对应字段记 0，报告中不谎报。
type VectorIndexAuditor interface {
	AuditVectorIndex(ctx context.Context, tenant string) (*IndexAudit, error)
}

// Reconcile 对账：只读扫描元数据与索引，报告差异（**不写库**，判据 3）。
//
// 行为：
//   - 经 LayerScanner 只读扫出该租户全部记忆（layer="" 表示不限层）；
//   - 统计 vectorized / pending 与分层计数；
//   - 若后端实现 VectorIndexAuditor，补齐 ZeroVector / OrphanIndex / MissingIndex；
//   - DriftedIDs = 孤儿 + 缺失（去重、排序、上限 50），保证幂等可比。
//
// 幂等：除 CheckedAt 外所有字段在相同数据下逐次相同。
func Reconcile(ctx context.Context, backend StorageBackend, tenant string) (*ReconcileReport, error) {
	if backend == nil {
		return nil, fmt.Errorf("reconcile: backend is nil")
	}
	sc, ok := backend.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("reconcile: backend %s does not implement LayerScanner", backend.Name())
	}
	ms, err := sc.ScanLayer(ctx, "", tenant, 0)
	if err != nil {
		return nil, fmt.Errorf("reconcile: scan: %w", err)
	}

	rep := &ReconcileReport{
		Backend:     backend.Name(),
		LayerCounts: make(map[MemoryLayer]int64),
	}
	metaIDs := make(map[string]bool, len(ms))
	for _, m := range ms {
		if m == nil {
			continue
		}
		rep.Total++
		rep.LayerCounts[m.Layer]++
		metaIDs[m.MemoryID] = true
		switch m.VectorStatus {
		case VectorVectorized:
			rep.Vectorized++
		case VectorPending:
			rep.Pending++
		}
	}

	// 后端特定的索引审计（可选）。
	if au, ok := backend.(VectorIndexAuditor); ok {
		audit, err := au.AuditVectorIndex(ctx, tenant)
		if err != nil {
			return nil, fmt.Errorf("reconcile: audit vector index: %w", err)
		}
		if audit != nil {
			rep.ZeroVector = audit.ZeroVector
			rep.OrphanIndex = int64(len(audit.OrphanIDs))
			rep.MissingIndex = int64(len(audit.MissingIDs))
			rep.DriftedIDs = capSortedIDs(append(append([]string{}, audit.OrphanIDs...), audit.MissingIDs...), driftIDCap)
		}
	} else {
		// 未实现审计接口：以元数据侧为准剔除「元数据有、索引无」的粗判——
		// 即所有 pending 都算缺失索引（元数据有、向量未就位）。
		missing := make([]string, 0)
		for _, m := range ms {
			if m != nil && m.VectorStatus != VectorVectorized {
				missing = append(missing, m.MemoryID)
			}
		}
		rep.MissingIndex = int64(len(missing))
		rep.DriftedIDs = capSortedIDs(missing, driftIDCap)
	}

	rep.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	return rep, nil
}

// capSortedIDs 去重 + 字典序排序 + 截断到 cap（保证幂等可比）。
func capSortedIDs(ids []string, cap int) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > cap {
		out = out[:cap]
	}
	return out
}

// RebuildIndex 重建索引：把缺失/待补的向量补上（B3）。
//
// 语义：**只补不删**——不删除孤儿索引点（删除是不可逆操作，留给运维显式执行）。
// 幂等：第二次运行时 Changed 应为 0。
func RebuildIndex(ctx context.Context, backend StorageBackend, embedder func(string) ([]float32, error), tenant string) (*RebuildReport, error) {
	if backend == nil {
		return nil, fmt.Errorf("reindex: backend is nil")
	}
	sc, ok := backend.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("reindex: backend %s does not implement LayerScanner", backend.Name())
	}
	rep := &RebuildReport{Backend: backend.Name(), RebuiltAt: time.Now().UTC().Format(time.RFC3339)}

	// 优先走后端原生 BackfillVectors（PG 已有、Qdrant 新增）。
	if bf, ok := backend.(VectorBackfiller); ok {
		if embedder == nil {
			return nil, fmt.Errorf("reindex: backend %s supports backfill but no embedder provided", backend.Name())
		}
		r, err := bf.BackfillVectors(ctx, embedder)
		if err != nil {
			return nil, fmt.Errorf("reindex: backfill: %w", err)
		}
		rep.Scanned, rep.Vectorized, rep.Failed = r.Scanned, r.Vectorized, r.Failed
		rep.Changed = r.Vectorized
		return rep, nil
	}

	// 兜底：无原生能力时，逐条扫描 + 通过 Upsert 让后端自行补向量。
	// 仅当后端 embedder 可用时有意义；否则只统计不动作。
	ms, err := sc.ScanLayer(ctx, "", tenant, 0)
	if err != nil {
		return nil, fmt.Errorf("reindex: scan: %w", err)
	}
	for _, m := range ms {
		if m == nil {
			continue
		}
		rep.Scanned++
		if m.VectorStatus == VectorVectorized {
			continue
		}
		if embedder == nil {
			rep.Failed++
			continue
		}
		if _, err := embedder(m.Fact); err != nil {
			rep.Failed++
			continue
		}
		if err := backend.Upsert(ctx, m); err != nil {
			rep.Failed++
			continue
		}
		rep.Vectorized++
		rep.Changed++
	}
	return rep, nil
}

// reconcileEqual 判据 2：两次 Reconcile 除 CheckedAt 外全等。
// 供测试与自检使用（导出以便 E 线在收口脚本里复用同一口径）。
func reconcileEqual(a, b *ReconcileReport) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Backend != b.Backend || a.Total != b.Total ||
		a.Vectorized != b.Vectorized || a.Pending != b.Pending ||
		a.ZeroVector != b.ZeroVector || a.OrphanIndex != b.OrphanIndex ||
		a.MissingIndex != b.MissingIndex {
		return false
	}
	if len(a.LayerCounts) != len(b.LayerCounts) {
		return false
	}
	for k, v := range a.LayerCounts {
		if b.LayerCounts[k] != v {
			return false
		}
	}
	if len(a.DriftedIDs) != len(b.DriftedIDs) {
		return false
	}
	for i := range a.DriftedIDs {
		if a.DriftedIDs[i] != b.DriftedIDs[i] {
			return false
		}
	}
	return true // CheckedAt 不参与比较
}

// ------------------------------------------------------------------
// B3 · 路由自注册（**不改 router.go**，守文件域硬约束）
// ------------------------------------------------------------------

// RegisterReindexRoutes 注册 B 线自带的对账/重建端点。
//
// 为什么自注册而不是改 router.go：§2.2 文件域互斥表规定 A/B/C/D **一律不得
// 再改 `kernel/router.go`**（T0 已封版）。任务书 §5.1 B3 原文写「只在 router.go
// 追加两行」，与文件域硬约束冲突；本线按**文件域优先**执行，经 T0 预埋的
// `RegisterRoutes` 自注册，router.go 零接触。
//
// 端点：
//   - GET  /admin/reconcile?tenant_id=...  对账（只读）
//   - POST /admin/reindex?tenant_id=...    重建（补向量）
func RegisterReindexRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/reconcile", func(w http.ResponseWriter, r *http.Request) {
		s := serverFromRequest(r)
		if s == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reindex routes not bound to an APIServer"})
			return
		}
		rep, err := Reconcile(r.Context(), s.backend, r.URL.Query().Get("tenant_id"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	mux.HandleFunc("POST /admin/reindex", func(w http.ResponseWriter, r *http.Request) {
		s := serverFromRequest(r)
		if s == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reindex routes not bound to an APIServer"})
			return
		}
		if s.embedder == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "embedder not configured"})
			return
		}
		rep, err := RebuildIndex(r.Context(), s.backend, s.embedder, r.URL.Query().Get("tenant_id"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
}

// reindexServer 保存自注册路由所绑定的 APIServer（避免侵入 router.go）。
// 由 SetReindexServer 注入；未注入时端点返回 503。
var reindexServer *APIServer

// SetReindexServer 绑定 B 线端点所属的 APIServer（B 线在 main.go 里调用）。
// 路由注册本身在 RegisterReindexRoutes 中完成，二者都由 B 线持有，router.go 零改动。
func SetReindexServer(s *APIServer) { reindexServer = s }

// serverFromRequest 返回绑定的 APIServer（nil 表示未注入）。
func serverFromRequest(_ *http.Request) *APIServer { return reindexServer }

// EnableReindexRoutes 启用 B 线的对账/重建端点（**默认不启用**）。
//
// 调用点：应在 `cmd/lightmem-kernel/main.go` 里按开关调用（该文件属 C 线独占域，
// 故本线只提供入口、不越界修改，见 §5.1 交接说明）：
//
//	if reindexEnabled {
//	    kernel.EnableReindexRoutes(srv)
//	}
//
// 默认不调用 ⇒ `routeHooks` 保持为空 ⇒ `Handler()` 与 `c908091` 逐字节一致
// （T0 判据 5 的对拍前提不被破坏）。
func EnableReindexRoutes(s *APIServer) {
	if s == nil {
		return
	}
	SetReindexServer(s)
	RegisterRoutes(RegisterReindexRoutes)
}
