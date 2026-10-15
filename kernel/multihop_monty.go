// R13-B 路：多跳召回（关系式 / 位移式检索，借鉴 DisplacementLM，建议 1）。
//
// 真源：lightmem.md §3.2.1；改造依据 lightmem/monty.md §11.2 DisplacementGraphLM
// 与 §4.2 建议 1。
//
// 与既有 kernel/multihop.go 的关系：**本文件是全新文件，不修改 multihop.go**。
// multihop.go（R4-A）是「证据式重排」——在已融合候选上按锚点覆盖度重排；
// 本文件是「位移式召回」——从种子候选出发，沿 displacement_edge 做预测式跳转，
// 把原本向量相似度捞不到、但与种子有确定关系（causal/temporal/task-flow/entity-alias）
// 的记忆补进候选集。
//
// DisplacementLM 的核心纪律（monty.md §11.2）：**位移预测、不一致即剪枝**——
// 不累积置信度、不修改原信号分。本函数严格遵守：
//   - accept(memID) 是调用方传入的「硬条件/§3.2 硬过滤」闸门：
//     返回 false 的候选立即剪枝，且**不再从它继续扩展**（不跨硬过滤边界）；
//   - 函数不读、不改种子候选的原始 Score/SignalScores，只输出**新增**的边召回候选；
//   - 保留的候选仅按 weight × 0.5^hop 做**软排序**（hop 越深衰减越重），
//     该分只用于在召回集内排序，不回灌到主融合分。
//
// 确定性：BFS 按 (hop 层, 边三元组全序) 扩展；同一记忆只保留首次（hop 最小）到达。
package kernel

import (
	"sort"
)

// DefaultMultihopMontyHops 位移式召回跳数上限 N（任务书 B.2：N=2）。
const DefaultMultihopMontyHops = 2

// MultihopMontyConfig 位移式召回配置（只增不改，调用方显式启用）。
type MultihopMontyConfig struct {
	// MaxHops 跳数上限；<=0 取 DefaultMultihopMontyHops(2)；>2 收敛到 2。
	MaxHops int
}

// DisplacementCandidate 一条被位移边召回的候选。
type DisplacementCandidate struct {
	MemoryID string       `json:"memory_id"`
	Hop      int          `json:"hop"`                // 0=种子本身通过硬过滤；>=1=经第 hop 跳边到达
	Score    float64      `json:"score"`              // 软排序分 = 路径权重累乘 × 0.5^hop；hop0=1.0
	Path     []string     `json:"path,omitempty"`     // 推理链：seed -> ... -> 本节点
	Relation EdgeRelation `json:"relation,omitempty"` // 最后一跳所用边关系（hop>=1）
}

// DisplacementRecall 位移式预测召回。
//
// 参数：
//   - seeds：种子记忆 ID（hop-1 检索已命中的候选）；
//   - store：位移边表（只读有效边）；
//   - accept：硬条件闸门——候选与查询硬条件不一致时返回 false 即剪枝。
//     种子 accept=false 直接丢弃且不扩展；被扩展节点 accept=false 同样剪枝。
//     这是「不跨越 §3.2 硬过滤边界」的执行点；
//   - cfg：跳数上限。
//
// 返回：通过硬过滤的候选（含种子 hop0 与边扩展 hop1/2），按 Score 降序、
// 同 Score 按 MemoryID 字典序全序排序。不修改任何输入与原信号分。
func DisplacementRecall(seeds []string, store *EdgeStore, accept func(id string) bool, cfg MultihopMontyConfig) []DisplacementCandidate {
	maxHops := cfg.MaxHops
	if maxHops <= 0 {
		maxHops = DefaultMultihopMontyHops
	}
	if maxHops > DefaultMultihopMontyHops {
		maxHops = DefaultMultihopMontyHops // 本期硬上限 N=2，超限收敛而非报错（调用方可在文档感知）
	}
	if store == nil || accept == nil {
		return nil
	}

	visited := make(map[string]int) // memoryID -> 已到达的 hop（最小 hop）
	type frontier struct {
		id    string
		hop   int
		score float64
		path  []string
		rel   EdgeRelation
	}
	var queue []frontier

	// hop-0：种子先过硬过滤。
	for _, s := range seeds {
		if s == "" {
			continue
		}
		if _, seen := visited[s]; seen {
			continue // 已处理过的种子去重
		}
		if !accept(s) {
			visited[s] = -1 // 硬条件不一致：剪枝，且不作为扩展源
			continue
		}
		visited[s] = 0
		queue = append(queue, frontier{id: s, hop: 0, score: 1.0, path: []string{s}})
	}

	out := make([]DisplacementCandidate, 0, len(queue))
	for len(queue) > 0 {
		// 按 hop 层序处理：逐层 BFS。本实现按队列序扩展（queue 先入先出，天然层序）。
		cur := queue[0]
		queue = queue[1:]
		out = append(out, DisplacementCandidate{
			MemoryID: cur.id,
			Hop:      cur.hop,
			Score:    cur.score,
			Path:     cur.path,
			Relation: cur.rel,
		})
		if cur.hop >= maxHops {
			continue // 跳数已达上限，不再扩展
		}
		for _, e := range store.OutEdges(cur.id) {
			next := e.ToMemoryID
			if next == "" {
				continue
			}
			if prev, seen := visited[next]; seen && prev >= 0 {
				continue // 已以更小/等 hop 到达，不重复
			}
			if !accept(next) {
				visited[next] = -1 // 硬条件不一致：剪枝，且不再从它扩展
				continue
			}
			visited[next] = cur.hop + 1
			nextPath := make([]string, len(cur.path), len(cur.path)+1)
			copy(nextPath, cur.path)
			nextPath = append(nextPath, next)
			queue = append(queue, frontier{
				id:    next,
				hop:   cur.hop + 1,
				score: cur.score * e.Weight * 0.5,
				path:  nextPath,
				rel:   e.Relation,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out
}
