// R24-A：L2 静态记忆确定性查找表 + 快车道（Engram 架构映射）
// 真源：lightmem.md 任务书 R24 §4「A 线」、§9「接口冻结」。
//
// 目标：给 L2 的"高频、确定性、结构化"记忆开一条 O(1) 查找直达通道；
// 查不到或查歧义就退回现有 8 信号融合，保证"分流"而不是"替换"。
//
// 设计要点（严格对齐任务书 §4.1）：
//   - 索引是派生视图，不是第二事实源：只存 memory_id + 排序键，不存 Fact 正文。
//   - 键：hash(normalize(entity) + "|" + normalize(attribute))；
//     normalize 复用 kernel/entity_align.go 的 normalizeAlignKey 口径（小写、去标点空白符号）。
//   - 实体/属性提取：entity 取 ReferenceFrame.Entity，空时退化为 Fact 中首个实体候选
//     （slotEntityRe 命中）；attribute 取 ReferenceFrame.Category，空时用 slotAttrPrefRe
//     （偏好/准备/预算/决策人）命中词。
//   - 值：[]string（memory_id 列表），按 (L2 > L1, IntrinsicValue 降序, memory_id 升序)
//     确定性排序，禁止 map 迭代序。
//   - 二级索引：entity 3-gram 前缀桶（参考实现，低成本）；命中多桶即视为歧义。
//
// 只增不改：本文件是 R24-A 新增文件，不修改任何既有 Schema 字段语义。
package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// LookupStats 查找表统计（§9 接口冻结）。
type LookupStats struct {
	Keys          int64 `json:"keys"`            // 主索引键数
	Entries       int64 `json:"entries"`         // 索引条目总数（memory_id 数）
	Ambiguous     int64 `json:"ambiguous"`       // 键下条目 ≥2（歧义键）的键数
	RebuiltAtUnix int64 `json:"rebuilt_at_unix"` // 最近一次 Rebuild 完成时刻（unix 秒）；未重建为 0
}

// lookupEntry 索引条目：只存排序键与标识（红线：不存 Fact 正文）。
type lookupEntry struct {
	memoryID string
	layer    MemoryLayer
	value    float64 // IntrinsicValue（§5.3 固有价值分）
}

// LookupIndex L2 静态记忆确定性查找表（§4.1）。
// 主索引 keys: hashKey -> []lookupEntry（确定性有序）；二级索引 grams: 3-gram -> set(hashKey)。
type LookupIndex struct {
	mu    sync.RWMutex
	keys  map[string][]lookupEntry
	grams map[string]map[string]struct{}
	stats LookupStats
	built bool
}

// lookupScanMax Rebuild 扫描上限：ScanLayer 的 max 参数（≤0 时后端默认 1000，
// 这里显式给大值保证"可重建恢复全部条目"判据不受截断影响）。
const lookupScanMax = 1 << 20

// NewLookupIndex 构造空的查找表（§9 接口冻结）。
func NewLookupIndex() *LookupIndex {
	return &LookupIndex{
		keys:  make(map[string][]lookupEntry),
		grams: make(map[string]map[string]struct{}),
	}
}

// ------------------------------------------------------------------
// 键构造与实体/属性提取（§4.1 口径）
// ------------------------------------------------------------------

// lookupKey 键构造：hash(normalize(entity) + "|" + normalize(attribute))。
// normalize 复用 normalizeAlignKey（entity_align.go:294），不新写一套归一化。
func lookupKey(entity, attribute string) string {
	raw := normalizeAlignKey(entity) + "|" + normalizeAlignKey(attribute)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// entityOf 提取记忆的实体键：ReferenceFrame.Entity 优先，空时退化为
// Fact 中首个实体候选（slotEntityRe 命中词，与 semantic_multi.go 同口径）。
func (x *LookupIndex) entityOf(m *Memory) string {
	if e := strings.TrimSpace(m.ReferenceFrame.Entity); e != "" {
		return e
	}
	return slotEntityRe.FindString(m.Fact)
}

// attrOf 提取记忆的属性键：ReferenceFrame.Category 优先，空时用
// slotAttrPrefRe（偏好/准备/预算/决策人）命中词。
func (x *LookupIndex) attrOf(m *Memory) string {
	if c := strings.TrimSpace(m.ReferenceFrame.Category); c != "" {
		return c
	}
	return slotAttrPrefRe.FindString(m.Fact)
}

// entityFromText 查询文本中的实体：与索引侧同一提取口径。
func entityFromText(text string) string { return slotEntityRe.FindString(text) }

// attrFromText 查询文本中的属性：与索引侧同一提取口径。
func attrFromText(text string) string { return slotAttrPrefRe.FindString(text) }

// layerPriority 分层优先级（§4.1 值排序：L2 > L1）。
func layerPriority(l MemoryLayer) int {
	switch l {
	case LayerL2:
		return 2
	case LayerL1:
		return 1
	default:
		return 0
	}
}

// entryLess 确定性排序：layer 优先级 L2 > L1，其次 IntrinsicValue 降序，最后 memory_id 升序。
func entryLess(a, b lookupEntry) bool {
	ra, rb := layerPriority(a.layer), layerPriority(b.layer)
	if ra != rb {
		return ra > rb
	}
	if a.value != b.value {
		return a.value > b.value
	}
	return a.memoryID < b.memoryID
}

// trigrams 返回字符串的全部 3-gram 前缀桶键；长度 <3 时整串作一个桶（支持短实体近似）。
func trigrams(s string) []string {
	rs := []rune(s)
	if len(rs) == 0 {
		return nil
	}
	if len(rs) < 3 {
		return []string{string(rs)}
	}
	out := make([]string, 0, len(rs)-2)
	for i := 0; i+3 <= len(rs); i++ {
		out = append(out, string(rs[i:i+3]))
	}
	return out
}

// ------------------------------------------------------------------
// 主索引维护（§4.1 / §9 接口冻结）
// ------------------------------------------------------------------

// Rebuild 从 LayerScanner 只读重建索引（L2 + L1 两层入索引），tenant 过滤。
// 只读 LayerScanner，不写后端（红线：索引不持久化）。
func (x *LookupIndex) Rebuild(ctx context.Context, sc LayerScanner, tenant string) (LookupStats, error) {
	x.mu.Lock()
	x.keys = make(map[string][]lookupEntry)
	x.grams = make(map[string]map[string]struct{})
	x.stats = LookupStats{}
	x.built = false
	x.mu.Unlock()

	for _, layer := range []MemoryLayer{LayerL2, LayerL1} {
		ms, err := sc.ScanLayer(ctx, layer, tenant, lookupScanMax)
		if err != nil {
			return x.Stats(), fmt.Errorf("lookup rebuild scan %s: %w", layer, err)
		}
		for _, m := range ms {
			x.Add(m)
		}
	}

	x.mu.Lock()
	x.built = true
	x.stats.RebuiltAtUnix = time.Now().Unix()
	x.mu.Unlock()
	return x.Stats(), nil
}

// Add 插入/更新一条记忆（同 memory_id 覆盖；无实体的记忆无法建立查找键，跳过）。
// 只接受 L2/L1 层记忆（L0 工作记忆与 L3 归档不入确定性查找表）。
func (x *LookupIndex) Add(m *Memory) {
	if m == nil || m.MemoryID == "" {
		return
	}
	if m.Layer != LayerL2 && m.Layer != LayerL1 {
		return
	}
	entity, attr := x.entityOf(m), x.attrOf(m)
	if entity == "" {
		return
	}
	key := lookupKey(entity, attr)
	entry := lookupEntry{
		memoryID: m.MemoryID,
		layer:    m.Layer,
		value:    IntrinsicValue(m, DefaultPromotionPolicy()),
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	entries := x.keys[key]
	found := false
	for i, e := range entries {
		if e.memoryID == m.MemoryID {
			entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entryLess(entries[i], entries[j]) })
	x.keys[key] = entries

	// 二级索引：entity 3-gram 前缀桶。
	if n := normalizeAlignKey(entity); len(n) > 0 {
		for _, g := range trigrams(n) {
			if x.grams[g] == nil {
				x.grams[g] = make(map[string]struct{})
			}
			x.grams[g][key] = struct{}{}
		}
	}
	x.built = true
}

// Remove 从索引移除一条记忆（所有键下该 memory_id 均删除；空键一并清除）。
// 二级索引桶保留（指向已删键的候选在 Lookup 时以主索引为准被过滤）。
func (x *LookupIndex) Remove(memoryID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for key, entries := range x.keys {
		idx := -1
		for i, e := range entries {
			if e.memoryID == memoryID {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		entries = append(entries[:idx], entries[idx+1:]...)
		if len(entries) == 0 {
			delete(x.keys, key)
		} else {
			x.keys[key] = entries
		}
	}
}

// Lookup 确定性查找：返回 (memory_id 列表, 是否精确命中)。
//   - 精确键命中：返回该键下（确定性排序后的）全部 id，exact=true。
//   - 精确未命中但 3-gram 近似唯一候选：返回候选 id，exact=false（可按近似慢车道兜底）。
//   - 近似多桶（歧义）、无候选、索引空：返回 nil，exact=false。
func (x *LookupIndex) Lookup(text string) ([]string, bool) {
	entity, attr := entityFromText(text), attrFromText(text)

	x.mu.RLock()
	defer x.mu.RUnlock()
	if !x.built || len(x.keys) == 0 {
		return nil, false
	}

	if entity != "" {
		if es, ok := x.keys[lookupKey(entity, attr)]; ok && len(es) > 0 {
			ids := make([]string, len(es))
			for i, e := range es {
				ids[i] = e.memoryID
			}
			return ids, true
		}
	}

	// 3-gram 近似：实体写法略有差异时找候选键。
	if n := normalizeAlignKey(entity); len(n) > 0 {
		cand := make(map[string]struct{})
		for _, g := range trigrams(n) {
			for k := range x.grams[g] {
				cand[k] = struct{}{}
			}
		}
		if len(cand) == 1 {
			for k := range cand {
				if es, ok := x.keys[k]; ok && len(es) > 0 {
					ids := make([]string, len(es))
					for i, e := range es {
						ids[i] = e.memoryID
					}
					return ids, false
				}
			}
		}
		// len(cand) > 1：命中多桶即歧义 → 返回 nil（调用方回退慢车道）。
	}
	return nil, false
}

// Stats 返回查找表统计（§9 接口冻结）。Keys/Entries/Ambiguous 实时统计，保证确定性一致。
func (x *LookupIndex) Stats() LookupStats {
	x.mu.RLock()
	defer x.mu.RUnlock()
	st := x.stats
	st.Keys = int64(len(x.keys))
	var entries, amb int64
	for _, es := range x.keys {
		entries += int64(len(es))
		if len(es) >= 2 {
			amb++
		}
	}
	st.Entries = entries
	st.Ambiguous = amb
	return st
}

// Built 返回索引是否已构建（Rebuild 完成）或非空（Add 曾写入）。
func (x *LookupIndex) Built() bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.built
}

// ------------------------------------------------------------------
// APIServer 注入与接线（§4.1：setter 放 lookup.go，不改 router.go）
// ------------------------------------------------------------------

// SetLookupIndex 注入 L2 确定性查找表（R24-A）。索引由调用方负责 Rebuild 或 Add 填充。
func (s *APIServer) SetLookupIndex(x *LookupIndex) {
	s.lookup = x
}

// SetLookupEnabled 开关查找表快车道。默认 false：关闭时 RecallAndFuse 逐位零回归。
func (s *APIServer) SetLookupEnabled(b bool) {
	s.lookupEnabled = b
}

// lookupIndex 返回注入的 *LookupIndex；未注入（nil 或类型不符）返回 nil。
func (s *APIServer) lookupIndex() *LookupIndex {
	if li, ok := s.lookup.(*LookupIndex); ok {
		return li
	}
	return nil
}

// NoteRecall 命中统计接缝（§9：A 线先落空实现，C 线填实现；access 为 nil 时 no-op）。
// T0 预埋的 access 为 interface{} 占位，这里用最小接口断言透传，避免无法编译。
func (s *APIServer) NoteRecall(ids []string) {
	if s.access == nil {
		return
	}
	if at, ok := s.access.(interface{ NoteRecall([]string) }); ok {
		at.NoteRecall(ids)
	}
}

// fusedIDsOf 从 Fused 结果收集去重的 memory_id 列表（NoteRecall 的 hit 来源）。
// 注意：composition_test.go 已有同名测试辅助 fusedIDs，这里命名 avoid 符号冲突。
func fusedIDsOf(fused []FusedResult) []string {
	seen := make(map[string]struct{}, len(fused))
	var ids []string
	for _, f := range fused {
		if f.MemoryID == "" {
			continue
		}
		if _, ok := seen[f.MemoryID]; ok {
			continue
		}
		seen[f.MemoryID] = struct{}{}
		ids = append(ids, f.MemoryID)
	}
	return ids
}

// tryLookupFastPath 尝试 L2 查找表快车道。
// 返回 (RouterResult, 是否命中, 回退原因)。命中时 res 已组装完成（不跑 8 信号融合）；
// 未命中时 res 为零值，fallback 原因为 "no_index" | "ambiguous" | "miss"。
func (s *APIServer) tryLookupFastPath(ctx context.Context, rc RouterContext) (RouterResult, bool, string) {
	li := s.lookupIndex()
	if li == nil || !li.Built() {
		return RouterResult{}, false, "no_index"
	}
	// 历史回溯（as_of）不属于确定性快车道：当前版本快照无法表示历史时刻，回退慢车道。
	if rc.AsOf != nil {
		return RouterResult{}, false, "miss"
	}
	ids, exact := li.Lookup(rc.Text)
	if len(ids) == 0 {
		return RouterResult{}, false, "miss"
	}
	if len(ids) >= 2 {
		return RouterResult{}, false, "ambiguous"
	}
	if !exact {
		return RouterResult{}, false, "miss"
	}

	// 唯一精确命中：取全文组装 RouterResult（不跑融合）。
	// Get 已做 visibility 校验；此处再叠加 canView 与当前生效版本硬过滤，
	// 与 RecallLayers 口径一致（越权或过期版本不允许经快车道返回）。
	m, err := s.backend.Get(ctx, ids[0], rc.UserID, rc.TenantID)
	if err != nil || m == nil || !m.canView(rc.UserID, rc.TenantID) {
		return RouterResult{}, false, "miss"
	}
	if m.SupersededBy != nil {
		return RouterResult{}, false, "miss"
	}

	res := RouterResult{
		Intent:       ClassifyIntent(rc.Text),
		Recalled:     nil, // 快车道不走多路召回
		FusionScores: nil, // 快车道不做融合/重排（红线）
		Fused: []FusedResult{{
			MemoryID:   m.MemoryID,
			Fact:       m.Fact,
			Layer:      m.Layer,
			Score:      1.0, // 确定性命中：置信满分，不参与融合加权
			Source:     m.Source,
			Confidence: m.Confidence,
			Memory:     m,
		}},
		Mode: "unique",
		Trace: &RetrievalTrace{
			Query:     rc.Text,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Ranked: []TraceRankedCandidate{{
				MemoryID: m.MemoryID,
				Fact:     m.Fact,
				Score:    1.0,
				Rank:     1,
				Layer:    string(m.Layer),
			}},
			Mode:          "unique",
			TotalCands:    1,
			FilteredCands: 1,
			FastPath:      true,
		},
	}
	return res, true, ""
}
