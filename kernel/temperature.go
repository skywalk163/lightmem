// 温度分层模型（§2.2 四层记忆模型）
// L0 工作记忆 → L1 动态记忆 → L2 静态记忆 → L3 归档记忆
// 核心：热容量固定 + 遗忘优先于扩容（§2.4）；分类决定初始落层（§2.3）。
package kernel

import "time"

// LayerPolicy 分层读写策略（§2.4 热容量固定 + 遗忘优先）
type LayerPolicy struct {
	// 热容量上限（超出触发压缩/降级，不无限扩）
	L0MaxItems int // 建议 200 轮会话
	L1MaxItems int // 建议 2000~10000 条

	// 半衰期（TTL）：超时降级
	L0TTL time.Duration // 建议 60min
	L1TTL time.Duration // 建议 30天

	// 晋升阈值：L1 访问达到该次数升 L2
	L1PromoteAccess int // 建议 10 次

	// 核心记忆块预算（§3.7）：L0 中留给核心块的容量占比
	CoreBlockRatio float64 // 建议 0.4（≤ L0 预算 40%）
}

// DefaultLayerPolicy 返回符合 §2.4 的默认策略
func DefaultLayerPolicy() LayerPolicy {
	return LayerPolicy{
		L0MaxItems:      200,
		L1MaxItems:      10000,
		L0TTL:           60 * time.Minute,
		L1TTL:           30 * 24 * time.Hour,
		L1PromoteAccess: 10,
		CoreBlockRatio:  0.4,
	}
}

// ClassifyLayer 按 memory_kind + 稳定性决定初始落层（§2.3 表）
func ClassifyLayer(m *Memory) MemoryLayer {
	if m == nil {
		return LayerL1
	}
	switch m.MemoryKind {
	case KindGoal:
		// goal 长期保留（§2.3）
		return LayerL2
	case KindFact:
		// fact 稳定落 L2；临时（有 valid_until 或低置信度）落 L1
		if m.ValidUntil != "" || m.Confidence < 0.8 {
			return LayerL1
		}
		return LayerL2
	case KindProcedure:
		// procedure L1/L2，跨 Agent 共享晋升 L2
		if m.RecentAccessScore >= float64(DefaultLayerPolicy().L1PromoteAccess) {
			return LayerL2
		}
		return LayerL1
	case KindPreference:
		// preference 默认 L1，长期偏好晋升 L2
		if m.RecentAccessScore >= float64(DefaultLayerPolicy().L1PromoteAccess) {
			return LayerL2
		}
		return LayerL1
	case KindEvent:
		// event 默认 L1，过期降 L3
		return LayerL1
	default:
		return LayerL1
	}
}

// NextLifecycle 根据访问/时间决定下一步动作（§3.10 Lifecycle）
// 返回：(from, to, reason)；保持原层返回 (layer, layer, "")。
func NextLifecycle(m *Memory, policy LayerPolicy, now time.Time) (from, to MemoryLayer, reason string) {
	if m == nil {
		return "", "", ""
	}
	from = m.Layer
	if from == "" {
		from = ClassifyLayer(m)
	}

	// 过期（§3.10）：expire_at 已过 → L3
	if m.ExpireAt != nil && now.After(*m.ExpireAt) {
		return from, LayerL3, "expired"
	}

	// valid_until 已过 → L3
	if m.ValidUntil != "" {
		if d, err := time.Parse("2006-01-02", m.ValidUntil); err == nil {
			if now.After(d.Add(24*time.Hour - time.Second)) {
				return from, LayerL3, "valid_until_expired"
			}
		}
	}

	switch from {
	case LayerL0:
		// L0 超 TTL 降 L1
		if policy.L0TTL > 0 && now.Sub(m.RecordedAt) > policy.L0TTL {
			return from, LayerL1, "l0_ttl_expired"
		}
	case LayerL1:
		// L1 超 TTL 或达到晋升阈值
		ttlExpired := policy.L1TTL > 0 && now.Sub(m.RecordedAt) > policy.L1TTL
		promoted := policy.L1PromoteAccess > 0 && m.AccessCount >= int64(policy.L1PromoteAccess)
		if ttlExpired && !promoted {
			return from, LayerL3, "l1_ttl_expired"
		}
		if promoted {
			return from, LayerL2, "access_promote"
		}
	case LayerL2:
		// L2 长期保留，除非 goal 完成/过期
		if m.GoalDetail != nil {
			switch m.GoalDetail.Status {
			case "completed", "expired":
				return from, LayerL3, "goal_" + m.GoalDetail.Status
			}
		}
	case LayerL3:
		// L3 归档保留；仅 delete_rights 用户删除
	}
	return from, from, ""
}
