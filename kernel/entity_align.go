// kernel/entity_align.go — R13-A 跨 Agent 参考系对齐前置（Monty 建议 5）。
//
// 理论依据（lightmem/monty.md §4.6，Thousand Brains Project，MIT 许可）：
//
//	Monty 中 vote 是「相对发送者身体」的，接收方必须把票变换到**自己**的参考系；
//	且投票不是 bag-of-features，而依赖特征在世界的相对排布。因此跨 Agent
//	融合前必须先把不同 Agent 的 entity/scene/task 语境对齐到同一参考系。
//
// lightmem 现状（§3.2）：visibility=shared_user 让同用户下多 Agent 共享记忆，
// 但**同一事实在不同 Agent 的 entity/scene/task 语境下可能指代不同**——
// 直接按 id 融合就是 monty.md §4.10 所说的「张冠李戴」。
//
// 与既有实现的边界（任务书红线 1/2/3「只增不改」）：
//   - 本文件为**独立前置函数**，不修改 kernel/voting.go 的 ConsensusVoter /
//     Interpretation / MatchFrames 语义，也不改 rank_fusion.go 的融合路径；
//   - **默认路径不调用本函数**：只有调用方在跨 Agent 融合前显式调用
//     AlignCrossAgentFusion 才会生效，既有召回/融合逐位不变；
//   - 不新增 / 不改动 Memory 既有 Schema 字段：对齐结论为计算期结构，不落库；
//   - visibility 取值以代码现状为准（kernel/model.go:21-26 的 VisPrivate /
//     VisSharedUser / VisTenant / VisCrossTenant），不引入新枚举；跨 Agent 共享
//     的可见性硬过滤仍由既有 canView / 查询层负责，本函数不放松任何权限判定。
//
// 确定性（任务书红线 4）：纯字符串比对，无随机源；分组/输出顺序按
// MemoryID 字典序，不依赖 map 迭代序与 time.Now。
package kernel

import (
	"sort"
	"strings"
	"unicode"
)

// EntityAlignPolicy 对齐判定策略参数。
type EntityAlignPolicy struct {
	// RequireScene 是否要求 scene 语境一致（默认 true）。
	// 同 entity 但 scene 不同（如「会议」vs「工单」）视为未对齐——Monty 的
	// 「同一特征在不同参考系下指代不同」正对应此情形。
	RequireScene bool
	// RequireTask 是否要求 task 语境一致（默认 true）。
	RequireTask bool
	// RequireCategory 是否要求 category 语境一致（默认 false）。
	// category 粒度较粗，默认不参与对齐，避免误拒。
	RequireCategory bool
}

// DefaultAlignPolicy 返回默认策略：entity 必须非空，且 scene / task 一致
// （非空且归一化后相等）才视为已对齐。
func DefaultAlignPolicy() EntityAlignPolicy {
	return EntityAlignPolicy{RequireScene: true, RequireTask: true}
}

// AlignStatus 单条记忆的对齐状态。
type AlignStatus string

const (
	// AlignAligned 与参考系（或所属分组）已对齐，允许融合。
	AlignAligned AlignStatus = "aligned"
	// AlignUnaligned 未对齐（实体/场景/任务语境与参考系或分组键冲突）。
	AlignUnaligned AlignStatus = "unaligned"
	// AlignUnknown 无法判定（关键坐标缺失）→ 保守按未对齐处理。
	AlignUnknown AlignStatus = "unknown"
)

// AlignedMemory 单条记忆的对齐结论。
type AlignedMemory struct {
	MemoryID string      `json:"memory_id"`
	AgentID  string      `json:"agent_id,omitempty"`
	Status   AlignStatus `json:"status"`
	// Reason 判定理由（可读，便于审计「为什么禁止融合」）。
	Reason string `json:"reason,omitempty"`
	// EntityKey 归一化实体键（小写、去标点与空白）；entity 为空时为空串。
	EntityKey string `json:"entity_key,omitempty"`
	// SceneKey / TaskKey 归一化场景 / 任务键。
	SceneKey string `json:"scene_key,omitempty"`
	TaskKey  string `json:"task_key,omitempty"`
	// FrameKey 该条记忆所属（或应对应）的参考系键。
	FrameKey string `json:"frame_key,omitempty"`
}

// AlignGroup 一组可安全融合的记忆（同参考系）。
type AlignGroup struct {
	// FrameKey 组键：由**策略要求的坐标**拼接，实体恒参与。
	FrameKey string `json:"frame_key"`
	// Members 组内成员（按 MemoryID 字典序）。
	Members []string `json:"members"`
	// Agents 组内涉及的 Agent（去重，按字典序）。
	Agents []string `json:"agents,omitempty"`
	// CrossAgent 组内是否含多个不同 Agent。
	CrossAgent bool `json:"cross_agent"`
}

// AlignReport 跨 Agent 对齐整体结论。
type AlignReport struct {
	// Groups 已对齐组（**组内**才允许融合；组间与 Unaligned 一律禁止融合）。
	Groups []AlignGroup `json:"groups,omitempty"`
	// Unaligned 未对齐 / 无法判定的记忆（**只并列展示，禁止融合与互相覆盖**）。
	Unaligned []AlignedMemory `json:"unaligned,omitempty"`
	// Details 全部输入记忆的逐条判定（按 MemoryID 字典序）。
	Details []AlignedMemory `json:"details,omitempty"`
	// Policy 本次生效的策略。
	Policy EntityAlignPolicy `json:"policy"`
	// ReferenceFrameKey 参考系模式下的目标键；自分组模式为空。
	ReferenceFrameKey string `json:"reference_frame_key,omitempty"`
	// Mode 本次判定模式：reference（对齐到显式参考系）/ group（自分组）。
	Mode string `json:"mode"`
}

// Aligned 该报告是否无未对齐项（即全部候选可安全融合）。
func (r AlignReport) Aligned() bool { return len(r.Unaligned) == 0 }

// AlignCrossAgentFusion 跨 Agent 融合前的**强制参考系对齐前置**（Monty 建议 5）。
//
// 两种模式（由 ref 是否为空决定）：
//
//  1. **参考系模式**（ref != nil，跨 Agent 融合的推荐用法）：
//     把 ref 当作「接收方自己的参考系」。每条记忆按策略要求的坐标与 ref 比对，
//     一致 → AlignAligned 并进同一组；不一致 → AlignUnaligned（附差异维度说明），
//     **只并列展示，不进任何组、禁止融合与互相覆盖**。
//  2. **自分组模式**（ref == nil）：按每条记忆**自身**的策略坐标构造分组键，
//     同键者同组。组间天然隔离，因此不同实体/场景绝不会被合并——但此时无法
//     判定「谁对谁未对齐」，故 Unaligned 只包含坐标缺失（AlignUnknown）者。
//
// 无论哪种模式，**只有同组记忆才允许进入融合**；Unaligned 中的记忆一律拒绝融合。
//
// 判定规则（确定性、纯字符串、不调 LLM）：
//   - entity 缺失 → AlignUnknown（保守拒绝融合）；
//   - 策略要求的 scene / task / category 缺失 → AlignUnknown；
//   - 参考系模式下与 ref 的任一要求维度归一化后不等 → AlignUnaligned；
//   - 其余 → AlignAligned。
//
// **默认路径不调用**：既有 Rank / Fuse / VoteFusion 行为逐位不变。
func AlignCrossAgentFusion(ms []*Memory, ref *ReferenceFrame, policy EntityAlignPolicy) AlignReport {
	rep := AlignReport{Policy: policy, Mode: "group"}
	if len(ms) == 0 {
		return rep
	}

	type rec struct {
		m        *Memory
		entity   string
		scene    string
		task     string
		category string
		agent    string
	}
	recs := make([]rec, 0, len(ms))
	for _, m := range ms {
		if m == nil || m.MemoryID == "" {
			continue
		}
		recs = append(recs, rec{
			m:        m,
			entity:   normalizeAlignKey(m.ReferenceFrame.Entity),
			scene:    normalizeAlignKey(m.ReferenceFrame.Scene),
			task:     normalizeAlignKey(m.ReferenceFrame.Task),
			category: normalizeAlignKey(m.ReferenceFrame.Category),
			agent:    strings.TrimSpace(m.AgentID),
		})
	}
	if len(recs) == 0 {
		return rep
	}
	// 确定性顺序：按 MemoryID 字典序推进（输入顺序不影响结论）。
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].m.MemoryID < recs[j].m.MemoryID })

	// 参考系模式：先算出目标键，作为比对的唯一基准。
	var refEntity, refScene, refTask, refCategory string
	if ref != nil {
		rep.Mode = "reference"
		refEntity = normalizeAlignKey(ref.Entity)
		refScene = normalizeAlignKey(ref.Scene)
		refTask = normalizeAlignKey(ref.Task)
		refCategory = normalizeAlignKey(ref.Category)
		rep.ReferenceFrameKey = buildFrameKey(refEntity, refScene, refTask, refCategory, policy)
	}

	groupIdx := map[string]int{}
	for _, r := range recs {
		miss := missingAlignCoord(r.entity, r.scene, r.task, r.category, policy)
		det := AlignedMemory{
			MemoryID: r.m.MemoryID, AgentID: r.agent,
			EntityKey: r.entity, SceneKey: r.scene, TaskKey: r.task,
		}
		if miss != "" {
			det.Status = AlignUnknown
			det.Reason = miss + "：无法判定指代对象，保守按未对齐处理"
			rep.Unaligned = append(rep.Unaligned, det)
			rep.Details = append(rep.Details, det)
			continue
		}
		if ref != nil {
			det.FrameKey = buildFrameKey(r.entity, r.scene, r.task, r.category, policy)
			// 参考系模式：与 ref 逐维比对（仅比对策略要求的维度）。
			if diffs := frameDiffs(r.entity, r.scene, r.task, r.category,
				refEntity, refScene, refTask, refCategory, policy); len(diffs) > 0 {
				det.Status = AlignUnaligned
				det.Reason = "与参考系不一致（" + strings.Join(diffs, "、") +
					"）：只并列展示，禁止融合与互相覆盖"
				rep.Unaligned = append(rep.Unaligned, det)
				rep.Details = append(rep.Details, det)
				continue
			}
		}
		det.Status = AlignAligned
		det.Reason = "参考系一致，允许融合"
		key := rep.ReferenceFrameKey
		if ref == nil {
			key = buildFrameKey(r.entity, r.scene, r.task, r.category, policy)
			det.FrameKey = key
		}
		gi, ok := groupIdx[key]
		if !ok {
			rep.Groups = append(rep.Groups, AlignGroup{FrameKey: key})
			gi = len(rep.Groups) - 1
			groupIdx[key] = gi
		}
		g := &rep.Groups[gi]
		g.Members = append(g.Members, r.m.MemoryID)
		if r.agent != "" && !contains(g.Agents, r.agent) {
			g.Agents = append(g.Agents, r.agent)
		}
		rep.Details = append(rep.Details, det)
	}

	// 收尾：组成员/Agent 排序 + 跨 Agent 标注（确定性）。
	for i := range rep.Groups {
		g := &rep.Groups[i]
		sort.Strings(g.Members)
		sort.Strings(g.Agents)
		g.CrossAgent = len(g.Agents) > 1
	}
	// 组间按 FrameKey 排序，保证报告可复现。
	sort.SliceStable(rep.Groups, func(i, j int) bool { return rep.Groups[i].FrameKey < rep.Groups[j].FrameKey })
	sort.SliceStable(rep.Unaligned, func(i, j int) bool { return rep.Unaligned[i].MemoryID < rep.Unaligned[j].MemoryID })
	sort.SliceStable(rep.Details, func(i, j int) bool { return rep.Details[i].MemoryID < rep.Details[j].MemoryID })
	return rep
}

// missingAlignCoord 返回第一个缺失（归一化后为空）的**策略要求**坐标说明；全齐返回 ""。
func missingAlignCoord(entity, scene, task, category string, p EntityAlignPolicy) string {
	if entity == "" {
		return "reference_frame.entity 缺失"
	}
	if p.RequireScene && scene == "" {
		return "reference_frame.scene 缺失（策略要求 scene 对齐）"
	}
	if p.RequireTask && task == "" {
		return "reference_frame.task 缺失（策略要求 task 对齐）"
	}
	if p.RequireCategory && category == "" {
		return "reference_frame.category 缺失（策略要求 category 对齐）"
	}
	return ""
}

// buildFrameKey 由**策略要求的坐标**拼接分组键（实体恒参与）。
// 关键：不需要对齐的维度不进键，否则「要求放宽」却仍按旧维度分组，
// 会出现「策略说可对齐、实际仍被拆开」的自相矛盾。
func buildFrameKey(entity, scene, task, category string, p EntityAlignPolicy) string {
	parts := []string{entity}
	if p.RequireScene {
		parts = append(parts, scene)
	}
	if p.RequireTask {
		parts = append(parts, task)
	}
	if p.RequireCategory {
		parts = append(parts, category)
	}
	return strings.Join(parts, "|")
}

// frameDiffs 返回 a 相对 ref 的策略要求维度差异说明（为空表示一致）。
func frameDiffs(aEntity, aScene, aTask, aCategory,
	refEntity, refScene, refTask, refCategory string, p EntityAlignPolicy) []string {
	var diffs []string
	if aEntity != refEntity {
		diffs = append(diffs, "entity="+aEntity+"≠"+refEntity)
	}
	if p.RequireScene && aScene != refScene {
		diffs = append(diffs, "scene="+aScene+"≠"+refScene)
	}
	if p.RequireTask && aTask != refTask {
		diffs = append(diffs, "task="+aTask+"≠"+refTask)
	}
	if p.RequireCategory && aCategory != refCategory {
		diffs = append(diffs, "category="+aCategory+"≠"+refCategory)
	}
	return diffs
}

// normalizeAlignKey 参考系键归一化：小写、去标点与空白（与 displacement_edge
// 的实体归一化口径一致，但**独立实现**，避免 A/B 两路耦合）。
func normalizeAlignKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
