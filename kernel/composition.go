// 组合记忆（R6-B 路：连续学习 + 存储期组合，千脑「组合物体」）。
//
// 真源：lightmem.md §4.1 只增原则 + 任务书 R6-B §B.2。
//
// 设计：
//   - Memory 新增可选字段 Components []string（子记忆 ID；只增不改，见 model.go）。
//   - Compose：把多条子记忆组合到一条父记忆之下；父记忆记录去重保序的子记忆 ID，
//     子记忆参考系从父记忆继承（父空字段由子覆盖——确定性）。
//   - Decompose：拆解父记忆，返回子记忆并清空父的 Components（子记忆保留）。
//   - ExpandComposition：检索集成用——深度受限递归展开组合记忆的后代子事实。
//   - APIServer.expandCompositions：RecallAndFuse 对组合记忆候选展开其子事实。
//
// 只增不改：本文件不修改 model.go 既有字段语义；Components 为纯新增可选字段；
// 展开仅在候选 Memory 含非空 Components 时触发，对不含组合的既有数据零影响。
package kernel

import (
	"context"
	"errors"
	"fmt"
)

// errNilParent 组合操作的非法入参（nil 父记忆）。
var errNilParent = errors.New("composition: nil parent")

// IsComposite 判断记忆是否为组合记忆（含非空 Components）。
func IsComposite(m *Memory) bool {
	return m != nil && len(m.Components) > 0
}

// dedupeComponents 去重并保序（确定性）：保留首次出现的非空子记忆 ID。
func dedupeComponents(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// inheritReferenceFrame 子记忆参考系继承 + 覆盖（确定性）：
// 子记忆 ReferenceFrame 中**为空**的字段继承父记忆对应值；子记忆已设字段保留（覆盖）。
// 这样每条子事实都携带完整参考系，且父记忆的聚合上下文（tenant/user/project/scene/
// task/entity/agent/category）自动下发给子事实——对应千脑「一物多帧」的组织方式。
func inheritReferenceFrame(parent, child *ReferenceFrame) {
	if child.Tenant == "" {
		child.Tenant = parent.Tenant
	}
	if child.User == "" {
		child.User = parent.User
	}
	if child.Agent == "" {
		child.Agent = parent.Agent
	}
	if child.Project == "" {
		child.Project = parent.Project
	}
	if child.Entity == "" {
		child.Entity = parent.Entity
	}
	if child.Scene == "" {
		child.Scene = parent.Scene
	}
	if child.Task == "" {
		child.Task = parent.Task
	}
	if child.Category == "" {
		child.Category = parent.Category
	}
}

// Compose 组合记忆：把 children 组合到 parent 之下（千脑「组合物体」的存储期建模）。
//
//   - parent.Components 设为去重保序的子记忆 ID；
//   - 每条子记忆的 ReferenceFrame 从 parent 继承（inheritReferenceFrame）；
//   - 子记忆与父记忆分别 Upsert（幂等写入口走调用方；此处直接落后端）。
//
// 确定性：Components 顺序 = children 入参顺序（去重保序）；子记忆 frame 合并规则固定。
func Compose(ctx context.Context, b StorageBackend, parent *Memory, children []*Memory) error {
	if parent == nil {
		return errNilParent
	}
	ids := make([]string, 0, len(children))
	for _, ch := range children {
		if ch == nil {
			continue
		}
		inheritReferenceFrame(&parent.ReferenceFrame, &ch.ReferenceFrame)
		if err := b.Upsert(ctx, ch); err != nil {
			return fmt.Errorf("compose upsert child %s: %w", ch.MemoryID, err)
		}
		ids = append(ids, ch.MemoryID)
	}
	parent.Components = dedupeComponents(ids)
	if err := b.Upsert(ctx, parent); err != nil {
		return fmt.Errorf("compose upsert parent %s: %w", parent.MemoryID, err)
	}
	return nil
}

// Decompose 拆解组合记忆：返回 component 子记忆（按 Components 顺序），并清空父记忆的
// Components，解除父子绑定。子记忆本身保留（存储期拆解不改变子记忆存在性）。
// 确定性：按 Components 顺序返回；子记忆缺失（可能已被遗忘）时跳过，不报错中断。
func Decompose(ctx context.Context, b StorageBackend, parent *Memory) ([]*Memory, error) {
	if parent == nil {
		return nil, errNilParent
	}
	ids := dedupeComponents(parent.Components)
	out := make([]*Memory, 0, len(ids))
	for _, id := range ids {
		m, err := b.Get(ctx, id, parent.UserID, parent.TenantID)
		if err != nil || m == nil {
			continue
		}
		out = append(out, m)
	}
	parent.Components = nil
	if err := b.Upsert(ctx, parent); err != nil {
		return out, fmt.Errorf("decompose upsert parent %s: %w", parent.MemoryID, err)
	}
	return out, nil
}

// ExpandComposition 递归展开组合记忆的后代子事实（千脑「组合物体」检索集成用）。
//
//   - 返回 parent 的全部后代子记忆（按 Components 顺序深度优先，深度上限 maxDepth）；
//   - 已访问的记忆不重复展开（防环）；子记忆不可见（Get 失败）时跳过；
//   - 确定性：固定顺序、固定深度。
func ExpandComposition(ctx context.Context, b StorageBackend, parent *Memory, maxDepth int) ([]*Memory, error) {
	if parent == nil || maxDepth <= 0 {
		return nil, nil
	}
	visited := map[string]struct{}{parent.MemoryID: {}}
	out := make([]*Memory, 0, len(parent.Components))
	var rec func(m *Memory, depth int)
	rec = func(m *Memory, depth int) {
		for _, id := range dedupeComponents(m.Components) {
			if _, ok := visited[id]; ok {
				continue
			}
			visited[id] = struct{}{}
			ch, err := b.Get(ctx, id, m.UserID, m.TenantID)
			if err != nil || ch == nil {
				continue
			}
			out = append(out, ch)
			if depth+1 < maxDepth {
				rec(ch, depth+1)
			}
		}
	}
	rec(parent, 0)
	return out, nil
}

// expandCompositions 检索集成：对融合结果中**组合记忆**候选，按 Components 展开子事实，
// 插入到父候选之后（保持融合排序，子事实紧邻父记忆）。
//
//   - 仅当候选的 Memory 含非空 Components 时触发；
//   - 子事实不可见或已存在于候选集时跳过（防环、防重复，确定性）；
//   - 不改动原始融合分（子事实沿用父记忆 Score，紧邻父之后）；
//   - 调用方仅当 rc.ComposeExpand=true 时进入本函数；关闭时为 isolate 复现
//     （等价于「存储期组合」特性上线前的逐位行为）。
func (s *APIServer) expandCompositions(ctx context.Context, rc RouterContext, fused []FusedResult) []FusedResult {
	if len(fused) == 0 {
		return fused
	}
	existing := make(map[string]struct{}, len(fused))
	for _, f := range fused {
		if f.MemoryID != "" {
			existing[f.MemoryID] = struct{}{}
		}
	}
	out := make([]FusedResult, 0, len(fused))
	for _, f := range fused {
		out = append(out, f)
		if f.Memory == nil || len(f.Memory.Components) == 0 {
			continue
		}
		expanded, err := ExpandComposition(ctx, s.backend, f.Memory, 3)
		if err != nil {
			continue
		}
		for _, ch := range expanded {
			if ch == nil || ch.MemoryID == "" {
				continue
			}
			if _, dup := existing[ch.MemoryID]; dup {
				continue
			}
			existing[ch.MemoryID] = struct{}{}
			out = append(out, FusedResult{
				MemoryID:   ch.MemoryID,
				Fact:       ch.Fact,
				Layer:      ch.Layer,
				Score:      f.Score, // 与父记忆同分，紧邻父之后
				Confidence: ch.Confidence,
				Source:     "composition_expand",
				Memory:     ch,
			})
		}
	}
	return out
}
