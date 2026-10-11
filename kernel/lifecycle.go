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
	"time"
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
