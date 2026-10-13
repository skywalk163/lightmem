// 控制面光明语言压缩（R5-D 任务书 §D.3.4）。
//
// 目标：对 query / 路由 prompt 用**确定性规则**（关键词压缩、停用词剔除、
// 模板化）降低 token 数，输出压缩前后对比；压缩后覆盖率不降（由
// polaris_eval -compress 复跑 M1 验证，见交付报告）。
//
// 规则（有序应用，全部确定性——固定词表 + 固定正则，无随机源、无 map 迭代序）：
//
//	R1 填充前缀剔除：礼貌/意图填充语（请问|麻烦|帮我|我想知道|请告诉我|
//	   please tell me|could you tell me|...）整段删除；
//	R2 句末语气词剔除：吗|呢|吧|啊|呀|嘛|哦（仅尾部）；
//	R3 句末疑问词/系词剔除：什么|怎么|怎样|哪些|哪个|多少|是|有|是有
//	   （仅尾部，保留句中意图结构如「是否」——「是否有效」类查询的意图词不删）；
//	R5 英文停用词剔除：词边界匹配（the/is/are/what/who/...；不含 a/an，
//	   见 enStopWords 注释）；
//	R6 模板化：路由/召回指令由自然语言长模板换为光明语言短模板
//	   （见 polaris_token.go 的 Directive 常量对）。
//
// ⚠️ R4（结构虚词 的/了 文中剔除）**已下线**：R5-D 压缩前后覆盖率对照实验
// 实测，删「的」会误伤「X的Y是什么（周三）」类带锚点查询（7/590 条单跳样本
// top-1 翻车），而压缩率仅从 35.77% 提升到 37.42%——收益极小、覆盖率有损，
// 按「覆盖率不降」验收口径下线。复合词误伤（目的地→目地）风险也随之消除。
//
// 安全护栏：压缩结果为空时回退原 query（禁止把查询压成空串）。
package main

import (
	"regexp"
	"strings"
)

// cnFillerPrefixes 填充前缀（整段删除，长词优先避免子串竞争）。
var cnFillerPrefixes = []string{
	"请告诉我", "我想知道", "我想了解", "请帮我", "帮我", "请问", "麻烦", "想要知道",
}

// enFillerPrefixes 英文填充前缀（整段删除，长词优先）。
var enFillerPrefixes = []string{
	"could you please tell me", "could you tell me", "can you tell me",
	"please tell me", "i want to know", "i'd like to know", "i would like to know",
	"please", "tell me",
}

// cnTailParticles 句末语气词。
var cnTailParticles = []string{"吗", "呢", "吧", "啊", "呀", "嘛", "哦"}

// cnTailInterrogatives 句末疑问词（仅尾部剔除，保护句中意图词）。
var cnTailInterrogatives = []string{"什么", "怎么", "怎样", "哪些", "哪个", "多少"}

// cnTailCopula 句末系词/存在词（仅尾部剔除；句中不删——「是否有效」的
// 「是否」在词中不受影响，只有整词落尾才删）。
var cnTailCopula = []string{"是有", "是", "有"}

// enStopWords 英文停用词（词边界剔除）。
// ⚠️ 不含 "a"/"an"：RE2 的 \b 以 ASCII 词字符为界，中文实体名（客户A/客户B）
// 旁的单个 ASCII 字母会被 \b 孤立成「单词」而被 "a" 误删（R5-D 实测踩坑，
// 单测锁定）。代价是英文冠词略省，换取实体名安全。
var enStopWords = []string{
	"the", "is", "are", "was", "were", "be", "been", "being",
	"of", "for", "to", "in", "on", "at", "with", "from", "by", "and", "or",
	"it", "its", "this", "that", "these", "those",
	"what", "who", "when", "where", "which", "how",
	"please", "could", "would", "should", "can", "will", "shall", "may", "might",
	"my", "your", "our", "their", "his", "her",
}

var (
	// enStopWordRe 英文停用词词边界匹配（大小写不敏感；列表按长度降序编译，
	// 保证 "the" 不会先于 "their" 匹配——实际由 alternation 顺序保证）。
	enStopWordRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(sortedByLenDesc(enStopWords), "|") + `)\b`)
	// wsRe 连续空白。
	wsRe = regexp.MustCompile(`\s+`)
	// tailPunctRe 尾部标点（压缩后清理）。
	tailPunctRe = regexp.MustCompile(`[\s,，。.!？?、;；:：]+$`)
)

// sortedByLenDesc 返回按长度降序的副本（正则 alternation 长词优先，
// 避免 "a" 抢先匹配 "an" 的前缀造成语义偏差——\b 边界已防前缀误配，
// 此处仅双重保险）。
func sortedByLenDesc(words []string) []string {
	out := make([]string, len(words))
	copy(out, words)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// CompressQuery 对单条 query 应用 R1-R5 确定性压缩规则。
// 永不返回空串：若规则删空则回退 trim 后的原 query。
// 同一输入逐位一致（无随机源）。
func CompressQuery(query string) string {
	s := strings.TrimSpace(query)
	if s == "" {
		return s
	}
	orig := s
	s = stripFillerPrefixes(s)
	s = stripTail(s, cnTailParticles)
	s = stripTail(s, cnTailInterrogatives)
	s = stripTail(s, cnTailCopula)
	s = enStopWordRe.ReplaceAllString(s, " ")
	s = wsRe.ReplaceAllString(s, " ")
	s = tailPunctRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return orig // 安全护栏：禁止压成空查询
	}
	return s
}

// stripFillerPrefixes 删除句首填充语（中英，长词优先，循环到稳定）。
func stripFillerPrefixes(s string) string {
	for changed := true; changed; {
		changed = false
		trimmed := strings.TrimSpace(s)
		for _, p := range append(append([]string{}, cnFillerPrefixes...), enFillerPrefixes...) {
			if len(trimmed) >= len(p) && strings.EqualFold(trimmed[:len(p)], p) {
				trimmed = strings.TrimSpace(trimmed[len(p):])
				s = trimmed
				changed = true
				break
			}
		}
	}
	return s
}

// stripTail 删除句尾词表中的词（长词优先；循环到稳定，如「…吗呢」）。
func stripTail(s string, words []string) string {
	for changed := true; changed; {
		changed = false
		t := strings.TrimSpace(s)
		for _, w := range sortedByLenDesc(words) {
			if strings.HasSuffix(t, w) {
				t = strings.TrimSpace(t[:len(t)-len(w)])
				s = t
				changed = true
				break
			}
		}
	}
	return s
}

// ------------------------------------------------------------------
// 压缩对比报告。
// ------------------------------------------------------------------

// CompressionItem 单条 query 的压缩前后对比。
type CompressionItem struct {
	Query         string `json:"query"`
	Compressed    string `json:"compressed"`
	ControlBefore int    `json:"control_tokens_before"`
	ControlAfter  int    `json:"control_tokens_after"`
	Saved         int    `json:"saved_tokens"`
}

// CompressionReport 压缩前后总量对比（控制面口径）。
type CompressionReport struct {
	Queries       int `json:"queries"`
	ControlBefore int `json:"control_tokens_before"`
	ControlAfter  int `json:"control_tokens_after"`
	Saved         int `json:"saved_tokens"`
	// CompressionRate 压缩率 = (before-after)/before（目标 ≥0.20）。
	CompressionRate float64           `json:"compression_rate"`
	Items           []CompressionItem `json:"items,omitempty"`
}

// CompressControlPlane 对一组 query 计算控制面压缩前后 token 对比。
// 控制面 = 路由指令 + query + 召回指令 + query（见 ControlPlaneText）。
// 确定性：同输入逐位一致。
func CompressControlPlane(queries []string) CompressionReport {
	rep := CompressionReport{Items: make([]CompressionItem, 0, len(queries))}
	for _, q := range queries {
		before := ControlPlaneTokens(q, false)
		after := ControlPlaneTokens(q, true)
		if after > before { // 护栏：压缩不得增 token（规则只删不增，防御性断言）
			after = before
		}
		rep.Queries++
		rep.ControlBefore += before
		rep.ControlAfter += after
		rep.Items = append(rep.Items, CompressionItem{
			Query:         q,
			Compressed:    CompressQuery(q),
			ControlBefore: before,
			ControlAfter:  after,
			Saved:         before - after,
		})
	}
	rep.Saved = rep.ControlBefore - rep.ControlAfter
	if rep.ControlBefore > 0 {
		rep.CompressionRate = float64(rep.Saved) / float64(rep.ControlBefore)
	}
	return rep
}

// ------------------------------------------------------------------
// R6-C 数据面压缩：候选 fact 展示/计费面文本压缩。
//
// 安全边界（任务书风险表）：压缩**只作用于展示/计费面的候选文本副本**，
// 不作用于 matchFactV4 的判定输入（双口径对照见 polaris_holdout.go）。
// 全部规则确定性：固定正则 + 固定截断长度，无随机性。
// ------------------------------------------------------------------

// dataFactSpeakerRe 对话体 speaker 前缀（"Sam: " / "Evan: " / "John: "）。
var dataFactSpeakerRe = regexp.MustCompile(`^[A-Z][A-Za-z'’]+:\s*`)

// dataFactSentenceRe 句子边界（保留首句做线索优先/核心块保留）。
var dataFactSentenceRe = regexp.MustCompile(`[.!?。！？]+`)

// dataFactMaxRunes 无 => 对话体的首句保留上限（核心块最大长度）。
const dataFactMaxRunes = 60

// CompressDataFact 数据面压缩一条候选 fact（展示/计费面副本）。
//
//	规则1 剥离 speaker 前缀（对话体噪音，不含判定线索）；
//	规则2 "Q => A" 事实只保留回答侧——问题镜像在控制面已计费，数据面不重复；
//	规则3 无 => 的对话体保留首句（线索优先），超上限按 rune 截断；
//	护栏 压缩结果为空时返回原文（永不产生空数据块）。
//
// 注意：本函数只用于展示/计费面 token 估算副本；matchFactV4 判定输入不受影响。
func CompressDataFact(fact string) string {
	s := strings.TrimSpace(fact)
	if s == "" {
		return s
	}
	orig := s
	// 规则1：speaker 前缀。
	s = dataFactSpeakerRe.ReplaceAllString(s, "")
	// 规则2：Q=>A 只留回答侧。
	if i := strings.Index(s, "=>"); i >= 0 {
		if ans := strings.TrimSpace(s[i+len("=>"):]); ans != "" {
			s = ans
		}
	} else {
		// 规则3：无 =>，首句核心块保留。跳过呼语碎片（"Hey Evan!" / "Hi!"），
		// 保留第一个 ≥10 rune 的实质句；超长按 rune 截断（词界内收，不切词中）。
		for _, part := range dataFactSentenceRe.Split(s, -1) {
			part = strings.TrimSpace(part)
			if len([]rune(part)) < 10 {
				continue
			}
			s = part
			break
		}
		if r := []rune(s); len(r) > dataFactMaxRunes {
			cut := string(r[:dataFactMaxRunes])
			if i := strings.LastIndexAny(cut, " ,—-"); i >= 0 && i > 0 {
				cut = cut[:i]
			}
			s = strings.TrimSpace(cut)
		}
	}
	s = tailPunctRe.ReplaceAllString(s, "")
	if s == "" {
		return orig
	}
	return s
}

// DataPlaneTokensBeforeAfter 数据面压缩前后 token 对照（确定性账目）。
type DataPlaneTokensBeforeAfter struct {
	TokensBefore int     `json:"tokens_before"`
	TokensAfter  int     `json:"tokens_after"`
	Saved        int     `json:"saved"`
	Reduction    float64 `json:"reduction_rate"`
}

// CompressDataPlaneAccount 批量统计候选 fact 列表的数据面 token 前后对照。
func CompressDataPlaneAccount(facts []string) DataPlaneTokensBeforeAfter {
	var acct DataPlaneTokensBeforeAfter
	for _, f := range facts {
		acct.TokensBefore += EstimateTokens(f)
		acct.TokensAfter += EstimateTokens(CompressDataFact(f))
	}
	acct.Saved = acct.TokensBefore - acct.TokensAfter
	if acct.TokensBefore > 0 {
		acct.Reduction = float64(acct.Saved) / float64(acct.TokensBefore)
	}
	return acct
}
