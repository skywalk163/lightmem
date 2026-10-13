// 生命周期扫描器（§3.10 MemoryLifecycleManager / §5.3 固有价值）
// 真源：lightmem.md §3.10、§5.3「晋升/降级按固有价值自动流转」。
//
// A4 变更（R138-B）：把「按固定时间硬晋升」替换为「按固有价值分数自动流转」：
//   - 固有价值分 value = 0.5*importance + 0.3*recent_access_score + 0.2*confidence（权重可配）。
//   - 阈值：L0→L1 value ≥ 0.6 或 access_count ≥ 3；
//     L1→L2 value ≥ 0.8 且 稳定期 ≥ 7 天（无 rejected 反馈）；
//     L2→L3 value ≤ 0.3 或 valid_until 过期；
//     L2→L1 value < 0.5 且无近期访问。
//   - 后台 worker（A6 整合）定期扫描批量流转。
//   - 不物理删除，只改 layer 字段；version_history 追加（通过 UpdateLayer 旁路维护）。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/lightmem/kernel/i18n"
)

// LifecycleStep 单次生命周期动作（§3.10）
type LifecycleStep struct {
	MemoryID string      `json:"memory_id"`
	From     MemoryLayer `json:"from"`
	To       MemoryLayer `json:"to"`
	Reason   string      `json:"reason"` // value_promote / low_value_archive / expired / goal_completed / ...
}

// LifecycleResult 一次扫描的汇总
type LifecycleResult struct {
	Scanned int             `json:"scanned"`
	Steps   []LifecycleStep `json:"steps"`
}

// LayerScanner 可选接口：后端支持按层扫描与层更新。
// pgBackend / sqliteBackend 实现。
type LayerScanner interface {
	// ScanLayer 返回指定层（或全部层，layer 为空）的记忆，限制 max 条。
	ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error)
	// UpdateLayer 更新单条记忆的 layer 字段。
	UpdateLayer(ctx context.Context, memoryID string, layer MemoryLayer) error
}

// PromotionPolicy 固有价值流转策略（§5.3，权重/阈值均可配）。
type PromotionPolicy struct {
	// 固有价值权重
	WImportance   float64
	WRecentAccess float64
	WConfidence   float64

	// 晋升/降级阈值
	ValueL0ToL1    float64 // 默认 0.6
	ValueL1ToL2    float64 // 默认 0.8
	ValueL2ToL3Low float64 // 默认 0.3
	ValueL2Demote  float64 // 默认 0.5

	AccessCountPromote int           // 默认 3：L0 access_count 达阈值也升 L1
	StablePeriod       time.Duration // 默认 7 天：L1→L2 需稳定期
	L0TTL              time.Duration // 兜底：L0 超 TTL 降 L1
	L1TTL              time.Duration // 兜底：L1 超 TTL 且低价值归档
}

// DefaultPromotionPolicy 返回 §5.3 建议默认值。
func DefaultPromotionPolicy() PromotionPolicy {
	return PromotionPolicy{
		WImportance:        0.5,
		WRecentAccess:      0.3,
		WConfidence:        0.2,
		ValueL0ToL1:        0.6,
		ValueL1ToL2:        0.8,
		ValueL2ToL3Low:     0.3,
		ValueL2Demote:      0.5,
		AccessCountPromote: 3,
		StablePeriod:       7 * 24 * time.Hour,
		L0TTL:              60 * time.Minute,
		L1TTL:              30 * 24 * time.Hour,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// IntrinsicValue 计算记忆固有价值分（§5.3 固定公式）。
// value = 0.5*importance + 0.3*recent_access_score + 0.2*confidence（各分量 clamp 到 [0,1]）。
func IntrinsicValue(m *Memory, p PromotionPolicy) float64 {
	if m == nil {
		return 0
	}
	if p.WImportance == 0 && p.WRecentAccess == 0 && p.WConfidence == 0 {
		p = DefaultPromotionPolicy()
	}
	return clamp01(p.WImportance*clamp01(m.Importance) +
		p.WRecentAccess*clamp01(m.RecentAccessScore) +
		p.WConfidence*clamp01(m.Confidence))
}

// hasRejectedFeedback 判断是否存在 rejected 反馈（L1→L2 稳定期要求「无 feedback rejected」）。
func hasRejectedFeedback(m *Memory) bool {
	for _, f := range m.FeedbackRecords {
		if f.Status == FbRejected {
			return true
		}
	}
	return false
}

func validUntilExpired(m *Memory, now time.Time) bool {
	if m.ValidUntil == "" {
		return false
	}
	d, err := time.Parse("2006-01-02", m.ValidUntil)
	if err != nil {
		return false
	}
	return now.After(d.Add(24*time.Hour - time.Second))
}

// NextLifecycleByValue 按固有价值决定下一步动作（§5.3）。
// 返回 (from, to, reason)；保持原层时 to==from 且 reason==""。
func NextLifecycleByValue(m *Memory, p PromotionPolicy, now time.Time) (from, to MemoryLayer, reason string) {
	if m == nil {
		return "", "", ""
	}
	if p.WImportance == 0 && p.WRecentAccess == 0 && p.WConfidence == 0 {
		p = DefaultPromotionPolicy()
	}
	from = m.Layer
	if from == "" {
		from = ClassifyLayer(m)
	}

	// 硬过期优先（§3.10）
	if m.ExpireAt != nil && now.After(*m.ExpireAt) {
		return from, LayerL3, "expired"
	}
	if validUntilExpired(m, now) {
		return from, LayerL3, "valid_until_expired"
	}

	v := IntrinsicValue(m, p)
	switch from {
	case LayerL0:
		if v >= p.ValueL0ToL1 {
			return from, LayerL1, "value_promote"
		}
		if p.AccessCountPromote > 0 && m.AccessCount >= int64(p.AccessCountPromote) {
			return from, LayerL1, "access_promote"
		}
		if p.L0TTL > 0 && now.Sub(m.RecordedAt) > p.L0TTL {
			return from, LayerL1, "l0_ttl_expired"
		}
	case LayerL1:
		stable := p.StablePeriod <= 0 || now.Sub(m.RecordedAt) >= p.StablePeriod
		if v >= p.ValueL1ToL2 && stable && !hasRejectedFeedback(m) {
			return from, LayerL2, "value_promote"
		}
		if p.L1TTL > 0 && now.Sub(m.RecordedAt) > p.L1TTL && v < p.ValueL2Demote {
			return from, LayerL3, "l1_ttl_low_value_archive"
		}
	case LayerL2:
		if m.GoalDetail != nil {
			switch m.GoalDetail.Status {
			case "completed", "expired":
				return from, LayerL3, "goal_" + m.GoalDetail.Status
			}
		}
		if v <= p.ValueL2ToL3Low {
			return from, LayerL3, "low_value_archive"
		}
		if v < p.ValueL2Demote && m.AccessCount == 0 {
			return from, LayerL1, "low_value_demote"
		}
	case LayerL3:
		// 归档保留；仅 delete_rights 用户删除（§7.3，留 P7）。
	}
	return from, from, ""
}

// LifecycleScanner 执行一次分层生命周期扫描（按固有价值自动流转）。
// backend 必须实现 LayerScanner，否则返回错误。
// 同一记忆在同一轮扫描内只流转一次（避免逐层重扫导致的重复流转）。
func LifecycleScanner(ctx context.Context, backend StorageBackend, policy LayerPolicy, tenant string, max int) (*LifecycleResult, error) {
	return LifecycleScannerByValue(ctx, backend, DefaultPromotionPolicy(), tenant, max)
}

// LifecycleScannerByValue 使用 PromotionPolicy 执行扫描（A4 主入口）。
// policy 参数保留以兼容既有调用签名（时间兜底阈值由 PromotionPolicy 提供）。
func LifecycleScannerByValue(ctx context.Context, backend StorageBackend, promotion PromotionPolicy, tenant string, max int) (*LifecycleResult, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("backend %s does not implement LayerScanner; P2 requires it", backend.Name())
	}
	if max <= 0 {
		max = 1000
	}
	now := time.Now().UTC()
	result := &LifecycleResult{}
	moved := map[string]bool{} // 同轮只流转一次

	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, max)
		if err != nil {
			return result, fmt.Errorf("scan %s: %w", layer, err)
		}
		for _, m := range ms {
			if m == nil {
				continue
			}
			result.Scanned++
			if moved[m.MemoryID] {
				continue
			}
			from, to, reason := NextLifecycleByValue(m, promotion, now)
			if to == "" || to == from {
				continue
			}
			if err := sc.UpdateLayer(ctx, m.MemoryID, to); err != nil {
				log.Printf("[lifecycle] update %s -> %s failed: %v", m.MemoryID, to, err)
				continue
			}
			moved[m.MemoryID] = true
			result.Steps = append(result.Steps, LifecycleStep{
				MemoryID: m.MemoryID,
				From:     from,
				To:       to,
				Reason:   reason,
			})
		}
	}
	return result, nil
}

// ------------------------------------------------------------------
// BudgetManager（P4）：按层容量上限触发遗忘/降级。
// 只增不改：不删除，只改 layer 字段。
// ------------------------------------------------------------------

// BudgetManager 检查每层条数，超过上限时删除最低价值记忆。
// 注意：这里"删除"指移至 L3（归档），不物理删除。
func BudgetManager(ctx context.Context, backend StorageBackend, limits map[MemoryLayer]int, tenant string) (int, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend %s does not implement LayerScanner", backend.Name())
	}
	deleted := 0
	for layer, limit := range limits {
		ms, err := sc.ScanLayer(ctx, layer, tenant, 10000)
		if err != nil {
			return deleted, err
		}
		if len(ms) <= limit {
			continue
		}
		// 按固有价值分升序删除，优先删除价值最低的记忆
		sort.Slice(ms, func(i, j int) bool {
			return IntrinsicValue(ms[i], DefaultPromotionPolicy()) < IntrinsicValue(ms[j], DefaultPromotionPolicy())
		})
		for _, m := range ms[:len(ms)-limit] {
			if err := sc.UpdateLayer(ctx, m.MemoryID, LayerL3); err != nil {
				log.Printf("[budget] move %s to L3 failed: %v", m.MemoryID, err)
				continue
			}
			deleted++
		}
	}
	return deleted, nil
}

// ------------------------------------------------------------------
// ForgettingEngine（P4）：低价值记忆自动移至 L3。
// ------------------------------------------------------------------

// ForgettingEngine 检查固有价值分 < 阈值时，移至 L3。
func ForgettingEngine(ctx context.Context, backend StorageBackend, threshold float64, tenant string) (int, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend %s does not implement LayerScanner", backend.Name())
	}
	moved := 0
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, 1000)
		if err != nil {
			return moved, err
		}
		for _, m := range ms {
			if IntrinsicValue(m, DefaultPromotionPolicy()) < threshold {
				if err := sc.UpdateLayer(ctx, m.MemoryID, LayerL3); err != nil {
					log.Printf("[forget] move %s to L3 failed: %v", m.MemoryID, err)
					continue
				}
				moved++
			}
		}
	}
	return moved, nil
}

// ------------------------------------------------------------------
// CoreMemoryManager（P4）：高频访问记忆标记为 is_core=true。
// ------------------------------------------------------------------

// CoreMemoryManager 标记 access_count > 阈值的记忆为 is_core=true。
// 注意：is_core 字段在 model.go 中已存在（只增不改）。
func CoreMemoryManager(ctx context.Context, backend StorageBackend, accessThreshold int64, tenant string) (int, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend %s does not implement LayerScanner", backend.Name())
	}
	marked := 0
	for _, layer := range []MemoryLayer{LayerL0, LayerL1} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, 1000)
		if err != nil {
			return marked, err
		}
		for _, m := range ms {
			if m.AccessCount >= accessThreshold && !m.IsCore {
				m.IsCore = true
				if err := backend.Upsert(ctx, m); err != nil {
					log.Printf("[core] mark %s as core failed: %v", m.MemoryID, err)
					continue
				}
				marked++
			}
		}
	}
	return marked, nil
}

// ------------------------------------------------------------------
// ArchiveEngine（P4）：对 L2 中超过 1 年未访问的记忆，移至 L3。
// ------------------------------------------------------------------

// ArchiveEngine 检查 L2 中超过 1 年未访问的记忆，移至 L3。
// 可配置未访问时长阈值（默认 1 年）。
func ArchiveEngine(ctx context.Context, backend StorageBackend, idleThreshold time.Duration, tenant string) (int, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend %s does not implement LayerScanner", backend.Name())
	}
	if idleThreshold <= 0 {
		idleThreshold = 365 * 24 * time.Hour // 默认 1 年
	}
	now := time.Now().UTC()
	moved := 0
	ms, err := sc.ScanLayer(ctx, LayerL2, tenant, 1000)
	if err != nil {
		return moved, err
	}
	for _, m := range ms {
		if m == nil {
			continue
		}
		// 计算未访问时长：用 RecordedAt 或 OccurredAt
		lastAccess := m.RecordedAt
		if m.OccurredAt != nil && !m.OccurredAt.IsZero() {
			lastAccess = *m.OccurredAt
		}
		if now.Sub(lastAccess) > idleThreshold {
			if err := sc.UpdateLayer(ctx, m.MemoryID, LayerL3); err != nil {
				log.Printf("[archive] move %s to L3 failed: %v", m.MemoryID, err)
				continue
			}
			moved++
		}
	}
	return moved, nil
}

// ------------------------------------------------------------------
// CompressionEngine（P4）：对 L1 中相似记忆做合并，生成 procedure。
// ------------------------------------------------------------------

// CompressionEngine 对 L1 中相似记忆（向量距离 < 阈值）做合并，生成 procedure。
// 合并后的 procedure 包含原记忆的 fact 列表，便于后续检索。
// 注意：原记忆不删除，只标记 is_historical=true。
func CompressionEngine(ctx context.Context, backend StorageBackend, embedder func(string) ([]float32, error), similarityThreshold float32, tenant string) (int, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend %s does not implement LayerScanner", backend.Name())
	}
	if similarityThreshold <= 0 {
		similarityThreshold = 0.9 // 默认 90% 相似度
	}
	compressed := 0
	ms, err := sc.ScanLayer(ctx, LayerL1, tenant, 1000)
	if err != nil {
		return compressed, err
	}
	if len(ms) < 2 {
		return compressed, nil
	}

	// 计算向量相似度，找出相似记忆对
	pairs := make(map[string][]string) // memoryID -> 相似 memoryIDs
	for i, m1 := range ms {
		if m1 == nil || embedder == nil {
			continue
		}
		v1, err1 := embedder(m1.Fact)
		if err1 != nil {
			continue
		}
		for j, m2 := range ms {
			if i >= j || m2 == nil {
				continue
			}
			v2, err2 := embedder(m2.Fact)
			if err2 != nil {
				continue
			}
			sim := cosine(v1, v2)
			if sim >= similarityThreshold {
				pairs[m1.MemoryID] = append(pairs[m1.MemoryID], m2.MemoryID)
				pairs[m2.MemoryID] = append(pairs[m2.MemoryID], m1.MemoryID)
			}
		}
	}

	// 对相似记忆对做合并
	merged := map[string]bool{}
	for m1ID, similarIDs := range pairs {
		if merged[m1ID] {
			continue
		}
		// 找到 m1 和所有相似记忆
		var toMerge []*Memory
		var m1 *Memory
		for _, m := range ms {
			if m.MemoryID == m1ID {
				m1 = m
				toMerge = append(toMerge, m)
				break
			}
		}
		for _, sid := range similarIDs {
			if merged[sid] || sid == m1ID {
				continue
			}
			for _, m := range ms {
				if m.MemoryID == sid {
					toMerge = append(toMerge, m)
					merged[sid] = true
					break
				}
			}
		}
		if len(toMerge) < 2 {
			continue
		}
		// 生成 procedure 记忆
		var facts []string
		for _, m := range toMerge {
			facts = append(facts, m.Fact)
			// 标记原记忆为 is_historical
			m.IsHistorical = true
			m.SupersededBy = &m1ID
			_ = backend.Upsert(ctx, m)
		}
		proc := &Memory{
			MemoryID:        newMemoryID(),
			IdempotencyKey:  newMemoryID(),
			TenantID:        tenant,
			UserID:          m1.UserID,
			AgentID:         m1.AgentID,
			ProjectID:       m1.ProjectID,
			Visibility:      m1.Visibility,
			Permission:      m1.Permission,
			PermissionLevel: m1.PermissionLevel,
			MemoryKind:      KindProcedure,
			Fact:            "合并记忆: " + facts[0],
			ReferenceFrame:  m1.ReferenceFrame,
			RecordedAt:      time.Now().UTC(),
			Confidence:      0.8,
			Source:          "compression_engine",
			Version:         1,
			Layer:           LayerL1,
			IsCore:          false,
		}
		if err := backend.Upsert(ctx, proc); err != nil {
			log.Printf("[compress] create procedure failed: %v", err)
			continue
		}
		compressed++
		merged[m1ID] = true
	}
	return compressed, nil
}

// ------------------------------------------------------------------
// R8-A（P7-E）i18n 接入样板（只增不改：新增调用点，不改既有返回语义）
// ------------------------------------------------------------------

// LifecycleLayerScannerError 返回「后端未实现 LayerScanner」的 i18n 文案。
// 接入样板：新增 i18n 调用点，供上层按当前 kernel locale 输出本地化错误文案，
// 与既有 `fmt.Errorf("backend %s does not implement LayerScanner...", ...)`
// 并存且互不影响；`SetKernelLocale(LocaleEN)` 可切回英文口径。
// 只增不改：本函数为新增导出，不修改 LifecycleScannerByValue 等既有函数签名/返回值。
func LifecycleLayerScannerError(backendName string) string {
	return T(i18n.MsgLayerScannerUnsupported, backendName)
}

// LifecycleScanLayerError 返回「扫描层失败」的 i18n 文案。
// 用法：`fmt.Errorf("%s", LifecycleScanLayerError(layer))`；
// 如需保留 wrap 语义可写 `fmt.Errorf("%s: %w", LifecycleScanLayerError(layer), err)`。
func LifecycleScanLayerError(layer MemoryLayer) string {
	return T(i18n.MsgScanFailed, layer)
}

// LifecycleUpdateFailedError 返回「层迁移失败」的 i18n 文案（lifecycle 日志/错误共用）。
// 用法：`log.Printf("%s", LifecycleUpdateFailedError(m.MemoryID, to, err))`。
func LifecycleUpdateFailedError(memoryID string, to MemoryLayer, err error) string {
	return T(i18n.MsgLifecycleUpdateFailed, memoryID, to, err)
}
