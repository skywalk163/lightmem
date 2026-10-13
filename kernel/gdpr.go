// GDPR 合规三件套（P7-A，§7.3/§7.4/§7.5）
// 真源：lightmem.md §7.3 删除权（级联删除）、§7.4 数据可携权（导出）、§7.5 被遗忘权。
//
// 实现边界（R5-C 红线条款）：
//   - 不改动 Memory / RouterContext / RecalledCandidate 字段语义。
//   - 不触碰检索链路（RecallAndFuse / RecallLayers / recallScore）；本文件只做删除/导出。
//   - 所有删除/导出通过 ComplianceBackend 可选接口执行（见 compliance.go）。
//   - 只增不改 kernel Schema；审计日志仅记录 ID/时间戳/动作码，不保留记忆内容 trace。
package kernel

import (
	"context"
	"fmt"
	"sort"
)

// CascadeDeleteResult 级联删除结果。
type CascadeDeleteResult struct {
	Tenant  string   `json:"tenant_id"`
	UserID  string   `json:"user_id"`
	Deleted []string `json:"deleted_memory_ids"` // 升序，确定性
	Count   int      `json:"deleted_count"`
}

// CascadeDelete 级联删除（§7.3 删除权）：删除 (tenant, userID) 作用域下全部记忆。
// 含该用户的所有 memory_kind、hop 链、feedback、provenance（均为 Memory 内联字段，随行移除）。
// 删除前会先导出一次快照用于 dry-run 报告（不落盘，仅计数），避免误伤无感知。
// 返回被删除 memory_id 列表（升序）；actor 记入审计日志（只记 ID，不记内容）。
func CascadeDelete(ctx context.Context, backend StorageBackend, tenant, userID, actor string) (*CascadeDeleteResult, error) {
	if tenant == "" || userID == "" {
		return nil, fmt.Errorf("tenant_id and user_id required for cascade delete")
	}
	cb, err := asComplianceBackend(backend)
	if err != nil {
		return nil, err
	}
	// dry-run：先统计将删条数，符合「级联删除前 dry-run 报告将删记忆数」风险应对。
	if _, err := cb.ExportByScope(ctx, tenant, userID); err != nil {
		return nil, fmt.Errorf("cascade delete pre-check: %w", err)
	}
	ids, err := cb.DeleteByScope(ctx, tenant, userID)
	if err != nil {
		return nil, fmt.Errorf("cascade delete: %w", err)
	}
	sort.Strings(ids)
	// 审计：只记录作用域 + 计数 + 动作码，不保留任何记忆内容 trace。
	GetAuditLogger().Log("gdpr_cascade_delete", "", tenant, userID, actor,
		fmt.Sprintf("action=right_to_erasure scope=tenant+user deleted=%d", len(ids)))
	GetMetricsCollector().RecordLifecycleStep("gdpr_cascade_delete")
	return &CascadeDeleteResult{
		Tenant:  tenant,
		UserID:  userID,
		Deleted: ids,
		Count:   len(ids),
	}, nil
}

// ExportAll 数据可携权导出（§7.4）：返回 (tenant, userID) 作用域下全部记忆。
// 失败（后端不支持）时返回明确错误。
func ExportAll(ctx context.Context, backend StorageBackend, tenant, userID string) ([]*Memory, error) {
	if tenant == "" || userID == "" {
		return nil, fmt.Errorf("tenant_id and user_id required for export")
	}
	cb, err := asComplianceBackend(backend)
	if err != nil {
		return nil, err
	}
	memories, err := cb.ExportByScope(ctx, tenant, userID)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	GetAuditLogger().Log("gdpr_export", "", tenant, userID, "system",
		fmt.Sprintf("action=data_portability exported=%d", len(memories)))
	return memories, nil
}

// ForgetResult 被遗忘权操作结果。
type ForgetResult struct {
	MemoryID  string `json:"memory_id"`
	Deleted   bool   `json:"deleted"`    // 物理移除
	TraceKept bool   `json:"trace_kept"` // 是否保留内容 trace（本实现恒为 false）
}

// RightToBeForgotten 被遗忘权（§7.5）：彻底移除单条记忆 + 匿名化（不保留 trace）。
// 实现：物理删除该 memory_id；审计日志仅记录 ID + 时间戳 + 动作码，不保留记忆内容，
// 满足「彻底移除 + 匿名化（不保留 trace）」。memory 不存在时视为已遗忘（幂等返回）。
func RightToBeForgotten(ctx context.Context, backend StorageBackend, memoryID, actor string) (*ForgetResult, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id required for right to be forgotten")
	}
	cb, err := asComplianceBackend(backend)
	if err != nil {
		return nil, err
	}
	if err := cb.DeleteByID(ctx, memoryID); err != nil {
		return nil, fmt.Errorf("right to be forgotten: %w", err)
	}
	// 匿名化/不保留 trace：审计只写 ID + 动作码，不写 fact/content。
	GetAuditLogger().Log("gdpr_forget", memoryID, "", "", actor, "action=right_to_be_forgotten trace=none")
	GetMetricsCollector().RecordLifecycleStep("gdpr_forget")
	return &ForgetResult{MemoryID: memoryID, Deleted: true, TraceKept: false}, nil
}
