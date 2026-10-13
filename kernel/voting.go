// kernel/voting.go — R6-A 千脑投票共识 + 分歧置信度 + 参考系多帧（新增）。
//
// 理论依据（《千脑智能》研读结论 §0.2）：
//   - 皮质柱各自达成完整解释，柱间「投票」涌现共识；
//   - 分歧 = 不确定（预测误差驱动注意）；
//   - 参考系是知识的组织核心，一物多帧（帧间变换匹配）。
//
// 与既有实现的边界（红线 1「只增不改」）：
//   - rank_fusion.go 的 VoteFusion 是「对多版本候选做信号分加权平均」，
//     本文件另写一条**解释级秩投票**通路，不改变 VoteFusion 语义；
//   - 不修改 Memory / RouterContext / RecalledCandidate 既有字段语义；
//   - 多帧容器 FrameSet 是 side-channel 结构（只增不改），不落 Memory Schema。
//
// 确定性（红线 5）：
//   - 倒数秩计票无随机源；
//   - 平局一律按 Source 名字典序打破（不引入 time.Now / map 迭代序）。
package kernel

import (
	"sort"
	"strings"
)

// Interpretation 单路信号 / 单检索层产出的 top-K 解释（候选 ID 序列）。
//
// 千脑映射：一根皮质柱对同一事物给出一份「完整解释」——即它认为最匹配的
// top-K 候选排序。N 路信号 = N 根柱，各自独立解释。
type Interpretation struct {
	// Source 解释来源（信号名或检索层名），固定平局打破次序用。
	Source string
	// TopIDs 该解释的 top-K 候选 ID 序列（降序；第 0 位 = 该柱第一选择）。
	TopIDs []string
	// Score 候选 ID -> 该柱给的原始分（仅用于 trace / 分析，不参与计票）。
	Score map[string]float64
	// Weight 该柱在共识投票中的信度（默认 1.0）。
	// R6-A：柱间信度可以不同（千脑：强语义柱比弱参考系柱更有话语权），
	// 计票时乘在倒数秩上；<=0 视为 1.0。缺省等权，保证既有单测语义不变。
	Weight float64
}

// ConsensusResult 共识投票输出。
//
// 千脑映射：柱间投票的结果 + 票数分布直接给出分歧度。
type ConsensusResult struct {
	// Ranking 共识序（候选 ID，按共识分降序）。
	Ranking []string
	// Scores 候选 ID -> 共识分（倒数秩计票，见 ConsensusVoter.Vote）。
	Scores map[string]float64
	// VoteCounts 候选 ID -> 被多少路解释列为解释成员（得票数）。
	VoteCounts map[string]int
	// Consensus 头部候选得票集中度 ∈ [0,1]：top1 共识分 / 全部共识分之和。
	// 1 = 所有柱的票都集中在 top1（全柱一致）；越小 = 越分散。
	Consensus float64
	// Divergence 1 - Consensus，柱间分歧度；越大 = 越不确定。
	Divergence float64
	// Interpretations 参与投票的解释来源名（按 Source 字典序，确定性）。
	Interpretations []string
}

// ConsensusVoter 解释级共识投票器（秩投票，倒数秩计票）。
//
// 计票规则（确定性）：
//   - 对每路解释，其 TopIDs 中第 rank 位（1-based）的候选得 1/rank 票
//     （倒数秩计票：rank=1 得 1.0，rank=2 得 0.5，rank=3 得 0.333…）；
//   - 候选共识分 = 各路解释给它的票权之和；
//   - 排名平局按 Source 名字典序打破（不引入随机 / 时间源）。
type ConsensusVoter struct {
	// K 每路解释参与计票的 top-K 深度；<=0 时取该解释自身长度。
	K int
}

// Vote 对 N 路解释做秩投票，返回共识序 + 共识度 + 分歧度。
// 输入 interps 为空时返回空 ConsensusResult。
// 输入顺序不影响结果（最终按 Source 排序），保证确定性。
func (v *ConsensusVoter) Vote(interps []Interpretation) ConsensusResult {
	res := ConsensusResult{
		Scores:     map[string]float64{},
		VoteCounts: map[string]int{},
	}
	if len(interps) == 0 {
		return res
	}
	// 1) 固定解释参与顺序（按 Source 字典序），杜绝 map 迭代序依赖。
	sorted := make([]Interpretation, len(interps))
	copy(sorted, interps)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Source < sorted[j].Source
	})
	for _, it := range sorted {
		if it.Source != "" {
			res.Interpretations = append(res.Interpretations, it.Source)
		}
	}
	// 2) 倒数秩计票。
	k := v.K
	if k <= 0 {
		k = len(sorted)
	}
	for _, it := range sorted {
		w := it.Weight
		if w <= 0 {
			w = 1.0
		}
		seen := map[string]bool{}
		for i, id := range it.TopIDs {
			if i >= k {
				break
			}
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			rank := i + 1
			res.Scores[id] += w * (1.0 / float64(rank))
			res.VoteCounts[id]++
		}
	}
	if len(res.Scores) == 0 {
		return res
	}
	// 3) 共识序：共识分降序，平局按 ID 字典序。
	ids := make([]string, 0, len(res.Scores))
	for id := range res.Scores {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if res.Scores[ids[i]] != res.Scores[ids[j]] {
			return res.Scores[ids[i]] > res.Scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	res.Ranking = ids
	// 4) 共识度 / 分歧度：top1 共识分 / 全部共识分之和。
	var total float64
	for _, sc := range res.Scores {
		total += sc
	}
	if total > 0 {
		res.Consensus = res.Scores[res.Ranking[0]] / total
	} else {
		res.Consensus = 0
	}
	if res.Consensus > 1 {
		res.Consensus = 1
	}
	res.Divergence = 1 - res.Consensus
	return res
}

// ------------------------------------------------------------------
// 参考系多帧（一物多帧 + 帧变换匹配）
// ------------------------------------------------------------------

// FrameSet 一个候选 / 一个查询的参考系集合（side-channel 结构）。
//
// 只增不改：本类型是独立容器，不写入 Memory 既有字段，因此不触碰
// Memory.ReferenceFrame（单帧）语义；与单帧共存的读取规则见 MatchFrames。
type FrameSet struct {
	// Frames 该物体附着的多个参考系（千脑「一物多帧」）。
	Frames []ReferenceFrame
	// Weight 该 FrameSet 的融合权重（默认 1.0；用于多帧加权匹配）。
	Weight float64
}

// MatchFrames 帧变换匹配：查询帧与候选 FrameSet 的**任一副帧**匹配即得分，
// 取代单帧稀释（P3 根因：单帧不匹配即得 0，投票权重被稀释）。
//
// 匹配口径（确定性，纯坐标比对）：
//   - 查询端非空坐标集合为 Q；
//   - 对候选每副帧计算命中率 = 命中坐标数 / |Q|；
//   - 候选得分 = max over 副帧（命中最优帧），再乘 FrameSet.Weight（若 0 则 1）。
//   - 查询端无坐标（Q 空）→ 返回 0（不给正偏向，与 R3-A 修复口径一致）。
//
// 坐标比对用 EqualFold，忽略大小写（与 rank_fusion.signalReferenceFrame 一致）。
func MatchFrames(q *ReferenceFrame, cand *FrameSet) float64 {
	if q == nil || cand == nil {
		return 0
	}
	w := cand.Weight
	if w <= 0 {
		w = 1.0
	}
	coords := referenceFrameNonEmptyCoords(q)
	if len(coords) == 0 {
		return 0
	}
	if len(cand.Frames) == 0 {
		return 0
	}
	best := 0.0
	for i := range cand.Frames {
		hit := 0
		for _, c := range coords {
			if frameCoordEqual(&cand.Frames[i], c, referenceFrameCoordValue(q, c)) {
				hit++
			}
		}
		score := float64(hit) / float64(len(coords))
		if score > best {
			best = score
		}
	}
	if best > 1 {
		best = 1
	}
	return best * w
}

// FrameFromSingle 把单帧包成单元素 FrameSet（兼容仅有 Memory.ReferenceFrame 的既有数据）。
func FrameFromSingle(f ReferenceFrame) FrameSet {
	return FrameSet{Frames: []ReferenceFrame{f}, Weight: 1.0}
}

// ------------------------------------------------------------------
// 内部辅助（坐标枚举 / 取值 / 比较），与 signalReferenceFrame 同一口径。
// ------------------------------------------------------------------

// refFrameCoord 单坐标名。
type refFrameCoord string

const (
	refCoordTenant   refFrameCoord = "tenant"
	refCoordUser     refFrameCoord = "user"
	refCoordAgent    refFrameCoord = "agent"
	refCoordProject  refFrameCoord = "project"
	refCoordScene    refFrameCoord = "scene"
	refCoordTask     refFrameCoord = "task"
	refCoordEntity   refFrameCoord = "entity"
	refCoordCategory refFrameCoord = "category"
)

// referenceFrameNonEmptyCoords 返回 f 中非空坐标名（按固定顺序）。
func referenceFrameNonEmptyCoords(f *ReferenceFrame) []refFrameCoord {
	var out []refFrameCoord
	if f == nil {
		return out
	}
	if f.Tenant != "" {
		out = append(out, refCoordTenant)
	}
	if f.User != "" {
		out = append(out, refCoordUser)
	}
	if f.Agent != "" {
		out = append(out, refCoordAgent)
	}
	if f.Project != "" {
		out = append(out, refCoordProject)
	}
	if f.Scene != "" {
		out = append(out, refCoordScene)
	}
	if f.Task != "" {
		out = append(out, refCoordTask)
	}
	if f.Entity != "" {
		out = append(out, refCoordEntity)
	}
	if f.Category != "" {
		out = append(out, refCoordCategory)
	}
	return out
}

// referenceFrameCoordValue 取 f 指定坐标的值。
func referenceFrameCoordValue(f *ReferenceFrame, c refFrameCoord) string {
	if f == nil {
		return ""
	}
	switch c {
	case refCoordTenant:
		return f.Tenant
	case refCoordUser:
		return f.User
	case refCoordAgent:
		return f.Agent
	case refCoordProject:
		return f.Project
	case refCoordScene:
		return f.Scene
	case refCoordTask:
		return f.Task
	case refCoordEntity:
		return f.Entity
	case refCoordCategory:
		return f.Category
	}
	return ""
}

// frameCoordEqual 判断 f 的坐标 c 是否等于 want（EqualFold，忽略大小写）。
func frameCoordEqual(f *ReferenceFrame, c refFrameCoord, want string) bool {
	return strings.EqualFold(referenceFrameCoordValue(f, c), want)
}
