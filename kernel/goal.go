// GoalMemory 与权限三元组（P6 完整实现）
// 真源：lightmem.md §3.8 goal、§7.1 权限
// 光明语言：用状态机表达目标生命周期
package kernel

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"
)

// GoalManager Goal 记忆管理
type GoalManager struct {
	backend StorageBackend
}

func NewGoalManager(backend StorageBackend) *GoalManager {
	return &GoalManager{backend: backend}
}

// GoalState 目标状态（光明语言：状态机）
type GoalState string

const (
	GoalActive    GoalState = "active"
	GoalPaused    GoalState = "paused"
	GoalCompleted GoalState = "completed"
	GoalExpired   GoalState = "expired"
	GoalArchived  GoalState = "archived"
)

// GoalTransition 状态转移规则（光明语言：声明式）
var GoalTransitions = map[GoalState][]GoalState{
	GoalActive:    {GoalPaused, GoalCompleted, GoalExpired},
	GoalPaused:    {GoalActive, GoalCompleted, GoalExpired},
	GoalCompleted: {},
	GoalExpired:   {GoalArchived},
	GoalArchived:  {},
}

// CanTransition 检查状态转移是否合法
func CanTransition(from, to GoalState) bool {
	allowed, ok := GoalTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// CreateGoal 创建目标记忆（旧签名，保留转发，零破坏）。
// 新代码请用 CreateGoalWithTenant 显式传 tenant。
func (m *GoalManager) CreateGoal(ctx context.Context, goal, deadline string, kpi *KPI, constraints []string, actor string) (*Memory, error) {
	return m.CreateGoalWithTenant(ctx, goal, deadline, kpi, constraints, actor, "default")
}

// CreateGoalWithTenant 创建目标记忆（解 G5：tenant 由调用方传入）。
func (m *GoalManager) CreateGoalWithTenant(ctx context.Context, goal, deadline string, kpi *KPI, constraints []string, actor, tenant string) (*Memory, error) {
	m2 := &Memory{
		MemoryID:       newMemoryID(),
		IdempotencyKey: newMemoryID(),
		TenantID:       tenant,
		UserID:         actor,
		MemoryKind:     KindGoal,
		Fact:           goal,
		Source:         actor,
		RecordedAt:     time.Now().UTC(),
		Confidence:     0.9,
		Version:        1,
		Layer:          LayerL1,
		GoalDetail: &GoalDetail{
			Goal:        goal,
			KPI:         kpi,
			Constraints: constraints,
			Status:      string(GoalActive),
			Progress:    0,
			Deadline:    deadline,
		},
	}
	if err := m.backend.Upsert(ctx, m2); err != nil {
		return nil, err
	}
	GetAuditLogger().Log("goal_create", m2.MemoryID, m2.TenantID, m2.UserID, actor, goal)
	return m2, nil
}

// UpdateGoalProgress 更新目标进度（旧签名，保留转发，零破坏）。
// 新代码请用 UpdateGoalProgressWithTenant 显式传 tenant。
func (m *GoalManager) UpdateGoalProgress(ctx context.Context, memoryID string, progress float64, actor string) error {
	return m.UpdateGoalProgressWithTenant(ctx, memoryID, progress, actor, "default")
}

// UpdateGoalProgressWithTenant 更新目标进度（解 G5：tenant/viewer 由调用方传入）。
func (m *GoalManager) UpdateGoalProgressWithTenant(ctx context.Context, memoryID string, progress float64, actor, tenant string) error {
	existing, err := m.backend.Get(ctx, memoryID, actor, tenant)
	if err != nil {
		return err
	}
	if existing.MemoryKind != KindGoal || existing.GoalDetail == nil {
		return fmt.Errorf("not a goal memory")
	}

	oldState := GoalState(existing.GoalDetail.Status)
	newState := oldState

	// 光明语言：状态机规则
	if progress >= 1.0 {
		newState = GoalCompleted
	} else if progress < 0 {
		newState = GoalActive
	}

	// 检查状态转移合法性
	if oldState != newState && !CanTransition(oldState, newState) {
		return fmt.Errorf("invalid state transition: %s -> %s", oldState, newState)
	}

	existing.GoalDetail.Progress = progress
	existing.GoalDetail.Status = string(newState)
	existing.RecordedAt = time.Now().UTC()

	if err := m.backend.Upsert(ctx, existing); err != nil {
		return err
	}
	GetAuditLogger().Log("goal_progress", memoryID, existing.TenantID, existing.UserID, actor, fmt.Sprintf("progress=%.1f state=%s->%s", progress, oldState, newState))
	return nil
}

// SetGoalState 设置目标状态（旧签名，保留转发，零破坏）。
// 新代码请用 SetGoalStateWithTenant 显式传 tenant。
func (m *GoalManager) SetGoalState(ctx context.Context, memoryID string, state GoalState, actor string) error {
	return m.SetGoalStateWithTenant(ctx, memoryID, state, actor, "default")
}

// SetGoalStateWithTenant 设置目标状态（解 G5：tenant/viewer 由调用方传入）。
func (m *GoalManager) SetGoalStateWithTenant(ctx context.Context, memoryID string, state GoalState, actor, tenant string) error {
	existing, err := m.backend.Get(ctx, memoryID, actor, tenant)
	if err != nil {
		return err
	}
	if existing.MemoryKind != KindGoal || existing.GoalDetail == nil {
		return fmt.Errorf("not a goal memory")
	}

	oldState := GoalState(existing.GoalDetail.Status)
	if !CanTransition(oldState, state) {
		return fmt.Errorf("invalid state transition: %s -> %s", oldState, state)
	}

	existing.GoalDetail.Status = string(state)
	existing.RecordedAt = time.Now().UTC()

	if err := m.backend.Upsert(ctx, existing); err != nil {
		return err
	}
	GetAuditLogger().Log("goal_state", memoryID, existing.TenantID, existing.UserID, actor, fmt.Sprintf("%s->%s", oldState, state))
	return nil
}

// CheckPermission 检查权限（§7.1 服务端强制校验）
func CheckPermission(m *Memory, user, action string) bool {
	if m == nil {
		return false
	}
	switch action {
	case "read":
		if m.Permission.Owner == user {
			return true
		}
		for _, r := range m.Permission.Readers {
			if r == user {
				return true
			}
		}
		return m.Visibility == VisTenant || m.Visibility == VisCrossTenant
	case "write":
		if m.Permission.Owner == user {
			return true
		}
		for _, w := range m.Permission.Writers {
			if w == user {
				return true
			}
		}
		return false
	case "delete":
		return m.Permission.Owner == user
	}
	return false
}

// RevokeAccess 撤销访问权（P6 撤销传播）
func RevokeAccess(m *Memory, user string) {
	if m == nil {
		return
	}
	// 从 Readers 中移除
	newReaders := []string{}
	for _, r := range m.Permission.Readers {
		if r != user {
			newReaders = append(newReaders, r)
		}
	}
	m.Permission.Readers = newReaders
	// 从 Writers 中移除
	newWriters := []string{}
	for _, w := range m.Permission.Writers {
		if w != user {
			newWriters = append(newWriters, w)
		}
	}
	m.Permission.Writers = newWriters
	log.Printf("[revoke] %s removed from memory %s", user, m.MemoryID)
}

// ShareMemory 共享记忆（权限三元组 + visibility）
func ShareMemory(ctx context.Context, backend StorageBackend, memoryID, fromUser, toUser string, canWrite bool) error {
	m, err := backend.Get(ctx, memoryID, fromUser, "default")
	if err != nil {
		return err
	}
	if m.Permission.Owner != fromUser {
		return fmt.Errorf("only owner can share")
	}
	if canWrite {
		m.Permission.Writers = append(m.Permission.Writers, toUser)
	} else {
		m.Permission.Readers = append(m.Permission.Readers, toUser)
	}
	if err := backend.Upsert(ctx, m); err != nil {
		return err
	}
	GetAuditLogger().Log("share", memoryID, m.TenantID, m.UserID, fromUser, fmt.Sprintf("to=%s write=%v", toUser, canWrite))
	return nil
}

// SessionCompressor 会话压缩（P6 完整实现）
func SessionCompressor(ctx context.Context, backend StorageBackend, tenant, user string, maxMemories int) (int, error) {
	if maxMemories <= 0 {
		maxMemories = 50
	}
	sc, ok := backend.(LayerScanner)
	if !ok {
		return 0, fmt.Errorf("backend does not implement LayerScanner")
	}
	ms, err := sc.ScanLayer(ctx, LayerL0, tenant, 1000)
	if err != nil {
		return 0, err
	}
	if len(ms) <= maxMemories {
		return 0, nil
	}
	// 按固有价值分升序，压缩低价值记忆
	sort.Slice(ms, func(i, j int) bool {
		return IntrinsicValue(ms[i], DefaultPromotionPolicy()) < IntrinsicValue(ms[j], DefaultPromotionPolicy())
	})
	compressed := 0
	for _, m := range ms[:len(ms)-maxMemories] {
		if m.MemoryKind == KindFact {
			m.Layer = LayerL2
			if err := backend.Upsert(ctx, m); err != nil {
				log.Printf("[compress] %s: %v", m.MemoryID, err)
				continue
			}
			compressed++
		}
	}
	return compressed, nil
}

// BatchImport 批量导入（P6）
func BatchImport(ctx context.Context, backend StorageBackend, tenant, user string, memories []*Memory) (int, error) {
	imported := 0
	for _, m := range memories {
		if m == nil {
			continue
		}
		if m.MemoryID == "" {
			m.MemoryID = newMemoryID()
		}
		if m.IdempotencyKey == "" {
			m.IdempotencyKey = m.MemoryID
		}
		if m.TenantID == "" {
			m.TenantID = tenant
		}
		if m.UserID == "" {
			m.UserID = user
		}
		if m.RecordedAt.IsZero() {
			m.RecordedAt = time.Now().UTC()
		}
		if m.Layer == "" {
			m.Layer = ClassifyLayer(m)
		}
		if err := backend.Upsert(ctx, m); err != nil {
			log.Printf("[import] %s: %v", m.MemoryID, err)
			continue
		}
		imported++
	}
	GetAuditLogger().Log("batch_import", "", tenant, user, "system", fmt.Sprintf("imported=%d", imported))
	return imported, nil
}

// BatchExport 批量导出（P6）
func BatchExport(ctx context.Context, backend StorageBackend, tenant, user string, kinds []MemoryKind) ([]*Memory, error) {
	sc, ok := backend.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("backend does not implement LayerScanner")
	}
	var all []*Memory
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, 10000)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if m == nil {
				continue
			}
			if m.UserID != user && user != "" {
				continue
			}
			if len(kinds) > 0 {
				ok := false
				for _, k := range kinds {
					if m.MemoryKind == k {
						ok = true
						break
					}
				}
				if !ok {
					continue
				}
			}
			all = append(all, m)
		}
	}
	return all, nil
}
