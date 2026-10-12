// R4-A：multi-hop 二次 hop 证据重排（hop 链式推理 + 上下文增强）。
//
// 真源：lightmem.md §3.4「多信号检索」、§3.1「MemoryRouter」。
//
// 背景（R3-D §4.3 结论）：合成集与公开基准 multi-hop 的「口径 A」差距 0.055 是
// **真实检索排序能力差异**，不是结构约束——公开基准 165 条 multi-hop 中 6 条
// 「target 在 top-2 但被干扰记忆挤到第二」，3 条「target 未进 top-2」。
// 因此本文件在 A2（rank_fusion.go 的多信号融合）之后再叠一层 **hop 证据重排**，
// 用「候选是否真正回答了该 query」的确定性证据修正融合序。
//
// 设计（与 R4-A 交付报告口径一致）：
//   - hop-1：A1+A2 已产出的融合候选集（MultiHopReRank 的输入，原序即原排名）。
//   - hop 锚点：从 query 抽取内容词锚点（复用 kernel/tokenize 的 ASCII 词 + 汉字
//     单字/二元组切分），剔除功能词（hopAnchorStopWords）。
//   - L1 证据「直接回答」：候选 Fact 归一化后**完整包含** query → 该记忆本身就是
//     这道问题的答案卡（LongMemEval 形态 question => answer），为最强证据。
//   - L2 证据「锚点覆盖」：|query 锚点 ∩ 候选切分词| / |query 锚点|，衡量候选
//     事实文本与 query 的证据重合度。
//   - L3（可选，Depth>=2）：上下文增强——以同参考系且时间窗内的 hop-2 邻接记忆
//     事实文本补强该候选的锚点覆盖，并记录推理链
//     `Reason = "multi-hop: <hop1_id> -> <hop2_id>"`。
//
// 确定性红线（R3/R4 红线 5）：不调 LLM；不使用 time.Now()；不使用 map 迭代序
// （只做计数，结果顺序无关；锚点集合显式排序）；排序用 sort.SliceStable + 全序比较键。
// Schema 红线（红线 1）：只读 FusedResult/Memory 既有字段，不修改任何 Schema 语义。
//
// 行为保证：MultiHopConfig{Enabled:false}（默认）时 RecallAndFuse 逐位不变，
// 单跳路径零行为差异；评测器 -no-multihop 可 isolate 复现 R3-D 基线。
package kernel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MultiHopConfig 二次 hop 证据重排配置（R4-A 新增；只增不改）。
type MultiHopConfig struct {
	// Enabled 是否启用二次 hop 证据重排。false（默认）时 RecallAndFuse 逐位不变。
	Enabled bool `json:"enabled"`
	// Depth hop 链式推理深度：
	//   <=1：只用 L1/L2（候选自身证据）；
	//   >=2：追加 L3 上下文增强（邻接记忆补强）。
	// 默认 1——L3 在双源实测中对 multi-hop hit@2 有回归，默认关闭（见交付报告）。
	Depth int `json:"depth"`
	// ContextWindow 上下文邻接的时间窗（<=0 表示不做上下文增强）。默认 0。
	ContextWindow time.Duration `json:"context_window_ns,omitempty"`
	// MinAnchors query 锚点数下限；锚点数不足时跳过重排（原序返回）。默认 1。
	MinAnchors int `json:"min_anchors"`
}

// DefaultMultiHopConfig 返回默认配置：关闭（逐位兼容 R3 既有行为）。
//
// ⚠️ 默认关闭是刻意的：kernel 的单跳检索路径必须保持既有行为，
// multi-hop 二次重排由调用方显式开启（评测器 -no-multihop 可 isolate 对照）。
func DefaultMultiHopConfig() MultiHopConfig {
	return MultiHopConfig{Enabled: false, Depth: 1, ContextWindow: 0, MinAnchors: 1}
}

// hopAnchorStopWords query 锚点抽取时剔除的功能词。
// 只影响锚点集合（从而覆盖率分母），不改动既有 signal 的词表。
var hopAnchorStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true,
	"for": true, "how": true, "many": true, "much": true, "what": true, "when": true,
	"who": true, "did": true, "do": true, "does": true, "i": true, "my": true, "me": true,
	"it": true, "its": true, "and": true, "or": true, "at": true, "by": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "been": true, "he": true, "she": true,
	"they": true, "you": true, "your": true, "we": true, "our": true, "this": true,
	"that": true, "these": true, "those": true, "have": true, "has": true, "had": true,
	"will": true, "would": true, "can": true, "could": true, "should": true, "any": true,
	"all": true, "some": true, "more": true, "most": true, "other": true, "than": true,
	"then": true, "there": true, "here": true, "from": true, "with": true, "about": true,
	"into": true, "over": true, "under": true, "again": true, "very": true, "just": true,
	"also": true,
}

// HopAnchors 抽取 query 的内容词锚点（去重后按字典序排序，保证确定性）。
// 分词复用 kernel 既有的 tokenize（ASCII 词 + 汉字单字/二元组），与 R3 信号口径同源。
func HopAnchors(query string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, t := range tokenize(query) {
		if hopAnchorStopWords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out) // map 迭代序不可依赖 → 显式排序，保证任何环境下顺序一致
	return out
}

// hopNormalize 归一化：小写 + 空白压缩（用于「候选原文完整包含 query」判定）。
func hopNormalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

// HopFactTokens 候选事实文本的切分词集合（供覆盖率计算复用）。
func HopFactTokens(fact string) map[string]bool {
	set := map[string]bool{}
	for _, t := range tokenize(fact) {
		set[t] = true
	}
	return set
}

// HopAnchorCoverage 锚点覆盖率：|anchors ∩ factTokens| / |anchors| ∈ [0,1]。
// 计数与顺序无关，结果确定。
func HopAnchorCoverage(anchors []string, factTokens map[string]bool) float64 {
	if len(anchors) == 0 {
		return 0
	}
	hit := 0
	for _, a := range anchors {
		if factTokens[a] {
			hit++
		}
	}
	return float64(hit) / float64(len(anchors))
}

// hopOccurredAt 取候选记忆的发生时间（缺失返回零值 → 不参与上下文邻接判定）。
func hopOccurredAt(f FusedResult) time.Time {
	if f.Memory == nil {
		return time.Time{}
	}
	if f.Memory.OccurredAt != nil {
		return f.Memory.OccurredAt.UTC()
	}
	return f.Memory.RecordedAt.UTC()
}

// hopSameFrame 判定两条候选是否处于同一参考系邻接（同 tenant/user/scene）。
// L3 上下文增强只在该范围内拼链，避免跨场景拼接出虚假推理链。
func hopSameFrame(a, b FusedResult) bool {
	ma, mb := a.Memory, b.Memory
	if ma == nil || mb == nil {
		return false
	}
	if ma.TenantID != mb.TenantID || ma.UserID != mb.UserID {
		return false
	}
	sa, sb := ma.ReferenceFrame.Scene, mb.ReferenceFrame.Scene
	return sa != "" && sa == sb
}

// hopContextAugment L3 上下文增强：在时间窗内找给该候选补充最多新锚点的邻接记忆。
// 返回邻接记忆 ID 与增强后的覆盖率；无增益时返回空 ID 与原覆盖率。
//
// 确定性：按候选切片原序遍历，只做数值比较；同增益取先出现者（由遍历顺序保证）。
func hopContextAugment(idx int, base float64, anchors []string, selfTokens map[string]bool,
	fused []FusedResult, cfg MultiHopConfig) (string, float64) {
	if cfg.Depth < 2 || cfg.ContextWindow <= 0 || len(fused) < 2 || len(anchors) == 0 {
		return "", base
	}
	self := fused[idx]
	tSelf := hopOccurredAt(self)
	if tSelf.IsZero() {
		return "", base
	}
	bestID, best := "", base
	for j := range fused {
		if j == idx || !hopSameFrame(self, fused[j]) {
			continue
		}
		tj := hopOccurredAt(fused[j])
		if tj.IsZero() {
			continue
		}
		d := tj.Sub(tSelf)
		if d < 0 {
			d = -d
		}
		if d > cfg.ContextWindow {
			continue
		}
		otherTokens := HopFactTokens(fused[j].Fact)
		newHit := 0
		for _, a := range anchors {
			if !selfTokens[a] && otherTokens[a] {
				newHit++
			}
		}
		if newHit == 0 {
			continue
		}
		cover := base + float64(newHit)/float64(len(anchors))
		if cover > 1 {
			cover = 1
		}
		if cover > best {
			best, bestID = cover, fused[j].MemoryID
		}
	}
	return bestID, best
}

// MultiHopRankKey 单条候选的 hop 排序键（导出供评测器 -diag-mh 诊断打印复用）。
// 返回：是否「直接回答 query」（L1）与锚点覆盖率（L2）。纯字符串处理，确定性。
func MultiHopRankKey(query string, f FusedResult, anchors []string) (bool, float64) {
	nq := hopNormalize(query)
	nf := hopNormalize(f.Fact)
	answers := nq != "" && nf != "" && strings.Contains(nf, nq)
	cover := HopAnchorCoverage(anchors, HopFactTokens(f.Fact))
	return answers, cover
}

// MultiHopReRank 二次 hop 证据重排（纯函数，便于单测与 isolate 对照）。
//
// 输入：query、A2 融合后的候选（按原序传入）与配置。
// 输出：重排后的候选切片（新切片，不原地改序）；
// cfg.Enabled=false、候选数 <2 或锚点数不足时**原序返回**。
//
// 排序键（全序，同键保留原序）：
//  1. L1 直接回答（answersQuery）优先；
//  2. L2/L3 锚点覆盖率高的优先；
//  3. 其余保持原融合序。
//
// Reason 标注（不参与打分，仅供 trace/诊断）：
// 有 hop-2 贡献 → "multi-hop: <id> -> <hop2_id> (answers=.. cover=..)"；
// 否则有证据 → "multi-hop: <id> (answers=.. cover=..)"。
func MultiHopReRank(query string, fused []FusedResult, cfg MultiHopConfig) []FusedResult {
	out := make([]FusedResult, len(fused))
	copy(out, fused)
	if !cfg.Enabled || len(out) < 2 {
		return out
	}
	anchors := HopAnchors(query)
	minAnchors := cfg.MinAnchors
	if minAnchors <= 0 {
		minAnchors = 1
	}
	if len(anchors) < minAnchors {
		return out
	}
	type rankKey struct {
		answers  bool
		coverage float64
		idx      int
	}
	keys := make([]rankKey, len(out))
	hop2 := make([]string, len(out))
	for i, f := range out {
		answers, cover := MultiHopRankKey(query, f, anchors)
		if id, aug := hopContextAugment(i, cover, anchors, HopFactTokens(f.Fact), out, cfg); aug > cover {
			hop2[i] = id
			cover = aug
		}
		keys[i] = rankKey{answers: answers, coverage: cover, idx: i}
	}
	// 按下标排序后再物化，避免比较器里反查元素（O(n log n)，确定性）。
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ka, kb := keys[order[a]], keys[order[b]]
		if ka.answers != kb.answers {
			return ka.answers
		}
		if ka.coverage != kb.coverage {
			return ka.coverage > kb.coverage
		}
		return ka.idx < kb.idx // 同键保留原融合序
	})
	res := make([]FusedResult, len(out))
	for i, oi := range order {
		res[i] = out[oi]
		k := keys[oi]
		if hop2[oi] != "" {
			res[i].Reason = fmt.Sprintf("multi-hop: %s -> %s (answers=%t cover=%.2f)",
				res[i].MemoryID, hop2[oi], k.answers, k.coverage)
		} else if k.answers || k.coverage > 0 {
			res[i].Reason = fmt.Sprintf("multi-hop: %s (answers=%t cover=%.2f)",
				res[i].MemoryID, k.answers, k.coverage)
		}
	}
	return res
}

// SetMultiHopConfig 设置 multi-hop 二次重排配置（传 Enabled:false 恢复原行为）。
func (s *APIServer) SetMultiHopConfig(cfg MultiHopConfig) { s.multiHop = cfg }

// MultiHopConfig 返回当前 multi-hop 配置。
func (s *APIServer) MultiHopConfig() MultiHopConfig { return s.multiHop }

// RecallAndFuseMultiHop 显式走 multi-hop 路径：等价于 RecallAndFuse，
// 但强制启用二次 hop 证据重排（忽略 cfg.Enabled），便于调用方按场景选择。
// 调用期间临时改写配置，返回前恢复（不改变调用方设定的长期配置）。
func (s *APIServer) RecallAndFuseMultiHop(ctx context.Context, rc RouterContext) (RouterResult, error) {
	saved := s.multiHop
	cfg := saved
	cfg.Enabled = true
	s.multiHop = cfg
	defer func() { s.multiHop = saved }()
	return s.RecallAndFuse(ctx, rc)
}
