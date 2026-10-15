// R13-B 路：记忆位移边（Memory Displacement Edges，建议 1）。
//
// 真源：lightmem.md §3.2.1；改造依据 lightmem/monty.md §4.2 建议 1。
//
// 背景（Monty graph memory）：节点 = pose + features，**边 = displacement**
// （从 A 到 B 需要的关系/动作）。lightmem §3.2 原本是一组扁平参考系维度，
// 记忆之间没有关系，多跳召回只能靠向量相似度近似。本文件在**不改动既有
// Schema 与默认检索路径**的前提下，新增一张扩展边表，支撑多跳召回与关系推理。
//
// 红线遵守：
//   - 只增不改：本文件为全新文件，不修改任何既有字段语义与默认 Rank 融合路径；
//   - 边只用于软排序与多跳召回（见 multihop_monty.go），**不跨越 §3.2 硬过滤边界**；
//   - 只对 L2 长期记忆建边（BuildEdgesHeuristic 内过滤 L1）；
//   - 写入幂等：(from_memory_id, to_memory_id, relation) 三元组唯一，重复写入不新增行；
//   - 淘汰为软删除（置 invalidated_at），不物理删除，保留审计痕迹；
//   - 不调 LLM，固定规则，确定性。
package kernel

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// EdgeRelation 位移边关系类型（来源 monty.md §4.2 建议 1 四类枚举）。
type EdgeRelation string

const (
	// RelTemporal 两条记忆 created_at 间隔 ≤ 7 天且属同一 task/session。初始权重 0.3。
	RelTemporal EdgeRelation = "temporal"
	// RelTaskFlow 两条记忆挂同一 task_id，按时间先后。初始权重 0.4。
	RelTaskFlow EdgeRelation = "task-flow"
	// RelCausal 同 user 下，后条 fact 中出现前条 entity 的指代。初始权重 0.5。
	RelCausal EdgeRelation = "causal"
	// RelEntityAlias 两条记忆 entity 归一化后相同。初始权重 0.8。
	RelEntityAlias EdgeRelation = "entity-alias"
)

// 各类边的初始权重（任务书 B 路正文给定）。
const (
	EdgeWeightTemporal    = 0.3
	EdgeWeightTaskFlow    = 0.4
	EdgeWeightCausal      = 0.5
	EdgeWeightEntityAlias = 0.8
)

// 默认容量治理参数。
const (
	// DefaultMaxOutEdges 单条记忆的有效出边上；超限软删 weight 最低且最旧的出边。
	DefaultMaxOutEdges = 16
	// temporalWindow 时间邻接窗。
	temporalWindow = 7 * 24 * time.Hour
)

// DisplacementEdge 一条记忆位移边（§3.2.1 单一真源）。
//
// 方向：from → to（从 from 沿该关系可预测/跳转至 to）。
// 只增：CreatedAt 只写一次；历史边永不物理删除，失效通过 InvalidatedAt 标记。
type DisplacementEdge struct {
	FromMemoryID string       `json:"from_memory_id"`
	ToMemoryID   string       `json:"to_memory_id"`
	Relation     EdgeRelation `json:"relation"`
	Weight       float64      `json:"weight"`
	CreatedAt    time.Time    `json:"created_at"`
	// InvalidatedAt 非空表示该边已被软删除（容量淘汰/显式失效）；不参与召回，但保留可审计。
	InvalidatedAt *time.Time `json:"invalidated_at,omitempty"`
}

// Alive 边当前是否有效（未被软删）。
func (e DisplacementEdge) Alive() bool { return e.InvalidatedAt == nil }

// edgeTriple 幂等键：(from, to, relation) 三元组。
func edgeTriple(from, to string, rel EdgeRelation) string {
	return from + "\x00" + to + "\x00" + string(rel)
}

// EdgeStore 位移边的内存存储（R13 最小可用集：进程内、幂等、可软删）。
//
// 并发安全。生产持久化（SQLite/PG 边表）留待后续轮次；本期接口已按
// 「幂等写入 + 软删 + 出边查询」收敛，便于后续替换为 SQL 实现而不改调用方。
type EdgeStore struct {
	mu     sync.Mutex
	edges  map[string]DisplacementEdge // triple -> edge（含已软删的行，审计保留）
	outIdx map[string][]string         // from -> 该起点的全部 triple（含已软删）
	maxOut int
	clock  func() time.Time
}

// NewEdgeStore 构造空边表。maxOut<=0 取 DefaultMaxOutEdges；clock 传 nil 取 time.Now。
func NewEdgeStore(maxOut int, clock func() time.Time) *EdgeStore {
	if maxOut <= 0 {
		maxOut = DefaultMaxOutEdges
	}
	if clock == nil {
		clock = time.Now
	}
	return &EdgeStore{
		edges:  make(map[string]DisplacementEdge),
		outIdx: make(map[string][]string),
		maxOut: maxOut,
		clock:  clock,
	}
}

// AddEdge 幂等写入一条边。
//   - 三元组已存在（无论是否已软删）：**不新增行、不更新权重**，返回已存在的边与 false；
//   - 新边：写入后执行出边容量治理——该起点有效出边超 maxOut 时，
//     软删 weight 最低、权重相同则 CreatedAt 最旧的边（不物理删除）。
//
// 新边的 CreatedAt 由 store 的 clock 决定。
func (s *EdgeStore) AddEdge(from, to string, rel EdgeRelation, weight float64) (DisplacementEdge, bool) {
	if from == "" || to == "" || from == to {
		return DisplacementEdge{}, false
	}
	key := edgeTriple(from, to, rel)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.edges[key]; ok {
		return existing, false
	}
	now := s.clock().UTC()
	e := DisplacementEdge{
		FromMemoryID: from,
		ToMemoryID:   to,
		Relation:     rel,
		Weight:       clampWeight(weight),
		CreatedAt:    now,
	}
	s.edges[key] = e
	s.outIdx[from] = append(s.outIdx[from], key)
	s.enforceOutgoingLocked(from)
	return e, true
}

// InvalidateEdge 软删一条边（置 invalidated_at）。不存在或已软删时返回 false。
// 不物理删除——行保留在表中可审计。
func (s *EdgeStore) InvalidateEdge(from, to string, rel EdgeRelation) bool {
	key := edgeTriple(from, to, rel)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.edges[key]
	if !ok || !e.Alive() {
		return false
	}
	now := s.clock().UTC()
	e.InvalidatedAt = &now
	s.edges[key] = e
	return true
}

// OutEdges 返回 from 的全部有效出边（未软删），按 CreatedAt 升序、再按 triple 字典序
// 全序排序，保证任何环境下顺序确定。
func (s *EdgeStore) OutEdges(from string) []DisplacementEdge {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.outIdx[from]
	out := make([]DisplacementEdge, 0, len(keys))
	for _, k := range keys {
		if e := s.edges[k]; e.Alive() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return edgeTriple(out[i].FromMemoryID, out[i].ToMemoryID, out[i].Relation) <
			edgeTriple(out[j].FromMemoryID, out[j].ToMemoryID, out[j].Relation)
	})
	return out
}

// EdgeCount 边表总行数（含已软删，审计口径）。
func (s *EdgeStore) EdgeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.edges)
}

// AliveEdgeCount 当前有效（未软删）边数。
func (s *EdgeStore) AliveEdgeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.edges {
		if e.Alive() {
			n++
		}
	}
	return n
}

// enforceOutgoingLocked 出边容量治理：from 的有效出边超上限时，
// 软删 weight 最低、权重相同则 CreatedAt 最旧者。调用方必须已持锁。
func (s *EdgeStore) enforceOutgoingLocked(from string) {
	keys := s.outIdx[from]
	alive := make([]string, 0, len(keys))
	for _, k := range keys {
		if s.edges[k].Alive() {
			alive = append(alive, k)
		}
	}
	for len(alive) > s.maxOut {
		// 选受害者：weight 最小；并列取 CreatedAt 最旧。
		victim := 0
		for i := 1; i < len(alive); i++ {
			ei, ev := s.edges[alive[i]], s.edges[alive[victim]]
			if ei.Weight < ev.Weight ||
				(ei.Weight == ev.Weight && ei.CreatedAt.Before(ev.CreatedAt)) {
				victim = i
			}
		}
		vk := alive[victim]
		now := s.clock().UTC()
		e := s.edges[vk]
		e.InvalidatedAt = &now
		s.edges[vk] = e
		// 从 alive 中移除（保持顺序）
		alive = append(alive[:victim], alive[victim+1:]...)
	}
}

// clampWeight 把权重收敛到 [0,1]。
func clampWeight(w float64) float64 {
	if w < 0 {
		return 0
	}
	if w > 1 {
		return 1
	}
	return w
}

// normalizeEntity 实体归一化：小写 + 去标点/空白。空串归一化后仍为空。
func normalizeEntity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// causalPronouns 指代词集合（causal 边判定用，简单字符串匹配，不调 LLM）。
var causalPronouns = []string{"它", "该", "这个", "那个", "it", "its", "this", "that"}

// factMentionsPronoun 后条 fact 归一化小写后是否出现指代词。
func factMentionsPronoun(fact string) bool {
	f := strings.ToLower(fact)
	for _, p := range causalPronouns {
		if strings.Contains(f, p) {
			return true
		}
	}
	return false
}

// BuildEdgesHeuristic 离线启发式建边（R13 最小可用集，纯规则、不调 LLM）。
//
// 只对 Layer==L2 的记忆建边（L1 默认不建边）。同 tenant+user 分组后组内两两配对，
// 按四类规则产出候选边，逐条送入 store（幂等 + 容量治理由 store 负责）。
//
// 配对方向：按 RecordedAt 升序，先者为 from、后者为 to（时间单向，避免双向重复）。
// RecordedAt 为零值（未落库时间）的记忆不参与 temporal/task-flow 时间类规则。
//
// 返回实际新增（store 此前不存在）的边数。
func BuildEdgesHeuristic(mems []Memory, store *EdgeStore) int {
	if store == nil {
		return 0
	}
	// 只保留 L2 长期记忆。
	l2 := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if m.Layer == LayerL2 {
			l2 = append(l2, m)
		}
	}
	// 按 tenant+user 分组。
	groups := make(map[string][]Memory)
	for _, m := range l2 {
		key := m.TenantID + "\x00" + m.UserID
		groups[key] = append(groups[key], m)
	}
	added := 0
	for _, g := range groups {
		sortMemoriesByRecordedAt(g)
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				a, b := g[i], g[j] // a 早于 b
				added += pairEdges(a, b, store)
			}
		}
	}
	return added
}

// pairEdges 对一对（已按时间排序 a 早 b 晚）L2 记忆尝试四类建边。
func pairEdges(a, b Memory, store *EdgeStore) int {
	added := 0
	// entity-alias：归一化实体相同且非空（空实体不连边，避免全表两两爆炸）。
	ea, eb := normalizeEntity(a.ReferenceFrame.Entity), normalizeEntity(b.ReferenceFrame.Entity)
	if ea != "" && ea == eb {
		if _, isNew := store.AddEdge(a.MemoryID, b.MemoryID, RelEntityAlias, EdgeWeightEntityAlias); isNew {
			added++
		}
	}
	// task-flow：同一非空 task，按时间先后 a->b。
	if t := a.ReferenceFrame.Task; t != "" && t == b.ReferenceFrame.Task {
		if _, isNew := store.AddEdge(a.MemoryID, b.MemoryID, RelTaskFlow, EdgeWeightTaskFlow); isNew {
			added++
		}
	}
	// temporal：两条 RecordedAt 均非零、间隔 ≤7 天，且同 task 或同 scene（session 代理）。
	if !a.RecordedAt.IsZero() && !b.RecordedAt.IsZero() {
		dt := b.RecordedAt.Sub(a.RecordedAt)
		if dt < 0 {
			dt = -dt
		}
		sameTask := a.ReferenceFrame.Task != "" && a.ReferenceFrame.Task == b.ReferenceFrame.Task
		sameScene := a.ReferenceFrame.Scene != "" && a.ReferenceFrame.Scene == b.ReferenceFrame.Scene
		if dt <= temporalWindow && (sameTask || sameScene) {
			if _, isNew := store.AddEdge(a.MemoryID, b.MemoryID, RelTemporal, EdgeWeightTemporal); isNew {
				added++
			}
		}
	}
	// causal：同 user 下（pairEdges 已在同 user 分组内），前条 entity 非空，
	// 后条 fact 中出现指代词（它/该/这个/it...）。
	if ea != "" && factMentionsPronoun(b.Fact) {
		if _, isNew := store.AddEdge(a.MemoryID, b.MemoryID, RelCausal, EdgeWeightCausal); isNew {
			added++
		}
	}
	return added
}

// sortMemoriesByRecordedAt 稳定排序：RecordedAt 早者在前；零值排最后；全序含 ID。
func sortMemoriesByRecordedAt(ms []Memory) {
	sort.SliceStable(ms, func(i, j int) bool {
		ai, bi := ms[i].RecordedAt, ms[j].RecordedAt
		if ai.IsZero() && bi.IsZero() {
			return ms[i].MemoryID < ms[j].MemoryID
		}
		if ai.IsZero() {
			return false
		}
		if bi.IsZero() {
			return true
		}
		if !ai.Equal(bi) {
			return ai.Before(bi)
		}
		return ms[i].MemoryID < ms[j].MemoryID
	})
}

// SummarizeEdges 输出边表统计（供评测/监控使用，确定性）。
type EdgeSummary struct {
	TotalRows       int `json:"total_rows"`       // 含已软删（审计口径）
	AliveRows       int `json:"alive_rows"`       // 有效边
	InvalidatedRows int `json:"invalidated_rows"` // 已软删
}

// Summarize 返回边表统计。
func (s *EdgeStore) Summarize() EdgeSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := EdgeSummary{TotalRows: len(s.edges)}
	for _, e := range s.edges {
		if e.Alive() {
			sum.AliveRows++
		} else {
			sum.InvalidatedRows++
		}
	}
	return sum
}
