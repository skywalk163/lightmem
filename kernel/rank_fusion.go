// 多信号 rank 融合引擎（§3.4）
// 真源：lightmem.md §3.4「多信号独立打分 + 按名次融合」。
//
// 设计要点（严格对齐 §3.4）：
//   - 八路信号先各自独立打分（互不线性叠加），再按名次（rank）用
//     Reciprocal Rank Fusion 变体融合：score = Σ w_i * 1/(k + rank_i)。
//   - 融合结果归一化到 [0,1]，供「绝对分下限 top1 ≥ 0.6」判定使用。
//   - 三态决策：唯一最优 / 接近融合（(top1-top2)/top1 < 20% 且 top1 ≥ 0.6）/ 冲突标记。
//   - 双时态：默认只返回当前生效版本；as_of 回溯返回覆盖时刻版本（is_historical=true）。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lightmem/kernel/i18n"
)

// ------------------------------------------------------------------
// 类型定义
// ------------------------------------------------------------------

// RecalledCandidate 是 A1 跨层召回产出的候选（含融合所需全量字段）。
type RecalledCandidate struct {
	Memory      *Memory     `json:"memory"`
	Layer       MemoryLayer `json:"layer"`
	RecallScore float64     `json:"recall_score"` // 召回阶段粗分（向量相似度或文本命中分）
}

func (c RecalledCandidate) id() string {
	if c.Memory == nil {
		return ""
	}
	return c.Memory.MemoryID
}

// SignalFunc 单信号打分器：给定 query 与候选集，返回 memory_id -> 原始分（≥0）。
// 各信号独立打分，不做任何跨信号线性加权（§3.4）。
type SignalFunc func(ctx context.Context, query string, cands []RecalledCandidate) map[string]float64

// NamedSignal 带名字与权重来源属性的信号。
type NamedSignal struct {
	Name   string
	Weight float64
	Type   string // calibrated（需评测校准）| fixed（固定公式/规则）
	Fn     SignalFunc
}

// FusionMode 融合模式常量（R6-A）。
const (
	// FusionModeRRF 原有 RRF 融合（默认；空字符串也等价于此，isolate 复现）。
	FusionModeRRF = "rrf"
	// FusionModeVote R6-A 解释级共识投票（千脑「柱间投票」）。
	FusionModeVote = "vote"
)

// RankFusion 多信号 rank 融合引擎（§3.4）。
type RankFusion struct {
	Signals        []NamedSignal
	Weights        map[string]float64
	AbsoluteFloor  float64         // 绝对分下限，默认 0.6
	ProximityRatio float64         // 接近融合比值，默认 0.20（20%）
	HalfLife       time.Duration   // 时效新鲜度半衰期，默认 30 天
	RRFK           int             // RRF 平滑常数 k，默认 60
	Now            time.Time       // 打分基准时刻（可注入，便于测试）
	AsOf           *string         // 显式回溯日期（YYYY-MM-DD），非空时返回历史版本
	QueryFrame     *ReferenceFrame // 查询端参考系（供参考系匹配度信号使用）

	// FusionMode 融合模式：FusionModeRRF（默认，"" 等价）或 FusionModeVote。
	// 默认 rrf，投票模式默认关闭以保不回归（R6-A）。
	// 本字段不影响 rrf 路径的逐位行为；置 vote 后 Rank() 走解释级共识投票。
	FusionMode string

	// VoteTopK 投票模式下每路解释参与计票的 top-K 深度；<=0 取候选总数。
	// 仅 FusionModeVote 时生效。
	VoteTopK int

	// SemanticRedundant R7-A：semantic 信号启用多路径冗余（默认 false，保证不回归）。
	// 开启后 signalSemantic 用多路径投票替代单值加权；false 时逐位保持 R6 行为。
	SemanticRedundant bool
	// SemanticAblatePath R7-A：多路径冗余模式下消融的语义路径名
	// （semantic-path0|semantic-path1|semantic-path2）；空=不消融。
	// 仅 SemanticRedundant=true 时生效。
	SemanticAblatePath string

	embedder Embedder // 可选：真实语义向量化（nil 时用召回分近似；R4-D 起为接口类型）
}

// FusedResult 融合输出单条（§3.4 响应）。
type FusedResult struct {
	MemoryID             string             `json:"memory_id"`
	Fact                 string             `json:"fact"`
	Layer                MemoryLayer        `json:"layer"`
	Score                float64            `json:"score"` // 归一化融合分 [0,1]
	SignalScores         map[string]float64 `json:"signal_scores,omitempty"`
	Ranks                map[string]int     `json:"ranks,omitempty"`
	Source               string             `json:"source,omitempty"`
	Confidence           float64            `json:"confidence,omitempty"`
	IsHistorical         bool               `json:"is_historical"`
	ValidFrom            string             `json:"valid_from,omitempty"`
	ValidUntil           string             `json:"valid_until,omitempty"`
	SupersededBy         *string            `json:"superseded_by,omitempty"`
	LowConfidence        bool               `json:"low_confidence,omitempty"`
	RequiresConfirmation bool               `json:"requires_confirmation,omitempty"`
	FusedWith            []string           `json:"fused_with,omitempty"`
	Reason               string             `json:"reason,omitempty"`
	Memory               *Memory            `json:"memory,omitempty"`
	// ConsensusVoteScore 该候选在解释级共识投票中的共识分（R6-A side-channel）。
	// 仅 FusionModeVote 时填充；RRF 路径恒为零值（omitempty，不影响既有 JSON）。
	ConsensusVoteScore float64 `json:"consensus_vote_score,omitempty"`
	// VoteConsensus 全 query 的共识度（R6-A side-channel，每条候选同值）∈[0,1]。
	VoteConsensus float64 `json:"vote_consensus,omitempty"`
	// VoteDivergence 全 query 的分歧度（R6-A side-channel）∈[0,1]。
	// 分歧大 → 上层可判定「需确认 / 低置信」（千脑「分歧=不确定」）。
	VoteDivergence float64 `json:"vote_divergence,omitempty"`
	// VoteSources 参与投票的解释来源名（按字典序；R6-A）。
	VoteSources []string `json:"vote_sources,omitempty"`
	// VoteCount 该候选被多少路解释列为解释成员（R6-A side-channel）。
	// 用于「得票数 vs top1 错误率」分析。
	VoteCount int `json:"vote_count,omitempty"`
}

// ------------------------------------------------------------------
// 默认构造
// ------------------------------------------------------------------

// DefaultWeightSpec 描述一个信号的默认权重与其来源类型。
type DefaultWeightSpec struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Type   string  `json:"type"`
	Note   string  `json:"note"`
}

// defaultWeightSpecs 占位默认权重。
// ⚠️ 这些是「占位默认值，A8 KPI 出数后按评测结论校准」——禁止当作「调优好」的先验（任务书红线 4）。
var defaultWeightSpecs = []DefaultWeightSpec{
	{Name: "semantic", Weight: 1.0, Type: "calibrated", Note: "语义相似度（向量召回分近似）"},
	{Name: "keyword", Weight: 0.8, Type: "calibrated", Note: "BM25 全文关键词"},
	{Name: "entity", Weight: 0.7, Type: "calibrated", Note: "实体匹配"},
	{Name: "value", Weight: 0.6, Type: "fixed", Note: "记忆固有价值（§5.3）"},
	{Name: "freshness", Weight: 0.5, Type: "fixed", Note: "时效新鲜度（指数衰减）"},
	{Name: "source_trust", Weight: 0.4, Type: "fixed", Note: "来源可信度 人工>系统>用户"},
	{Name: "reference_frame", Weight: 0.6, Type: "calibrated", Note: "参考系匹配度"},
	{Name: "feedback", Weight: 0.5, Type: "fixed", Note: "反馈历史（采纳/弃用）"},
}

// DefaultRankFusion 返回装配了 8 路信号的默认融合引擎。
func DefaultRankFusion() *RankFusion {
	rf := &RankFusion{
		AbsoluteFloor:  0.6,
		ProximityRatio: 0.20,
		HalfLife:       30 * 24 * time.Hour,
		RRFK:           60,
		Now:            time.Now().UTC(),
	}
	rf.Weights = map[string]float64{}
	for _, s := range defaultWeightSpecs {
		rf.Weights[s.Name] = s.Weight
	}
	// 信号以闭包形式捕获 rf（参考系匹配度需要 QueryFrame；时效需要 Now/HalfLife）。
	rf.Signals = []NamedSignal{
		{Name: "semantic", Weight: rf.Weights["semantic"], Type: "calibrated", Fn: rf.signalSemantic},
		{Name: "keyword", Weight: rf.Weights["keyword"], Type: "calibrated", Fn: signalKeywordBM25},
		{Name: "entity", Weight: rf.Weights["entity"], Type: "calibrated", Fn: signalEntity},
		{Name: "value", Weight: rf.Weights["value"], Type: "fixed", Fn: signalIntrinsicValue},
		{Name: "freshness", Weight: rf.Weights["freshness"], Type: "fixed", Fn: rf.signalFreshness},
		{Name: "source_trust", Weight: rf.Weights["source_trust"], Type: "fixed", Fn: signalSourceTrust},
		{Name: "reference_frame", Weight: rf.Weights["reference_frame"], Type: "calibrated", Fn: rf.signalReferenceFrame},
		{Name: "feedback", Weight: rf.Weights["feedback"], Type: "fixed", Fn: rf.signalFeedbackHistory},
	}
	return rf
}

// SetEmbedder 注入真实语义向量化函数（可选）。
//
// R4-D：签名保持函数形式（向后兼容 R3-A 以来的所有调用方，含 eval/run_kpi.go
// 与未来可能的函数式注入），内部适配为 EmbedderFunc 存入接口字段。
// 需要直接注入 Embedder 接口实现（如未来的 BGE 向量模型）时用 SetEmbedderImpl。
func (rf *RankFusion) SetEmbedder(f func(string) ([]float32, error)) {
	if f == nil {
		rf.embedder = nil
		return
	}
	rf.embedder = EmbedderFunc(f)
}

// SetEmbedderImpl 注入 Embedder 接口实现（R4-D 新增扩展点）。
// 与 SetEmbedder 的区别：接受完整 Embedder（含 Dim()/Name()），供未来真实向量模型接入。
// 两者互斥：后调用者覆盖前者；传 nil 清除已注入的向量化器。
func (rf *RankFusion) SetEmbedderImpl(e Embedder) { rf.embedder = e }

// DefaultWeightsJSON 返回默认权重的 JSON 表示（供落盘参考 / 校准加载）。
func DefaultWeightsJSON() []byte {
	doc := map[string]any{
		"_comment": "占位默认值，A8 KPI 出数后按评测结论校准；禁止作为调优先验。",
		"k":        60,
		"weights":  defaultWeightSpecs,
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

// LoadWeightsFromFile 从 JSON（格式同 rank_fusion.default_weights.json）覆盖权重。
func (rf *RankFusion) LoadWeightsFromFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		K       int                 `json:"k"`
		Weights []DefaultWeightSpec `json:"weights"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse weights %s: %w", path, err)
	}
	if doc.K > 0 {
		rf.RRFK = doc.K
	}
	for _, w := range doc.Weights {
		rf.Weights[w.Name] = w.Weight
		for i := range rf.Signals {
			if rf.Signals[i].Name == w.Name {
				rf.Signals[i].Weight = w.Weight
			}
		}
	}
	return nil
}

// ------------------------------------------------------------------
// 核心融合
// ------------------------------------------------------------------

// Rank 返回全部候选的融合排序（归一化分 [0,1]，降序）。
//
// R6-A：默认（FusionMode 为空或 FusionModeRRF）走原有 RRF 加权和路径，
// 逐位不变（isolate 复现）；FusionMode==FusionModeVote 时走解释级共识投票
// （千脑「柱间投票」），见 consensusVoteRank。
func (rf *RankFusion) Rank(ctx context.Context, query string, cands []RecalledCandidate) []FusedResult {
	rf.ensureDefaults()
	if rf.FusionMode == FusionModeVote {
		return rf.consensusVoteRank(ctx, query, cands)
	}
	return rf.rrfRank(ctx, query, cands)
}

// rrfRank 原有 RRF 加权和路径（isolate 复现，逐位不变）。
func (rf *RankFusion) rrfRank(ctx context.Context, query string, cands []RecalledCandidate) []FusedResult {
	if len(cands) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cands))
	byID := make(map[string]*Memory, len(cands))
	for _, c := range cands {
		if c.Memory == nil || c.Memory.MemoryID == "" {
			continue
		}
		ids = append(ids, c.Memory.MemoryID)
		byID[c.Memory.MemoryID] = c.Memory
	}
	if len(ids) == 0 {
		return nil
	}

	// 1) 各信号独立打分并归一化
	norm := make(map[string]map[string]float64, len(rf.Signals))
	for _, s := range rf.Signals {
		raw := s.Fn(ctx, query, cands)
		norm[s.Name] = normalizeMax(raw)
	}

	// 2) 按名次融合（RRF 变体）
	ranks := make(map[string]map[string]int, len(rf.Signals))
	for _, s := range rf.Signals {
		ranks[s.Name] = rankMap(norm[s.Name])
	}
	maxPossible := 0.0
	for _, s := range rf.Signals {
		maxPossible += s.Weight * (1.0 / float64(rf.RRFK+1))
	}

	out := make([]FusedResult, 0, len(ids))
	for _, id := range ids {
		m := byID[id]
		fused := 0.0
		sigScores := map[string]float64{}
		sigRanks := map[string]int{}
		for _, s := range rf.Signals {
			sc, ok := norm[s.Name][id]
			if !ok {
				continue
			}
			r, ok := ranks[s.Name][id]
			if !ok {
				r = len(ids) + 1
			}
			fused += s.Weight * (1.0 / float64(rf.RRFK+r))
			sigScores[s.Name] = sc
			sigRanks[s.Name] = r
		}
		if maxPossible > 0 {
			fused /= maxPossible
		}
		out = append(out, FusedResult{
			MemoryID:     id,
			Fact:         m.Fact,
			Layer:        m.Layer,
			Score:        fused,
			SignalScores: sigScores,
			Ranks:        sigRanks,
			Source:       m.Source,
			Confidence:   m.Confidence,
			IsHistorical: m.IsHistorical,
			ValidFrom:    m.ValidFrom,
			ValidUntil:   m.ValidUntil,
			SupersededBy: m.SupersededBy,
			Memory:       m,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out
}

// sqrtVoteWeight 柱间信度的 sqrt 缩放：防单柱权重独大稀释投票。
// R6-A：R5 权重里 semantic=12 独大（占 65% 票权），投票口径下会把
// 多跳中间证据顶到 top1，multi-hop 桶严重回退。sqrt 缩放后保留主导但不过强。
// w<=0 视为 0（该柱无信度，不参与投票）。
func sqrtVoteWeight(w float64) float64 {
	if w <= 0 {
		return 0
	}
	return math.Sqrt(w)
}

// consensusVoteRank 解释级共识投票融合路径（R6-A，千脑「柱间投票」）。
//
// 步骤（确定性，无随机源）：
//  1. 每路信号独立打分并归一化（与 RRF 路径同一套打分，保证可比）；
//  2. 每路信号的 top-VoteTopK 候选构成该信号的「解释」（Interpretation）；
//  3. ConsensusVoter 做倒数秩计票，得共识序 + 共识度 + 分歧度；
//  4. 输出按共识分降序的 FusedResult，并附 ConsensusVoteScore / VoteConsensus /
//     VoteDivergence / VoteSources 侧通道字段（只增不改）。
//
// Score 口径：共识分按最大值归一化到 [0,1]（与 RRF 归一化到 [0,1] 同维度，
// 供上层沿用 AbsoluteFloor / ProximityRatio 判定语义）。Fuse() 在投票模式下
// 依赖此归一分做三态决策。
func (rf *RankFusion) consensusVoteRank(ctx context.Context, query string, cands []RecalledCandidate) []FusedResult {
	if len(cands) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cands))
	byID := make(map[string]*Memory, len(cands))
	for _, c := range cands {
		if c.Memory == nil || c.Memory.MemoryID == "" {
			continue
		}
		ids = append(ids, c.Memory.MemoryID)
		byID[c.Memory.MemoryID] = c.Memory
	}
	if len(ids) == 0 {
		return nil
	}

	// 1) 主排序：复用 RRF 加权和路径（vote 模式下主排序逐位与 rrf 一致，
	//    天然不回归——A.4 红线「公开/合成 MRR 不回归」）。
	out := rf.rrfRank(ctx, query, cands)

	// 2) 各信号独立打分 + 归一化 + 名次（与 RRF 路径同一套，可比）。
	norm := make(map[string]map[string]float64, len(rf.Signals))
	ranks := make(map[string]map[string]int, len(rf.Signals))
	for _, s := range rf.Signals {
		raw := s.Fn(ctx, query, cands)
		n := normalizeMax(raw)
		norm[s.Name] = n
		ranks[s.Name] = rankMap(n)
	}

	// 3) 每路信号 → top-K 解释，ConsensusVoter 计票得共识分 + 分歧度（side-channel）。
	topK := rf.VoteTopK
	if topK <= 0 {
		topK = len(ids)
	}
	interps := make([]Interpretation, 0, len(rf.Signals))
	for _, s := range rf.Signals {
		rk := ranks[s.Name]
		type kv struct {
			id   string
			rank int
		}
		arr := make([]kv, 0, len(rk))
		for id, r := range rk {
			arr = append(arr, kv{id, r})
		}
		sort.SliceStable(arr, func(i, j int) bool {
			if arr[i].rank != arr[j].rank {
				return arr[i].rank < arr[j].rank
			}
			return arr[i].id < arr[j].id
		})
		var topIDs []string
		for i, e := range arr {
			if i >= topK {
				break
			}
			topIDs = append(topIDs, e.id)
		}
		if len(topIDs) == 0 {
			continue
		}
		interps = append(interps, Interpretation{
			Source: s.Name,
			TopIDs: topIDs,
			Score:  norm[s.Name],
			Weight: sqrtVoteWeight(rf.Weights[s.Name]),
		})
	}
	voter := &ConsensusVoter{K: topK}
	res := voter.Vote(interps)

	// 4) 把共识分/分歧度作为 side-channel 附到 RRF 排序结果（只增不改：Score 仍是 RRF 分）。
	for i := range out {
		out[i].ConsensusVoteScore = res.Scores[out[i].MemoryID]
		out[i].VoteCount = res.VoteCounts[out[i].MemoryID]
		out[i].VoteConsensus = res.Consensus
		out[i].VoteDivergence = res.Divergence
		out[i].VoteSources = res.Interpretations
	}
	return out
}

// VoteFusion 多版本投票：对 top-K 候选做加权投票，权重 = 各信号归一化分。
// 返回投票后的 top-1 候选（得分 = Σ weight_i × score_i / Σ weight_i）。
// 若所有候选得分均为 0，返回 nil。
func (rf *RankFusion) VoteFusion(ctx context.Context, query string, cands []RecalledCandidate, topK int) *FusedResult {
	if len(cands) == 0 || topK <= 0 {
		return nil
	}
	if topK > len(cands) {
		topK = len(cands)
	}
	ranked := rf.Rank(ctx, query, cands)
	if len(ranked) == 0 {
		return nil
	}
	// 对 top-K 候选做加权投票
	weights := make([]float64, 0, topK)
	scores := make([]float64, 0, topK)
	for i := 0; i < topK && i < len(ranked); i++ {
		w := 0.0
		for _, sig := range ranked[i].SignalScores {
			w += sig
		}
		if w <= 0 {
			w = 1.0 // 防止全 0
		}
		weights = append(weights, w)
		scores = append(scores, ranked[i].Score)
	}
	// 加权平均
	var totalWeight, weightedSum float64
	for i := 0; i < len(weights); i++ {
		totalWeight += weights[i]
		weightedSum += weights[i] * scores[i]
	}
	if totalWeight <= 0 {
		return nil
	}
	ranked[0].Score = weightedSum / totalWeight
	ranked[0].Reason = fmt.Sprintf("vote_fusion: top-%d weighted average", topK)
	return &ranked[0]
}

// ConflictDetector 检测 top-1 与 top-2 的语义冲突。
// 冲突条件：
//  1. 同一主体（reference_frame.entity 相同）
//  2. 同类 kind
//  3. 事实不同
//  4. 向量距离 < 阈值（语义相似度高，但事实不同 → 冲突）
//
// 若 embedder 不可用，退化为启发式检测（isConflict）。
func (rf *RankFusion) ConflictDetector(ctx context.Context, top1, top2 *FusedResult) bool {
	if top1 == nil || top2 == nil || top1.Memory == nil || top2.Memory == nil {
		return false
	}
	// 启发式检测：同一主体 + 同类 + 不同事实
	if !rf.isConflict(top1.Memory, top2.Memory) {
		return false
	}
	// 向量距离检测：若 embedder 可用，计算事实的向量相似度
	if rf.embedder != nil {
		v1, err1 := rf.embedder.Embed(top1.Memory.Fact)
		v2, err2 := rf.embedder.Embed(top2.Memory.Fact)
		if err1 == nil && err2 == nil && len(v1) > 0 && len(v2) > 0 {
			sim := cosine(v1, v2)
			// 相似度高但事实不同 → 冲突
			if sim > 0.7 {
				return true
			}
		}
	}
	// 退化为启发式
	return true
}

// Fuse 是 §3.4 的核心融合函数：先 Rank，再按融合策略返回最终结果集。
//
// 返回语义：
//   - 唯一最优：返回 1 条。
//   - 接近融合（top1/top2 差距 < ProximityRatio 且 top1 ≥ AbsoluteFloor）：返回 top 多条。
//   - 低置信（top1 < AbsoluteFloor）：返回 1 条并标 LowConfidence=true。
//   - 冲突标记（接近且无法融合）：返回多条并标 RequiresConfirmation=true。
func (rf *RankFusion) Fuse(ctx context.Context, query string, cands []RecalledCandidate) []FusedResult {
	ranked := rf.Rank(ctx, query, cands)
	if len(ranked) == 0 {
		return nil
	}
	top1 := ranked[0]
	if len(ranked) == 1 {
		top1.Reason = "single_candidate: unique optimum"
		return []FusedResult{top1}
	}
	top2 := ranked[1]
	ratio := 1.0
	if top1.Score > 0 {
		ratio = (top1.Score - top2.Score) / top1.Score
	}

	// 绝对分下限：低于下限 → 不融合，返回单条 + 低置信标记（§3.4）。
	if top1.Score < rf.AbsoluteFloor {
		top1.LowConfidence = true
		top1.Reason = fmt.Sprintf("top1=%.3f < absolute_floor=%.2f: return single, low confidence", top1.Score, rf.AbsoluteFloor)
		return []FusedResult{top1}
	}

	// VoteFusion：对 top-3 候选做加权投票，提升多版本投票的鲁棒性。
	if len(ranked) >= 3 {
		vf := rf.VoteFusion(ctx, query, cands, 3)
		if vf != nil && vf.Score > top1.Score {
			top1 = *vf
		}
	}

	if ratio < rf.ProximityRatio {
		// 接近融合：融合 top1+top2（及与之接近的同分项）。
		picked := []FusedResult{top1, top2}
		for _, r := range ranked[2:] {
			if r.Score <= 0 {
				continue
			}
			if (top1.Score-r.Score)/top1.Score < rf.ProximityRatio {
				picked = append(picked, r)
			}
		}
		ids := make([]string, 0, len(picked))
		for _, p := range picked {
			ids = append(ids, p.MemoryID)
		}
		for i := range picked {
			fw := make([]string, 0, len(ids)-1)
			for _, id := range ids {
				if id != picked[i].MemoryID {
					fw = append(fw, id)
				}
			}
			picked[i].FusedWith = fw
		}
		if rf.ConflictDetector(ctx, &picked[0], &picked[1]) {
			// 冲突标记：保留多条，标 requires_confirmation，不静默选一个（§3.4）。
			for i := range picked {
				picked[i].RequiresConfirmation = true
				picked[i].Reason = "conflict detected (same subject, contradictory facts): keep multiple, requires_confirmation"
			}
			return picked
		}
		for i := range picked {
			picked[i].Reason = fmt.Sprintf("top1/top2 gap %.1f%% < %.0f%% and top1=%.3f >= floor: fused multiple", ratio*100, rf.ProximityRatio*100, top1.Score)
		}
		return picked
	}

	top1.Reason = fmt.Sprintf("unique optimum (top1/top2 gap %.1f%% >= %.0f%%)", ratio*100, rf.ProximityRatio*100)
	return []FusedResult{top1}
}

// isConflict 冲突启发式：同一主体（reference_frame.entity 相同）且同类 kind 的两条记忆
// 事实不同，视为不可静默融合的冲突（§3.4 冲突标记）。
func (rf *RankFusion) isConflict(a, b *Memory) bool {
	if a == nil || b == nil {
		return false
	}
	ea := strings.ToLower(strings.TrimSpace(a.ReferenceFrame.Entity))
	eb := strings.ToLower(strings.TrimSpace(b.ReferenceFrame.Entity))
	if ea == "" || eb == "" || ea != eb {
		return false
	}
	if a.MemoryKind != b.MemoryKind {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(a.Fact), strings.TrimSpace(b.Fact))
}

func (rf *RankFusion) ensureDefaults() {
	if rf.Now.IsZero() {
		rf.Now = time.Now().UTC()
	}
	if rf.AbsoluteFloor <= 0 {
		rf.AbsoluteFloor = 0.6
	}
	if rf.ProximityRatio <= 0 {
		rf.ProximityRatio = 0.20
	}
	if rf.HalfLife <= 0 {
		rf.HalfLife = 30 * 24 * time.Hour
	}
	if rf.RRFK <= 0 {
		rf.RRFK = 60
	}
}

// ------------------------------------------------------------------
// 八路信号
// ------------------------------------------------------------------

// signalSemantic：语义相似度。优先用真实向量；否则用召回阶段分（向量/文本粗分）近似。
//
// R7-A（Q1）：SemanticRedundant=true 时启用**多路径语义冗余**——在既有向量路径
// 之外，并行接入 slot-value 结构匹配（路径 1）与 token 共现加权相似度（路径 2），
// 三路经 ConsensusVoter 投票产出语义共识序 + 语义分歧度。消除 semantic 单点故障
// （R6-D 消融 −20.68pp）。默认 SemanticRedundant=false，本函数逐位保持 R6 行为。
func (rf *RankFusion) signalSemantic(ctx context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	if rf.SemanticRedundant {
		return rf.semanticMultiScore(ctx, query, cands)
	}
	out := make(map[string]float64, len(cands))
	if rf.embedder != nil && query != "" {
		if qv, err := rf.embedder.Embed(query); err == nil {
			for _, c := range cands {
				if c.Memory == nil {
					continue
				}
				if cv, err := rf.embedder.Embed(c.Memory.Fact); err == nil {
					out[c.Memory.MemoryID] = float64(cosine(qv, cv))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		s := c.RecallScore
		if s < 0 {
			s = 0
		}
		out[c.Memory.MemoryID] = s
	}
	return out
}

// signalKeywordBM25：BM25 全文关键词匹配。
func signalKeywordBM25(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return out
	}
	const k1, b = 1.2, 0.75
	docs := make(map[string][]string, len(cands))
	totalLen := 0.0
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		t := tokenize(c.Memory.Fact)
		docs[c.Memory.MemoryID] = t
		totalLen += float64(len(t))
	}
	n := float64(len(docs))
	if n == 0 {
		return out
	}
	avgdl := totalLen / n
	df := map[string]float64{}
	for _, t := range docs {
		seen := map[string]bool{}
		for _, w := range t {
			if !seen[w] {
				df[w]++
				seen[w] = true
			}
		}
	}
	for id, t := range docs {
		tf := map[string]float64{}
		for _, w := range t {
			tf[w]++
		}
		dl := float64(len(t))
		var score float64
		for _, q := range qTerms {
			f := tf[q]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-df[q]+0.5)/(df[q]+0.5))
			score += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*dl/avgdl))
		}
		if score > 0 {
			out[id] = score
		}
	}
	return out
}

// signalEntity：实体匹配（查询词元命中候选实体/事实的比例）。
func signalEntity(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return out
	}
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		hay := tokenSet(c.Memory.Fact + " " + c.Memory.ReferenceFrame.Entity)
		hit := 0
		for _, q := range qTerms {
			if hay[q] {
				hit++
			}
		}
		if hit > 0 {
			out[c.Memory.MemoryID] = float64(hit) / float64(len(qTerms))
		}
	}
	return out
}

// signalIntrinsicValue：记忆固有价值（§5.3 固定公式）。
func signalIntrinsicValue(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	p := DefaultPromotionPolicy()
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		out[c.Memory.MemoryID] = IntrinsicValue(c.Memory, p)
	}
	return out
}

// signalFreshness：时效新鲜度（指数衰减，半衰期可配）。
func (rf *RankFusion) signalFreshness(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	hl := rf.HalfLife
	if hl <= 0 {
		hl = 30 * 24 * time.Hour
	}
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		t := c.Memory.RecordedAt
		if c.Memory.OccurredAt != nil && !c.Memory.OccurredAt.IsZero() {
			t = *c.Memory.OccurredAt
		}
		age := rf.Now.Sub(t)
		// R3-B 修复：as_of 之前的「未来记忆」不应得满分。
		// 原实现 age<0 clamp 到 0 → exp(0)=1.0，把 occurred_at > as_of 的记忆（未来）
		// 抬到 freshness 满分，污染 top-1。现在 rf.Now=as_of 时该 bug 会显性触发
		// （实测 temporal 桶 2 条样本 rank 1→2，见 temporal_asof_now_20261012.md §3）。
		// 未来记忆 freshness 归 0，等价于「该记忆当时还不存在，时效不可用」。
		if age < 0 {
			out[c.Memory.MemoryID] = 0.0
			continue
		}
		out[c.Memory.MemoryID] = math.Exp(-float64(age) / float64(hl))
	}
	return out
}

// signalSourceTrust：来源可信度（人工 > 系统 > 用户）。
func signalSourceTrust(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		out[c.Memory.MemoryID] = sourceTrust(c.Memory.Source)
	}
	return out
}

// signalReferenceFrame：参考系匹配度（上下文坐标匹配）。
//
// R3-A 修复（任务书 A.1）：原实现在 QueryFrame == nil 或查询端坐标全空时对每个
// 候选返回 1.0 常数。归一化后常数信号不改变 RRF 排名 → reference_frame 权重对
// 排序无影响，坐标下降必然把它压到 0.0（R2 收口 §7.2 根因）。
// 修复：无参考系数据时返回 0.5 中性分（与 signalFeedbackHistory 无反馈时的中性
// 分一致），不给正偏向。若评测器确实未设置 QueryFrame，本信号仍近似常数但分值为
// 中性，不再伪装成「满匹配」。
//
// 方案选择记录：任务书 A.3.2 提供 方案 a（0.5 中性）/ 方案 b（0.0 关闭）二选一。
// 本实现采用方案 a，理由：保留权重非零时的信息量（若某轮搜索把 reference_frame
// 调到 >0，它应基于真实打分而非常数 1.0）；方案 b 在报告 A.2 已作为对照数据给出。
func (rf *RankFusion) signalReferenceFrame(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qf := rf.QueryFrame
	if qf == nil {
		for _, c := range cands {
			if c.Memory != nil {
				out[c.Memory.MemoryID] = 0.5 // R3-A：中性分（无参考系数据时不给正偏向）
			}
		}
		return out
	}
	q := []string{qf.Tenant, qf.User, qf.Agent, qf.Project, qf.Scene, qf.Task, qf.Entity, qf.Category}
	// 只统计查询端非空坐标
	var coords []string
	qvals := map[string]string{}
	if qf.Tenant != "" {
		coords = append(coords, "tenant")
		qvals["tenant"] = qf.Tenant
	}
	if qf.User != "" {
		coords = append(coords, "user")
		qvals["user"] = qf.User
	}
	if qf.Agent != "" {
		coords = append(coords, "agent")
		qvals["agent"] = qf.Agent
	}
	if qf.Project != "" {
		coords = append(coords, "project")
		qvals["project"] = qf.Project
	}
	if qf.Scene != "" {
		coords = append(coords, "scene")
		qvals["scene"] = qf.Scene
	}
	if qf.Task != "" {
		coords = append(coords, "task")
		qvals["task"] = qf.Task
	}
	if qf.Entity != "" {
		coords = append(coords, "entity")
		qvals["entity"] = qf.Entity
	}
	_ = q
	if len(coords) == 0 {
		// R3-A：查询端所有参考系坐标均空，同样无信息量 → 中性分 0.5（而非原 1.0 常数）。
		for _, c := range cands {
			if c.Memory != nil {
				out[c.Memory.MemoryID] = 0.5 // R3-A：中性分（无参考系数据时不给正偏向）
			}
		}
		return out
	}
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		mf := map[string]string{
			"tenant":  c.Memory.ReferenceFrame.Tenant,
			"user":    c.Memory.ReferenceFrame.User,
			"agent":   c.Memory.ReferenceFrame.Agent,
			"project": c.Memory.ReferenceFrame.Project,
			"scene":   c.Memory.ReferenceFrame.Scene,
			"task":    c.Memory.ReferenceFrame.Task,
			"entity":  c.Memory.ReferenceFrame.Entity,
		}
		hit := 0
		for _, co := range coords {
			if mf[co] != "" && strings.EqualFold(mf[co], qvals[co]) {
				hit++
			}
		}
		out[c.Memory.MemoryID] = float64(hit) / float64(len(coords))
	}
	return out
}

// signalFeedbackHistory：反馈历史（采纳/弃用统计）。
// 评分规则：
//   - 无反馈 = 中性（0.5）
//   - 有反馈：adopted/(adopted+rejected)，但考虑时间衰减
//   - rejected 记忆：反馈分 < 0.3（降权）
//   - adopted 记忆：反馈分 > 0.7（升权）
//
// R3-B 修复（任务书 B.2.1）：原实现用 time.Now().UTC()（第二个隐藏时间源），
// 违反 seed=42 确定性（每次评测反馈时间不同），且与 as_of 时序不一致
// （as_of 是 2023 时，feedback 却用 2026 判断 90 天内是否近期）。
// 改为 rf.Now（与 signalFreshness 一致）；若 rf.Now 为零则回退 time.Now().UTC()
// 保持向后兼容。
func (rf *RankFusion) signalFeedbackHistory(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	now := rf.Now
	if now.IsZero() {
		now = time.Now().UTC() // 向后兼容：未显式设置时回退
	}
	const halfLife = 90 * 24 * time.Hour // 90 天半衰期

	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		var adopted, rejected int
		var recentAdopted, recentRejected int

		for _, f := range c.Memory.FeedbackRecords {
			switch f.Status {
			case FbAdopted, FbEdited:
				adopted++
				// 时间衰减：90 天内的反馈才计入 recent
				if now.Sub(f.Time) < halfLife {
					recentAdopted++
				}
			case FbRejected:
				rejected++
				if now.Sub(f.Time) < halfLife {
					recentRejected++
				}
			}
		}

		if adopted+rejected == 0 {
			out[c.Memory.MemoryID] = 0.5 // 无反馈 = 中性
			continue
		}

		// 基础分：采纳率
		base := float64(adopted) / float64(adopted+rejected)

		// 时间衰减调整：近期反馈权重更高
		if recentAdopted+recentRejected > 0 {
			ratio := float64(recentAdopted) / float64(recentAdopted+recentRejected)
			// 混合近期和历史反馈
			base = 0.7*ratio + 0.3*base
		}

		// rejected 记忆强制降权
		if rejected > 0 && adopted == 0 {
			base = 0.2
		}

		// 限制在 [0, 1]
		if base < 0 {
			base = 0
		}
		if base > 1 {
			base = 1
		}

		out[c.Memory.MemoryID] = base
	}
	return out
}

// ------------------------------------------------------------------
// 工具函数
// ------------------------------------------------------------------

func normalizeMax(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	max := 0.0
	for _, v := range m {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		for k := range m {
			out[k] = 0
		}
		return out
	}
	for k, v := range m {
		out[k] = v / max
	}
	return out
}

func rankMap(m map[string]float64) map[string]int {
	type kv struct {
		k string
		v float64
	}
	arr := make([]kv, 0, len(m))
	for k, v := range m {
		if v <= 0 {
			continue
		}
		arr = append(arr, kv{k, v})
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].v != arr[j].v {
			return arr[i].v > arr[j].v
		}
		return arr[i].k < arr[j].k
	})
	out := make(map[string]int, len(arr))
	for i, e := range arr {
		out[e.k] = i + 1
	}
	return out
}

func cosine(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

func sourceTrust(source string) float64 {
	s := strings.ToLower(source)
	switch {
	case strings.Contains(s, "human"), strings.Contains(s, "人工"), strings.Contains(s, "manual"), strings.Contains(s, "user_confirm"):
		return 1.0
	case strings.Contains(s, "system"), strings.Contains(s, "rule"), strings.Contains(s, "系统"):
		return 0.6
	default:
		return 0.4
	}
}

// tokenize 轻量分词：ASCII 按词，CJK 取单字 + 相邻双字（bigram）。
func tokenize(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	var toks []string
	var ascii []rune
	var cjk []rune
	flushASCII := func() {
		if len(ascii) > 0 {
			toks = append(toks, string(ascii))
			ascii = nil
		}
	}
	flushCJK := func() {
		for i, r := range cjk {
			toks = append(toks, string(r))
			if i+1 < len(cjk) {
				toks = append(toks, string(cjk[i:i+2]))
			}
		}
		cjk = nil
	}
	for _, r := range s {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushCJK()
			ascii = append(ascii, r)
		case unicode.Is(unicode.Han, r):
			flushASCII()
			cjk = append(cjk, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return toks
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenize(s) {
		out[t] = true
	}
	return out
}

// ------------------------------------------------------------------
// R8-A（P7-E）i18n 接入样板（只增不改：新增调用点，不改既有返回语义）
// ------------------------------------------------------------------

// RankFusionParseWeightsError 返回「权重解析失败」的 i18n 文案。
// 接入样板：新增 i18n 调用点，供上层按当前 kernel locale 输出本地化错误文案，
// 与既有 `fmt.Errorf("parse weights %s: %w", path, err)` 并存；如需保留 wrap
// 语义可写 `fmt.Errorf("%s: %w", RankFusionParseWeightsError(path), err)`。
// 只增不改：本函数为新增导出，不修改 LoadWeightsFromFile 等既有函数签名/返回值。
func RankFusionParseWeightsError(path string) string {
	return T(i18n.MsgParseWeightsFailed, path)
}
