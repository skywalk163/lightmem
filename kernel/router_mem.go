// MemoryRouter 意图识别 + 跨层召回（§3.1 / §3.4）
// 真源：lightmem.md §3.1「MemoryRouter」、§3.4「多信号检索」、§2.2「四层温度分层」。
//
// 职责边界：
//   - A1（本文件）：规则意图识别（不调 LLM）+ 四层并行召回，输出未融合候选集。
//   - A2（rank_fusion.go）：对候选集做多信号 rank 融合（由 RecallAndFuse 调用）。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ------------------------------------------------------------------
// 意图识别（§3.1，规则表，不调 LLM）
// ------------------------------------------------------------------

// Intent 用户意图分类（§3.1）。
type Intent string

const (
	IntentQueryKnowledge    Intent = "query_knowledge"
	IntentRecordFact        Intent = "record_fact"
	IntentConfirmPreference Intent = "confirm_preference"
	IntentSessionOnly       Intent = "session_only"
)

// IntentRule 一条意图规则；Priority 越大越优先，便于后续扩展而不写长 if-else 链。
type IntentRule struct {
	Intent   Intent   `json:"intent"`
	Keywords []string `json:"keywords"`
	Priority int      `json:"priority"`
	Note     string   `json:"note,omitempty"`
}

// DefaultIntentRules 默认意图规则表（任务书 §2.2）。
func DefaultIntentRules() []IntentRule {
	return []IntentRule{
		{Intent: IntentConfirmPreference, Priority: 30, Keywords: []string{"偏好是", "我喜欢", "我习惯", "默认用", "以后用"}, Note: "确认偏好"},
		{Intent: IntentRecordFact, Priority: 20, Keywords: []string{"记住", "以后", "别忘了", "记一下", "保存一下", "提醒我"}, Note: "记录事实"},
		{Intent: IntentQueryKnowledge, Priority: 10, Keywords: []string{"查询", "帮我找", "有哪些", "是什么", "查一下", "告诉我", "谁", "什么时候"}, Note: "查询知识"},
		{Intent: IntentSessionOnly, Priority: 5, Keywords: []string{"临时", "这次", "仅本次", "当前会话", "随便问问"}, Note: "仅当前会话"},
	}
}

// ClassifyIntent 用默认规则表识别意图；无命中默认 query_knowledge。
func ClassifyIntent(text string) Intent {
	return ClassifyIntentByRules(text, DefaultIntentRules())
}

// ClassifyIntentByRules 按给定规则表识别意图。
// 规则：取命中的最高 Priority 规则；同优先级按规则表顺序；无命中 → query_knowledge。
func ClassifyIntentByRules(text string, rules []IntentRule) Intent {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return IntentQueryKnowledge
	}
	best := Intent("")
	bestPri := -1
	for _, r := range rules {
		hit := false
		for _, kw := range r.Keywords {
			if kw != "" && strings.Contains(t, strings.ToLower(kw)) {
				hit = true
				break
			}
		}
		if hit && r.Priority > bestPri {
			best = r.Intent
			bestPri = r.Priority
		}
	}
	if best == "" {
		return IntentQueryKnowledge
	}
	return best
}

// ------------------------------------------------------------------
// 请求 / 响应类型
// ------------------------------------------------------------------

// RouterContext 一次路由调用的输入上下文（§3.1）。
type RouterContext struct {
	Text            string       `json:"text"`
	TenantID        string       `json:"tenant_id"`
	UserID          string       `json:"user_id"`
	ProjectID       string       `json:"project_id,omitempty"`
	AgentID         string       `json:"agent_id,omitempty"`
	Scene           string       `json:"scene,omitempty"`
	Task            string       `json:"task,omitempty"`
	Entity          string       `json:"entity,omitempty"`
	IncludeArchived bool         `json:"include_archived,omitempty"`
	TopK            int          `json:"top_k,omitempty"`
	AsOf            *string      `json:"as_of,omitempty"`
	MemoryKinds     []MemoryKind `json:"memory_kinds,omitempty"`
	Visibility      []Visibility `json:"visibility,omitempty"`
	// ComposeExpand 组合记忆检索展开开关（R6-B，storage 期组合 / 千脑「组合物体」）。
	// 默认 false = 关闭（isolate 复现，等价于组合特性上线前）；
	// 置 true 时 RecallAndFuse 对组合记忆候选展开其子事实（见 expandCompositions）。
	// 对不含组合的既有数据集零影响（无 Components 候选不触发展开）。
	ComposeExpand bool `json:"compose_expand,omitempty"`
}

func (rc RouterContext) frame() *ReferenceFrame {
	return &ReferenceFrame{
		Tenant:  rc.TenantID,
		User:    rc.UserID,
		Agent:   rc.AgentID,
		Project: rc.ProjectID,
		Scene:   rc.Scene,
		Task:    rc.Task,
		Entity:  rc.Entity,
	}
}

// RecalledLayer 单层召回结果（§3.1）。
type RecalledLayer struct {
	Layer      string               `json:"layer"`
	Candidates []*RecalledCandidate `json:"candidates"`
	Count      int                  `json:"count"`
	Skipped    bool                 `json:"skipped,omitempty"`
	Note       string               `json:"note,omitempty"`
}

// RouterResult 路由结果（A1 融合前；A5 recall 融合后填充 Fused）。
type RouterResult struct {
	Intent       Intent          `json:"intent"`
	Recalled     []RecalledLayer `json:"recalled"`
	FusionScores any             `json:"fusion_scores"` // A1 阶段为 null，交 A2
	Fused        []FusedResult   `json:"fused,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	// Trace 检索 trace：完整检索链路，便于调试和审计。
	Trace *RetrievalTrace `json:"trace,omitempty"`
}

// TraceEntry 单条候选的 trace 条目（兼容旧版）。
type TraceEntry struct {
	MemoryID string             `json:"memory_id"`
	Fact     string             `json:"fact"`
	Score    float64            `json:"score"`
	Signals  map[string]float64 `json:"signals"`
}

// RetrievalTrace 完整检索 trace（§3.4）。
// 记录每层召回的候选数、每信号的打分、融合后的排名。
type RetrievalTrace struct {
	Query         string                 `json:"query"`
	Timestamp     string                 `json:"timestamp"`
	Layers        []TraceLayer           `json:"layers"`
	Signals       []TraceSignal          `json:"signals"`
	Ranked        []TraceRankedCandidate `json:"ranked"`
	Mode          string                 `json:"mode"`
	TotalCands    int                    `json:"total_candidates"`
	FilteredCands int                    `json:"filtered_candidates"`
}

// TraceLayer 单层召回的 trace。
type TraceLayer struct {
	Layer      string      `json:"layer"`
	Candidates []TraceCand `json:"candidates"`
	Count      int         `json:"count"`
	Skipped    bool        `json:"skipped,omitempty"`
	Note       string      `json:"note,omitempty"`
}

// TraceCand 单条候选的 trace。
type TraceCand struct {
	MemoryID string  `json:"memory_id"`
	Fact     string  `json:"fact"`
	Score    float64 `json:"score"`
	Layer    string  `json:"layer"`
}

// TraceSignal 单信号的 trace。
type TraceSignal struct {
	Name   string             `json:"name"`
	Weight float64            `json:"weight"`
	Type   string             `json:"type"`
	Scores map[string]float64 `json:"scores"`
}

// TraceRankedCandidate 融合后的排名候选。
type TraceRankedCandidate struct {
	MemoryID             string             `json:"memory_id"`
	Fact                 string             `json:"fact"`
	Score                float64            `json:"score"`
	Rank                 int                `json:"rank"`
	Ranks                map[string]int     `json:"ranks,omitempty"`
	SignalScores         map[string]float64 `json:"signal_scores,omitempty"`
	Layer                string             `json:"layer"`
	LowConfidence        bool               `json:"low_confidence,omitempty"`
	RequiresConfirmation bool               `json:"requires_confirmation,omitempty"`
}

// ------------------------------------------------------------------
// 跨层召回
// ------------------------------------------------------------------

// defaultRecallTopK 每层默认 top-k（任务书 §2.3）。
const defaultRecallTopK = 5

// Route 执行 A1：意图识别 + 四层并行召回，返回未融合候选集。
func (s *APIServer) Route(ctx context.Context, rc RouterContext) (RouterResult, error) {
	res := RouterResult{
		Intent:       ClassifyIntent(rc.Text),
		FusionScores: nil,
	}
	layers, err := s.RecallLayers(ctx, rc)
	if err != nil {
		return res, err
	}
	res.Recalled = layers
	return res, nil
}

// RecallAndFuse 执行 A1+A2：跨层召回后做多信号 rank 融合。
// FeedbackLoop：过滤用户拒绝的记忆（rejectedIDs）。
func (s *APIServer) RecallAndFuse(ctx context.Context, rc RouterContext) (RouterResult, error) {
	res, err := s.Route(ctx, rc)
	if err != nil {
		return res, err
	}
	var flat []RecalledCandidate
	for _, l := range res.Recalled {
		for _, c := range l.Candidates {
			if c != nil && c.Memory != nil {
				// 过滤已拒绝的记忆（R10-A：加锁读）
				if !s.isRejectedID(c.Memory.MemoryID) {
					flat = append(flat, *c)
				}
			}
		}
	}
	rf := s.fusion
	var nowVal time.Time
	// R10-A：并发安全加固。修复前直接写共享 rf 的 Now/AsOf/QueryFrame
	// （本行原逻辑），并发请求下构成跨 goroutine 数据竞争（生产 main.go
	// SetRankFusion(DefaultRankFusion()) 后所有请求共享同一实例）。现改为
	// 构造请求级实例：配置（权重/常量/embedder）继承模板，请求状态独立。
	// 融合算法与逐位输出不回归（Fuse 未动；Now/AsOf/QueryFrame 值语义不变）。
	if rf == nil {
		rf = DefaultRankFusion()
	}
	// 确定性：仅当注入方未显式设置 Now 时才用当前时刻。
	// 评测集跑批时由 run_kpi.go 固定 Now，避免逐条 freshness 抖动。
	if rf.Now.IsZero() {
		nowVal = time.Now().UTC()
	} else {
		nowVal = rf.Now
	}
	rf = rf.ForRequest(nowVal, rc.AsOf, rc.frame())
	fused := rf.Fuse(ctx, rc.Text, flat)
	// R4-A：二次 hop 证据重排（hop 链式推理 + 上下文增强）。
	// 仅当调用方显式开启（APIServer.SetMultiHopConfig / RecallAndFuseMultiHop）时生效；
	// 默认 Enabled=false → 本行不改变任何行为，单跳路径逐位不变。
	if s.multiHop.Enabled {
		fused = MultiHopReRank(rc.Text, fused, s.multiHop)
	}
	// R6-B：组合记忆检索展开（千脑「组合物体」）。仅当调用方显式开启
	// rc.ComposeExpand 且候选含非空 Components 时生效；默认 false → 逐位保持
	// 既有单跳/多跳行为（对不含组合的既有数据集零影响）。
	if rc.ComposeExpand {
		fused = s.expandCompositions(ctx, rc, fused)
	}
	res.Fused = fused

	// 指标：记录检索结果
	metrics := GetMetricsCollector()
	if len(fused) > 0 {
		metrics.RecordHit()
	} else {
		metrics.RecordMiss()
	}
	// 记录层容量
	for _, l := range res.Recalled {
		metrics.SetLayerCount(MemoryLayer(l.Layer), int64(l.Count))
	}

	// 构建完整检索 trace
	trace := &RetrievalTrace{
		Query:         rc.Text,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Mode:          res.Mode,
		TotalCands:    len(flat),
		FilteredCands: len(flat),
	}

	// 填充各层召回信息
	for _, l := range res.Recalled {
		layer := TraceLayer{
			Layer:   l.Layer,
			Count:   l.Count,
			Skipped: l.Skipped,
			Note:    l.Note,
		}
		for _, c := range l.Candidates {
			if c != nil && c.Memory != nil {
				layer.Candidates = append(layer.Candidates, TraceCand{
					MemoryID: c.Memory.MemoryID,
					Fact:     c.Memory.Fact,
					Score:    c.RecallScore,
					Layer:    string(c.Layer),
				})
			}
		}
		trace.Layers = append(trace.Layers, layer)
	}

	// 填充各信号打分
	for _, sig := range rf.Signals {
		signal := TraceSignal{
			Name:   sig.Name,
			Weight: sig.Weight,
			Type:   sig.Type,
			Scores: map[string]float64{},
		}
		// 重新计算该信号的打分
		raw := sig.Fn(ctx, rc.Text, flat)
		for id, score := range raw {
			signal.Scores[id] = score
		}
		trace.Signals = append(trace.Signals, signal)
	}

	// 融合后的排名
	for i, f := range fused {
		ranked := TraceRankedCandidate{
			MemoryID:             f.MemoryID,
			Fact:                 f.Fact,
			Score:                f.Score,
			Rank:                 i + 1,
			Ranks:                f.Ranks,
			SignalScores:         f.SignalScores,
			Layer:                string(f.Layer),
			LowConfidence:        f.LowConfidence,
			RequiresConfirmation: f.RequiresConfirmation,
		}
		trace.Ranked = append(trace.Ranked, ranked)
	}

	res.Trace = trace

	if len(fused) == 0 {
		res.Mode = "empty"
	} else if fused[0].RequiresConfirmation {
		res.Mode = "conflict"
	} else if fused[0].LowConfidence {
		res.Mode = "low_confidence"
	} else if len(fused) > 1 {
		res.Mode = "fused"
	} else {
		res.Mode = "unique"
	}
	return res, nil
}

// RecallLayers 四层并行召回（§2.2 + §2.3）。
// 硬过滤先做：tenant/user/project/visibility（§3.2 安全硬过滤清单）。
func (s *APIServer) RecallLayers(ctx context.Context, rc RouterContext) ([]RecalledLayer, error) {
	sc, ok := s.backend.(LayerScanner)
	if !ok {
		return nil, fmt.Errorf("backend %s does not implement LayerScanner; cross-layer recall requires it", s.backend.Name())
	}
	topK := rc.TopK
	if topK <= 0 {
		topK = defaultRecallTopK
	}
	maxScan := topK * 40
	if maxScan < 200 {
		maxScan = 200
	}
	needle := strings.ToLower(strings.TrimSpace(rc.Text))

	out := make([]RecalledLayer, 0, 4)
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		rl := RecalledLayer{Layer: string(layer)}
		if layer == LayerL3 && !rc.IncludeArchived {
			rl.Skipped = true
			rl.Note = "archived layer skipped (set include_archived=true to include)"
			out = append(out, rl)
			continue
		}
		ms, err := sc.ScanLayer(ctx, layer, rc.TenantID, maxScan)
		if err != nil {
			return out, fmt.Errorf("recall layer %s: %w", layer, err)
		}
		cands := make([]*RecalledCandidate, 0, len(ms))
		for _, m := range ms {
			if m == nil {
				continue
			}
			if !m.canView(rc.UserID, rc.TenantID) {
				continue
			}
			if rc.ProjectID != "" && m.ProjectID != "" && m.ProjectID != rc.ProjectID {
				continue
			}
			if len(rc.MemoryKinds) > 0 && !containsKind(rc.MemoryKinds, m.MemoryKind) {
				continue
			}
			if rc.AsOf != nil {
				// 时序回溯（§3.4 双时态）：只检查 valid_from/valid_until，不检查 superseded_by（历史版本可能已被取代）。
				if m.ValidFrom != "" && *rc.AsOf < m.ValidFrom {
					continue
				}
				if m.ValidUntil != "" && *rc.AsOf > m.ValidUntil {
					continue
				}
			} else {
				// 默认只返回当前生效版本（§3.4 时效裁决）
				if m.SupersededBy != nil {
					continue
				}
				if !m.visibleAt(time.Now().UTC().Format("2006-01-02")) {
					continue
				}
			}
			score := recallScore(m, needle)
			if needle != "" && score == 0 {
				continue
			}
			score += m.Confidence * 0.2
			cands = append(cands, &RecalledCandidate{Memory: m, Layer: layer, RecallScore: score})
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].RecallScore != cands[j].RecallScore {
				return cands[i].RecallScore > cands[j].RecallScore
			}
			return cands[i].Memory.MemoryID < cands[j].Memory.MemoryID
		})
		if len(cands) > topK {
			cands = cands[:topK]
		}
		rl.Candidates = cands
		rl.Count = len(cands)
		out = append(out, rl)
	}
	return out, nil
}

func recallScore(m *Memory, needle string) float64 {
	if needle == "" {
		return m.Confidence
	}
	fact := strings.ToLower(m.Fact)
	switch {
	case fact == needle:
		return 1.0
	case strings.Contains(fact, needle):
		return 0.7
	}
	// 词元回退：查询与事实的分词重合度
	q := tokenize(needle)
	if len(q) == 0 {
		return 0
	}
	fset := tokenSet(m.Fact + " " + m.ReferenceFrame.Entity)
	hit := 0
	for _, t := range q {
		if fset[t] {
			hit++
		}
	}
	if hit == 0 {
		return 0
	}
	return 0.5 * float64(hit) / float64(len(q))
}

// ------------------------------------------------------------------
// HTTP 端点：POST /mem/router
// ------------------------------------------------------------------

func (s *APIServer) handleMemRouter(w http.ResponseWriter, r *http.Request) {
	var rc RouterContext
	if err := json.NewDecoder(r.Body).Decode(&rc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if rc.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.Route(ctx, rc)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
