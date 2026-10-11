// GoalMemory 与权限三元组（P6）
// 真源：lightmem.md §3.8 goal、§7.1 权限
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// GoalManager Goal 记忆管理
type GoalManager struct {
	backend StorageBackend
}

func NewGoalManager(backend StorageBackend) *GoalManager {
	return &GoalManager{backend: backend}
}

// CreateGoal 创建目标记忆
func (m *GoalManager) CreateGoal(ctx context.Context, goal, deadline string, kpi *KPI, constraints []string, actor string) (*Memory, error) {
	m2 := &Memory{
		MemoryID:       newMemoryID(),
		IdempotencyKey: newMemoryID(),
		TenantID:       "default",
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
			Status:      "active",
			Progress:    0,
			Deadline:    deadline,
		},
	}
	if _, err := m.backend.Upsert(ctx, m2); err != nil {
		return nil, err
	}
	GetAuditLogger().Log("goal_create", m2.MemoryID, m2.TenantID, m2.UserID, actor, goal)
	return m2, nil
}

// UpdateGoalProgress 更新目标进度
func (m *GoalManager) UpdateGoalProgress(ctx context.Context, memoryID string, progress float64, actor string) error {
	existing, err := m.backend.Get(ctx, memoryID, actor, "default")
	if err != nil {
		return err
	}
	if existing.MemoryKind != KindGoal || existing.GoalDetail == nil {
		return fmt.Errorf("not a goal memory")
	}
	existing.GoalDetail.Progress = progress
	if progress >= 1.0 {
		existing.GoalDetail.Status = "completed"
	}
	existing.RecordedAt = time.Now().UTC()
	if err := m.backend.Upsert(ctx, existing); err != nil {
		return err
	}
	GetAuditLogger().Log("goal_progress", memoryID, existing.TenantID, existing.UserID, actor, fmt.Sprintf("progress=%.1f", progress))
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

// SessionCompressor 会话压缩（P6）
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
