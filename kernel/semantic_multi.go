// kernel/semantic_multi.go — R7-A semantic 单点冗余加固（Q1）。
//
// 理论依据（《千脑智能》千脑概念 7「多模型冗余=优雅降级」）：
//   - R6-D 消融证明 semantic 是唯一单点（关闭后 M1 0.9525→0.7458，−20.68pp，
//     保留率 0.725，远超 5pp 红线）；理想状态是「去掉任一路，系统仍然能工作」。
//   - 语义理解不应依赖单条通路（单向量 + 单相似度度量），而应有多条**互补**的
//     语义路径同时工作：任一路失效，其余仍能产出正确解释。
//
// 与既有实现的边界（红线 1「只增不改」）：
//   - 不修改 signalSemantic 既有路径行为（SemanticRedundant=false 时逐位不变）；
//   - 不修改 Memory / RouterContext / RecalledCandidate 既有字段语义；
//   - 多路径是 signalSemantic 内部的**旁路扩展**，默认关闭（flag 门控）。
//
// 三条互补路径（确定性，零 LLM / 零随机源）：
//   - 路径 0（semantic-path0，原向量路径）：rf.embedder 的余弦相似度，
//     语义消歧主力，保留原实现不动；
//   - 路径 1（semantic-path1，slot-value 结构匹配）：query 与候选按槽名
//     （entity / attr / value / anchor）对齐，值重叠度加权，零向量依赖；
//   - 路径 2（semantic-path2，token 共现加权确定性相似度）：query 与候选的
//     token 双向共现 + IDF 加权，覆盖语义路径 0 对短 query 的冷启动盲区。
//
// 集成：三条路径各自产出 top-K 解释 → kernel.ConsensusVoter 倒数秩投票
// → 语义共识序 + 语义分歧度（side-channel，可复用做「低置信」判定）。
//
// 确定性（红线 5）：本文件无 shuffle / 无 time.Now / 无 map 迭代序依赖；
// 投票平局按 Source 名字典序打破（ConsensusVoter 既有语义）。
package kernel

import (
	"context"
	"math"
	"regexp"
	"strings"
)

// Semantic 多路径常量（供评测器/flag 用）。
const (
	// SemanticPathVector 路径 0：原向量余弦（保留原实现）。
	SemanticPathVector = "semantic-path0"
	// SemanticPathSlotValue 路径 1：slot-value 结构匹配（零向量依赖）。
	SemanticPathSlotValue = "semantic-path1"
	// SemanticPathTokenCo 路径 2：token 共现加权确定性相似度。
	SemanticPathTokenCo = "semantic-path2"
	// SemanticMultiPathTopK 每条路径参与投票的解释深度（与 VoteTopK 同量级）。
	SemanticMultiPathTopK = 5
)

// semanticPathOrder 路径固定顺序（确定性：与文件常量序一致，非 map 迭代序）。
var semanticPathOrder = []string{
	SemanticPathVector,
	SemanticPathSlotValue,
	SemanticPathTokenCo,
}

// semanticPathScore 计算单条语义路径的打分（memory_id -> 原始分 ≥0）。
func (rf *RankFusion) semanticPathScore(ctx context.Context, path string, query string, cands []RecalledCandidate) map[string]float64 {
	switch path {
	case SemanticPathSlotValue:
		return semanticPathSlotValue(ctx, query, cands)
	case SemanticPathTokenCo:
		return semanticPathTokenCo(ctx, query, cands)
	case SemanticPathVector:
		return rf.semanticPathVector(ctx, query, cands)
	}
	return map[string]float64{}
}

// semanticPathVector 路径 0：原向量余弦（与 signalSemantic 原实现同源）。
func (rf *RankFusion) semanticPathVector(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	if rf.embedder == nil || query == "" {
		// 无 embedder 时回退召回粗分（与 signalSemantic 原实现一致）。
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

// ------------------------------------------------------------------
// 路径 1：slot-value 结构匹配（零向量依赖）
// ------------------------------------------------------------------

// slotPair 单个槽值对（键 = 槽名，值 = 槽值）。
type slotPair struct {
	Key string
	Val string
}

var (
	// slotEntityRe 匹配实体/决策人/项目/场景词（「客户A」「决策人000」等）。
	slotEntityRe = regexp.MustCompile(`(客户|项目|场景|决策人)[0-9A-Za-z]+`)
	// slotAttrOfRe 匹配「X的Y」形态的属性段（捕获 Y）。
	slotAttrOfRe = regexp.MustCompile(`的(.+?)(?:是|在|为|$|（)`)
	// slotAttrPrefRe 匹配「X偏好Y」形态的属性段。
	slotAttrPrefRe = regexp.MustCompile(`(偏好|准备|预算|决策人)`)
	// slotAnchorRe 匹配行尾全角括号锚。
	slotAnchorRe = regexp.MustCompile(`（(.+?)）`)
	// slotDeciderRe 匹配括号内的「决策人：X」。
	slotDeciderRe = regexp.MustCompile(`决策人[:：]\s*(\S+)`)
	// slotValueIsRe 匹配「X的Y是Z」形态的值段（捕获 Z）。
	slotValueIsRe = regexp.MustCompile(`是(.+)$`)
)

// parseQuerySlots 从 query 抽 slot-value 对（确定性正则）。
//
// 输出槽名：entity（主体）、attr（属性/谓词）、anchor（括号锚）、decider（决策人）。
// 例：「客户A的偏好是什么（周一）」→ {entity:客户A, attr:偏好, anchor:周一}
//
//	「客户F偏好什么时间开会（决策人：决策人000）」→ {entity:客户F, decider:决策人000}
func parseQuerySlots(query string) []slotPair {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	var slots []slotPair
	// entity
	if m := slotEntityRe.FindString(q); m != "" {
		slots = append(slots, slotPair{"entity", m})
	}
	// attr：优先「X的Y」的 Y，其次「偏好/准备/预算/决策人」
	body := q
	if i := strings.LastIndex(q, "（"); i >= 0 {
		body = strings.TrimSpace(q[:i])
	}
	if m := slotAttrOfRe.FindStringSubmatch(body); len(m) == 2 {
		slots = append(slots, slotPair{"attr", strings.TrimSpace(m[1])})
	} else if m := slotAttrPrefRe.FindString(body); m != "" {
		slots = append(slots, slotPair{"attr", m})
	}
	// anchor
	if m := slotAnchorRe.FindStringSubmatch(q); len(m) == 2 {
		inner := strings.TrimSpace(m[1])
		// 若含「决策人：X」，单独抽 decider
		if dm := slotDeciderRe.FindStringSubmatch(inner); len(dm) == 2 {
			slots = append(slots, slotPair{"decider", strings.TrimSpace(dm[1])})
			// 剩余部分仍作为 anchor
			rest := strings.TrimSpace(slotDeciderRe.ReplaceAllString(inner, ""))
			rest = strings.Trim(rest, "：:,，")
			if rest != "" {
				slots = append(slots, slotPair{"anchor", rest})
			}
		} else {
			slots = append(slots, slotPair{"anchor", inner})
		}
	}
	return slots
}

// parseFactSlots 从候选 fact 抽 slot-value 对（确定性正则）。
//
// 输出槽名：entity（主体）、attr（属性）、value（事实值）。
// 例：「客户A的偏好是周一」→ {entity:客户A, attr:偏好, value:周一}
//
//	「决策人000偏好周三开会」→ {entity:决策人000, attr:偏好, value:周三开会}
func parseFactSlots(fact string) []slotPair {
	f := strings.TrimSpace(fact)
	if f == "" {
		return nil
	}
	var slots []slotPair
	if m := slotEntityRe.FindString(f); m != "" {
		slots = append(slots, slotPair{"entity", m})
	}
	// attr：优先「X的Y」的 Y
	if m := slotAttrOfRe.FindStringSubmatch(f); len(m) == 2 {
		slots = append(slots, slotPair{"attr", strings.TrimSpace(m[1])})
	} else if m := slotAttrPrefRe.FindString(f); m != "" {
		slots = append(slots, slotPair{"attr", m})
	}
	// value：「X的Y是Z」的 Z，或「X偏好Y」的 Y
	if m := slotValueIsRe.FindStringSubmatch(f); len(m) == 2 {
		slots = append(slots, slotPair{"value", strings.TrimSpace(m[1])})
	} else if idx := strings.Index(f, "偏好"); idx >= 0 {
		v := strings.TrimSpace(f[idx+len("偏好"):])
		if v != "" {
			slots = append(slots, slotPair{"value", v})
		}
	}
	return slots
}

// slotGet 取指定槽名的值（无则返回空）。
func slotGet(slots []slotPair, key string) string {
	for _, s := range slots {
		if s.Key == key {
			return s.Val
		}
	}
	return ""
}

// slotValueOverlap 判断两值是否等价（EqualFold + 双向包含，边界对齐）。
func slotValueOverlap(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// semanticPathSlotValue 路径 1：query 与候选的 slot-value 结构对齐打分。
//
// 权重（确定性）：
//   - entity 匹配：+0.5（主体对齐，语义消歧核心）
//   - attr 匹配：+0.2（谓词/属性对齐）
//   - value ↔ anchor/decider 匹配：+0.3（跨槽值语义桥接）
//
// 满分 1.0；无任何槽位对齐返回 0。零向量依赖（不调用 embedder）。
func semanticPathSlotValue(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qSlots := parseQuerySlots(query)
	if len(qSlots) == 0 {
		return out
	}
	qEntity := slotGet(qSlots, "entity")
	qAttr := slotGet(qSlots, "attr")
	qAnchor := slotGet(qSlots, "anchor")
	qDecider := slotGet(qSlots, "decider")
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		fSlots := parseFactSlots(c.Memory.Fact)
		if len(fSlots) == 0 {
			continue
		}
		var score float64
		// entity 对齐：query.entity 与 fact.entity 直接匹配，
		// 或 query.decider 与 fact.entity 桥接（多跳语义）。
		if qEntity != "" && slotValueOverlap(qEntity, slotGet(fSlots, "entity")) {
			score += 0.5
		} else if qDecider != "" && slotValueOverlap(qDecider, slotGet(fSlots, "entity")) {
			score += 0.4 // 决策人桥接（query 的决策人 = fact 的主体）
		}
		// attr 对齐
		if qAttr != "" && slotValueOverlap(qAttr, slotGet(fSlots, "attr")) {
			score += 0.2
		}
		// value ↔ anchor/decider 对齐（跨槽值匹配）
		fVal := slotGet(fSlots, "value")
		if fVal != "" {
			if slotValueOverlap(qAnchor, fVal) {
				score += 0.3
			}
			if slotValueOverlap(qDecider, fVal) {
				score += 0.2 // 决策人同时出现在值段
			}
		}
		// R7-A 修复（ref 桶 miss 根因）：query 若为「问属性值」形态（如「客户B的决策人是什么」），
		// 且候选 fact 的 value 段是**主体词**（如「决策人031」），则该 fact 更像 hop1 中间记忆
		// （把主体词引用当成值），而非最终事实值（「周二」）。这类 fact 与正确 fact 在
		// entity/attr 上同分（平局由 ID 字典序决定），会把 hop1 顶到 top1。
		// 抑制：扣掉 value 段的 0.3 分，使正确 fact 稳定排前。
		askAttr := isAskAttrQuery(query)
		for _, c := range cands {
			if c.Memory == nil {
				continue
			}
			fSlots := parseFactSlots(c.Memory.Fact)
			fVal := slotGet(fSlots, "value")
			if askAttr && fVal != "" && subjectValueRe.MatchString(strings.TrimSpace(fVal)) {
				score -= 0.3
			}
			if score > 0 {
				if score > 1 {
					score = 1
				}
				out[c.Memory.MemoryID] = score
			}
		}
	}
	return out
}

// ------------------------------------------------------------------
// 路径 2：token 共现加权确定性相似度
// ------------------------------------------------------------------

// semanticPathTokenCo 路径 2：query 与候选的 token 双向共现 + IDF 加权。
//
// 与路径 0（向量）互补：向量对短 query 冷启动弱（未出现在 IDF 语料的 token
// 无向量）；本路径用候选集内的 DF 动态 IDF，对短 query 与长 fact 的双向对齐
// 更敏感。与 signalKeywordBM25 的区别：BM25 是**单向**词频（query→doc），
// 本路径做**双向**归一（query 在 fact 中的覆盖率 × fact 在 query 中的覆盖率），
// 语义对齐语义更贴近「共同话题度」。
//
// 公式（确定性）：
//   - idf(t) = ln(1 + N/(df(t)+1))，N=候选数，df=候选集内含 token t 的文档数；
//   - Q = {t ∈ tokenize(query)}，F = {t ∈ tokenize(fact)}；
//   - score = Σ_{t ∈ Q∩F} idf(t)² / Σ_{t ∈ Q} idf(t)²  （query→fact 覆盖率，主项）
//     × Σ_{t ∈ F∩Q} idf(t)² / Σ_{t ∈ F} idf(t)²  （fact→query 覆盖率，次项）
//
// 最终分 = 0.6 × 主项 + 0.4 × 次项（强调 query 侧命中，同时惩罚过长 fact）。
func semanticPathTokenCo(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		return out
	}
	// 候选集内 DF（确定性：按 cands 固定顺序累加）
	df := map[string]int{}
	tokenSets := map[string]map[string]bool{}
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		ts := tokenSet(c.Memory.Fact)
		tokenSets[c.Memory.MemoryID] = ts
		for t := range ts {
			df[t]++
		}
	}
	n := float64(len(tokenSets))
	if n == 0 {
		return out
	}
	// query 侧 IDF 权重（固定）
	qWeights := map[string]float64{}
	var qDenom float64
	for _, t := range qTokens {
		w := math.Log(1 + n/(float64(df[t])+1))
		qWeights[t] = w
		qDenom += w * w
	}
	if qDenom == 0 {
		return out
	}
	// R7-A 修复（ref 桶 miss 根因）：query 若为「问属性值」形态（如「客户B的决策人是什么」），
	// 且候选 fact 的 value 段是**主体词**（如「决策人031」），则该 fact 更像 hop1 中间记忆，
	// 而非最终事实值（「周二」）。对这类 fact 施加 penalty，抑制 path2 把 hop1 顶到 top1。
	askAttr := isAskAttrQuery(query)
	subjectVal := map[string]bool{} // memory_id -> value 段是主体词
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		fSlots := parseFactSlots(c.Memory.Fact)
		val := slotGet(fSlots, "value")
		if val != "" && subjectValueRe.MatchString(strings.TrimSpace(val)) {
			subjectVal[c.Memory.MemoryID] = true
		}
	}
	for id, fSet := range tokenSets {
		// 主项：query→fact 覆盖率
		var main float64
		for t, w := range qWeights {
			if fSet[t] {
				main += w * w
			}
		}
		main /= qDenom
		// 次项：fact→query 覆盖率
		var sub float64
		qSet := make(map[string]bool, len(qTokens))
		for _, t := range qTokens {
			qSet[t] = true
		}
		var fDenom float64
		for t := range fSet {
			w, ok := qWeights[t]
			if !ok {
				w = math.Log(1 + n/(float64(df[t])+1))
			}
			fDenom += w * w
			if qSet[t] {
				sub += w * w
			}
		}
		if fDenom == 0 {
			continue
		}
		sub /= fDenom
		score := 0.6*main + 0.4*sub
		// 问属性值 + value 是主体词 → 直接归 0（该 fact 是 hop1 中间记忆，不是答案）
		if askAttr && subjectVal[id] {
			score = 0
		}
		if score > 0 {
			out[id] = score
		}
	}
	return out
}

// askAttrQueryRe 匹配「问属性值」形态的 query（如「X的决策人是什么」「X偏好什么」）。
var askAttrQueryRe = regexp.MustCompile(`的(.+?)(是什么|是什么样|偏好什么|预算多少|计划什么|是啥|啥)$`)

func isAskAttrQuery(query string) bool {
	return askAttrQueryRe.MatchString(strings.TrimSpace(query))
}

// subjectValueRe 匹配主体词（客户X/决策人X/项目X/场景X），作为「hop1 中间记忆」的判据。
var subjectValueRe = regexp.MustCompile(`^(客户|决策人|项目|场景)[0-9A-Za-z]+$`)

// ------------------------------------------------------------------
// 多路径集成：ConsensusVoter 投票
// ------------------------------------------------------------------

// semanticMultiScore R7-A 语义多路径集成：各路径独立打分 → top-K 解释 →
// ConsensusVoter 倒数秩投票 → 共识分归一化。
//
// 平滑策略：未消融路径 0 时，用向量底座（原语义分）做保底，避免投票稀疏化
// 导致 RRF 路径下语义信号对非 top-K 候选失去区分度；共识分叠加在底座上。
// 消融路径 0 时（底座不可用），直接用共识分（测「结构路径能否替代向量」）。
func (rf *RankFusion) semanticMultiScore(ctx context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	ablate := rf.SemanticAblatePath
	pathScores := map[string]map[string]float64{}
	for _, name := range semanticPathOrder {
		if ablate != "" && name == ablate {
			continue
		}
		pathScores[name] = rf.semanticPathScore(ctx, name, query, cands)
	}
	if len(pathScores) == 0 {
		return map[string]float64{}
	}
	// 各路径 top-K 解释
	interps := make([]Interpretation, 0, len(pathScores))
	k := rf.VoteTopK
	if k <= 0 {
		k = SemanticMultiPathTopK
	}
	for name, sc := range pathScores {
		top := topIDsByScore(sc, k)
		if len(top) == 0 {
			continue
		}
		interps = append(interps, Interpretation{Source: name, TopIDs: top, Score: sc, Weight: 1.0})
	}
	if len(interps) == 0 {
		return map[string]float64{}
	}
	voter := &ConsensusVoter{K: k}
	res := voter.Vote(interps)
	if len(res.Scores) == 0 {
		return map[string]float64{}
	}
	// 平滑：底座（原向量分）保底
	out := map[string]float64{}
	if base, ok := pathScores[SemanticPathVector]; ok {
		for id, b := range base {
			w, has := res.Scores[id]
			if has {
				out[id] = 0.5*b + 0.5*w
			} else {
				out[id] = 0.5 * b
			}
		}
		// 未在底座里但进了共识序的候选（结构路径发现的新候选）
		for id, w := range res.Scores {
			if _, exists := out[id]; !exists {
				out[id] = w
			}
		}
	} else {
		// 消融路径 0：直接用共识分
		out = res.Scores
	}
	return normalizeMax(out)
}

// topIDsByScore 取打分 map 中 top-K 的候选 ID（确定性：降序，平局按 ID 字典序）。
func topIDsByScore(sc map[string]float64, k int) []string {
	type kv struct {
		id string
		v  float64
	}
	arr := make([]kv, 0, len(sc))
	for id, v := range sc {
		if v <= 0 {
			continue
		}
		arr = append(arr, kv{id, v})
	}
	// 确定性排序：分降序，平局 ID 升序
	for i := 0; i < len(arr); i++ {
		for j := i + 1; j < len(arr); j++ {
			if arr[j].v > arr[i].v || (arr[j].v == arr[i].v && arr[j].id < arr[i].id) {
				arr[i], arr[j] = arr[j], arr[i]
			}
		}
	}
	if k <= 0 || k > len(arr) {
		k = len(arr)
	}
	out := make([]string, 0, k)
	for i := 0; i < k; i++ {
		out = append(out, arr[i].id)
	}
	return out
}

// SemanticPathNames 返回全部语义路径名（供评测器/flag 枚举）。
func SemanticPathNames() []string {
	out := make([]string, len(semanticPathOrder))
	copy(out, semanticPathOrder)
	return out
}
