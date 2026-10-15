// R14-B 路：显式 L0→L1 落库边界（Flush 决策）。
//
// 真源：lightmem.md §3.5.1；依据 lightmem/monty.md §4.5 建议 4。
//
// 背景（Monty STM / LTM 双缓冲）：Monty 的短期记忆（STM）只在 episode 结束时
// 一次性 flush 进长期记忆（LTM）。lightmem 现状是「持续捕获 → 压缩 → 整合」，
// L0（短期）与 L1（长期）之间没有显式边界，L1 被 L0 的持续写入稀释。本文件
// 在**不改动既有 Schema 与写入路径**的前提下，产出**纯决策计划**：由调用方
// 决定是否执行实际的 L0→L1 落库，本文件不执行任何写入。
//
// 红线遵守：
//   - 只增不改：本文件为全新文件，不修改任何既有字段语义与默认检索/写入路径；
//   - 纯决策：DecideFlush 只产出 FlushPlan，不调 StorageBackend、无 I/O、
//     无随机、无时间依赖（时间由调用方注入 sig.Now）；
//   - 幂等：同一输入多次调用返回结构完全一致（reflect.DeepEqual 可断言）；
//   - 确定性：无并发依赖、无共享可变状态；
//   - 私有函数一律 flush 前缀（flushTokens / flushTokenSet / flushNormalize），
//     避免与包内既有小写标识符重名（如 tokenize / tokenSet / normalizeEntity）。
package kernel

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// FlushReason 四类显式触发（§3.5.1）。
// 常量按枚举序递增，DecideFlush 依此稳定输出 Reasons。
type FlushReason int

const (
	// FlushSessionEnd 会话结束触发。
	FlushSessionEnd FlushReason = iota
	// FlushTopicSwitch 话题切换触发。
	FlushTopicSwitch
	// FlushTaskDone 任务完成触发。
	FlushTaskDone
	// FlushL0Capacity L0 容量阈值触发。
	FlushL0Capacity
)

// String 返回枚举的可读标签（snake_case，便于日志 / 审计）。
func (r FlushReason) String() string {
	switch r {
	case FlushSessionEnd:
		return "session_end"
	case FlushTopicSwitch:
		return "topic_switch"
	case FlushTaskDone:
		return "task_done"
	case FlushL0Capacity:
		return "l0_capacity"
	}
	return "unknown"
}

// FlushConfig flush 决策可调参数。各字段 <=0 / <0 的回落语义见字段注释。
type FlushConfig struct {
	// L0Limit L0 容量阈值，默认 64；<=0 时用 DefaultFlushConfig().L0Limit。
	L0Limit int
	// TopicSwitchOverlap 话题切换 Jaccard 阈值，默认 0.35；<=0 时用默认值。
	TopicSwitchOverlap float64
	// KeepRecent 话题切换时保留在 L0 的最近条数，默认 3；<0 视为 0；
	// 0 是合法值，语义为「不保留」（不回落到默认 3）。
	KeepRecent int
}

// DefaultFlushConfig 返回推荐默认配置：L0Limit=64, TopicSwitchOverlap=0.35,
// KeepRecent=3。
func DefaultFlushConfig() FlushConfig {
	return FlushConfig{
		L0Limit:            64,
		TopicSwitchOverlap: 0.35,
		KeepRecent:         3,
	}
}

// FlushSignal 一次 flush 决策的输入快照。
type FlushSignal struct {
	// Now 观测时刻（注入，便于确定性测试；本决策不使用，仅供上层记录）。
	Now time.Time
	// SessionEnd 会话是否已关闭。
	SessionEnd bool
	// TaskDone 当前任务是否已完成。
	TaskDone bool
	// L0Count 当前 L0 记忆数。
	L0Count int
	// L0Limit 调用方覆盖的 L0 容量阈值；>0 时优先于 cfg.L0Limit。
	L0Limit int
	// Items 需结算的 L0 memoryID（按写入顺序，首=最旧）。
	Items []string
	// PrevSnippet 上一条记忆片段（话题指纹用）。
	PrevSnippet string
	// NewSnippet 拟写入片段。
	NewSnippet string
}

// FlushPlan 纯决策输出：调用方按此决定是否执行实际落库。
type FlushPlan struct {
	// Flush 是否结算。
	Flush bool
	// Reasons 触发的原因列表，按枚举序稳定输出，无重复。
	Reasons []FlushReason
	// Items 本轮结算项：去重 + 字典序；空输入返回非 nil 空切片。
	Items []string
	// HoldItems 留待下轮：仅话题切换 + KeepRecent>0 时可能非空；
	// 取输入顺序尾部 KeepRecent 条，输出按字典序；与 Items 不相交。
	HoldItems []string
}

// flushNormalize 归一化：转小写 + 去首尾空白。
func flushNormalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// flushTokens 轻量分词（话题指纹用）：转小写 + 去首尾空白，ASCII 按词，
// CJK 取单字 + 相邻双字（bigram）。对大小写、标点、多空格不敏感。
//
// 逻辑与 rank_fusion.go 的 tokenize 等价；本文件独立实现，避免符号耦合，
// 也符合本文件私有函数一律 flush 前缀的约束。
func flushTokens(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	var toks []string
	var ascii []rune
	var cjk []rune
	emitASCII := func() {
		if len(ascii) > 0 {
			toks = append(toks, string(ascii))
			ascii = nil
		}
	}
	emitCJK := func() {
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
			emitCJK()
			ascii = append(ascii, r)
		case unicode.Is(unicode.Han, r):
			emitASCII()
			cjk = append(cjk, r)
		default:
			emitASCII()
			emitCJK()
		}
	}
	emitASCII()
	emitCJK()
	return toks
}

// flushTokenSet 词集（去重）。
func flushTokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range flushTokens(s) {
		out[t] = true
	}
	return out
}

// TopicFingerprintOverlap 计算两个片段的 Jaccard 相似度，规则：
//
//   - 两边词集均空 → 1.0（视为同一话题，避免空值误触发切换）；
//   - 只有一边空   → 0.0（视为完全不同）；
//   - 否则        → |交集| / |并集|，落在 [0,1]。
//
// 词集对大小写、标点、多空格不敏感（内部使用 flushTokenSet）。
func TopicFingerprintOverlap(a, b string) float64 {
	ta := flushTokenSet(a)
	tb := flushTokenSet(b)
	na, nb := len(ta), len(tb)
	if na == 0 && nb == 0 {
		return 1.0
	}
	if na == 0 || nb == 0 {
		return 0.0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := na + nb - inter
	return float64(inter) / float64(union)
}

// DecideFlush 产出纯决策计划。
//
// 规则（严格对齐 §3.5.1）：
//  1. 容量触发：sig.L0Count >= effectiveL0Limit（边界 == 触发）。
//     effectiveL0Limit 优先取 sig.L0Limit>0，其次 cfg.L0Limit>0，最后回落 64。
//  2. 话题切换：PrevSnippet 与 NewSnippet 归一化后均非空，且
//     TopicFingerprintOverlap <= effectiveTopicSwitchOverlap。
//     effectiveTopicSwitchOverlap：cfg.TopicSwitchOverlap>0 时用之，否则回落 0.35。
//  3. SessionEnd → FlushSessionEnd；TaskDone → FlushTaskDone。
//  4. 任一命中即 Flush=true；Reasons 按枚举序稳定输出，不重复。
//  5. Items：去重后按字典序；空输入返回非 nil 空切片。
//  6. HoldItems：仅话题切换 + effectiveKeepRecent>0 + 去重后条数>KeepRecent
//     时非空，取输入顺序尾部 KeepRecent 条，输出按字典序；与 Items 不相交。
//     effectiveKeepRecent：cfg.KeepRecent<0 视为 0；0 表示「不保留」。
//
// 纯确定性、无 I/O、无随机、无时间依赖；同一输入多次调用结构完全一致。
func DecideFlush(sig FlushSignal, cfg FlushConfig) FlushPlan {
	def := DefaultFlushConfig()

	// 有效 L0Limit：sig.L0Limit > 0 优先，其次 cfg.L0Limit > 0，最后回落默认。
	effectiveL0 := def.L0Limit
	if cfg.L0Limit > 0 {
		effectiveL0 = cfg.L0Limit
	}
	if sig.L0Limit > 0 {
		effectiveL0 = sig.L0Limit
	}

	// 有效 topic switch overlap：<=0 回落默认。
	effectiveOverlap := def.TopicSwitchOverlap
	if cfg.TopicSwitchOverlap > 0 {
		effectiveOverlap = cfg.TopicSwitchOverlap
	}

	// 有效 KeepRecent：<0 视为 0；0 是合法值（不保留），不回落到默认 3。
	effectiveKeep := cfg.KeepRecent
	if effectiveKeep < 0 {
		effectiveKeep = 0
	}

	// 按枚举序稳定收集触发原因，任一命中即 Flush=true。
	reasons := []FlushReason{}
	if sig.SessionEnd {
		reasons = append(reasons, FlushSessionEnd)
	}

	// 话题切换：先做归一化非空检查，再计算 Jaccard。
	topicSwitch := false
	if p := flushNormalize(sig.PrevSnippet); p != "" {
		if n := flushNormalize(sig.NewSnippet); n != "" {
			if ov := TopicFingerprintOverlap(sig.PrevSnippet, sig.NewSnippet); ov <= effectiveOverlap {
				topicSwitch = true
				reasons = append(reasons, FlushTopicSwitch)
			}
		}
	}

	if sig.TaskDone {
		reasons = append(reasons, FlushTaskDone)
	}
	if sig.L0Count >= effectiveL0 {
		reasons = append(reasons, FlushL0Capacity)
	}

	// Items 去重（保留输入顺序）→ 字典序输出。
	seen := map[string]bool{}
	unique := []string{}
	for _, id := range sig.Items {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	items := make([]string, len(unique))
	copy(items, unique)
	sort.Strings(items)

	// HoldItems：仅话题切换 + KeepRecent>0 + 去重后条数>KeepRecent 时非空。
	// 取输入顺序尾部 KeepRecent 条，输出前按字典序排序，并从 Items 中剔除。
	holdItems := []string{}
	if topicSwitch && effectiveKeep > 0 && len(unique) > effectiveKeep {
		tail := unique[len(unique)-effectiveKeep:]
		holdItems = make([]string, len(tail))
		copy(holdItems, tail)
		sort.Strings(holdItems)

		holdSet := map[string]bool{}
		for _, h := range holdItems {
			holdSet[h] = true
		}
		filtered := []string{}
		for _, it := range items {
			if !holdSet[it] {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}

	return FlushPlan{
		Flush:     len(reasons) > 0,
		Reasons:   reasons,
		Items:     items,
		HoldItems: holdItems,
	}
}
