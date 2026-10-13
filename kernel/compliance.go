// 合规操作可选接口（P7-A GDPR，§7.3 删除权 / §7.4 数据可携 / §7.5 被遗忘权）
// 真源：lightmem.md §7.3「删除权」、§7.4「数据可携权（导出）」、§7.5「被遗忘权」。
//
// 设计约束（R5-C 红线条款）：
//   - 只增不改：本文件不修改 StorageBackend 接口的既有方法签名与语义，
//     也不改动 Memory / RouterContext / RecalledCandidate 字段语义。
//   - GDPR 函数通过类型断言调用本可选接口；不支持的后端返回明确错误。
//   - 所有删除/导出均为物理操作（含 hop 链 / feedback / provenance 内联字段随行删除）。
package kernel

import (
	"context"
	"fmt"
)

// ComplianceBackend 合规操作可选接口（GDPR 三件套）。
// 仅增不改：StorageBackend 不嵌入本接口；GDPR 函数以类型断言调用，
// 避免强制所有后端实现删除/导出（符合「只增不改 kernel Schema」红线）。
type ComplianceBackend interface {
	// ExportByScope 返回 (tenant, userID) 作用域下的全部记忆（含内联的 hop 链、
	// feedback、provenance 字段）。用于 §7.4 数据可携权（导出）。
	// 返回的切片按 (recorded_at, memory_id) 升序，保证确定性。
	ExportByScope(ctx context.Context, tenant, userID string) ([]*Memory, error)

	// DeleteByScope 删除 (tenant, userID) 作用域下的全部记忆（级联：含该用户的
	// 所有 kind、hop 链、feedback、provenance）。返回被删除的 memory_id 列表（升序）。
	// 用于 §7.3 删除权（级联删除）。
	DeleteByScope(ctx context.Context, tenant, userID string) ([]string, error)

	// DeleteByID 删除单条记忆（物理移除，不留可恢复 trace）。
	// 用于 §7.5 被遗忘权。memory 不存在时视为已遗忘（返回 nil，幂等）。
	DeleteByID(ctx context.Context, memoryID string) error
}

// asComplianceBackend 类型断言到合规接口；不支持时返回错误（带后端名提示）。
func asComplianceBackend(backend StorageBackend) (ComplianceBackend, error) {
	if cb, ok := backend.(ComplianceBackend); ok {
		return cb, nil
	}
	return nil, fmt.Errorf("backend %s does not support compliance operations (GDPR)", backend.Name())
}
