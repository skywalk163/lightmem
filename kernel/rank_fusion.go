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

	embedder func(string) ([]float32, error) // 可选：真实语义向量化（nil 时用召回分近似）
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
		{Name: "feedback", Weight: rf.Weights["feedback"], Type: "fixed", Fn: signalFeedbackHistory},
	}
	return rf
}

// SetEmbedder 注入真实语义向量化函数（可选）。
func (rf *RankFusion) SetEmbedder(f func(string) ([]float32, error)) { rf.embedder = f }

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
func (rf *RankFusion) Rank(ctx context.Context, query string, cands []RecalledCandidate) []FusedResult {
	rf.ensureDefaults()
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

	// VoteFusion：对 top-3 候选做加权平均，提升多版本投票的鲁棒性。
	// 权重 = 各信号归一化分 × 名次衰减（top1:1.0, top2:0.5, top3:0.3）。
	if len(ranked) >= 3 {
		w1, w2, w3 := ranked[0].Score, ranked[1].Score, ranked[2].Score
		total := w1 + w2 + w3
		if total > 0 {
			ranked[0].Score = (w1*w1 + w2*w2*0.5 + w3*w3*0.3) / total
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
		if rf.isConflict(top1.Memory, top2.Memory) {
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
func (rf *RankFusion) signalSemantic(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	if rf.embedder != nil && query != "" {
		if qv, err := rf.embedder(query); err == nil {
			for _, c := range cands {
				if c.Memory == nil {
					continue
				}
				if cv, err := rf.embedder(c.Memory.Fact); err == nil {
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
		if age < 0 {
			age = 0
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
func (rf *RankFusion) signalReferenceFrame(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qf := rf.QueryFrame
	if qf == nil {
		for _, c := range cands {
			if c.Memory != nil {
				out[c.Memory.MemoryID] = 1.0
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
		for _, c := range cands {
			if c.Memory != nil {
				out[c.Memory.MemoryID] = 1.0
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
func signalFeedbackHistory(_ context.Context, _ string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		var adopted, rejected int
		for _, f := range c.Memory.FeedbackRecords {
			switch f.Status {
			case FbAdopted, FbEdited:
				adopted++
			case FbRejected:
				rejected++
			}
		}
		if adopted+rejected == 0 {
			out[c.Memory.MemoryID] = 0.5 // 无反馈 = 中性
			continue
		}
		out[c.Memory.MemoryID] = float64(adopted) / float64(adopted+rejected)
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
