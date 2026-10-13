// polaris_fp.go — R6-C 残留假阳性（FP）确定性审计层。
//
// 背景（R5 收口 P4）：v4 事实判定在公开基准上放过 3 条语义错误命中——
//  1. lme-031748ae_abs×2：expected 是拒绝式回答（"The information provided
//     is not enough..."），got 却是肯定答案，v4 因共享词元放行（否定语义冲突）；
//  2. lc-conv-49-D1_16：expected="friend's advice" 极短事实，got 是一段与
//     问题无关的对话碎片（碎片化假阳性）。
//
// 本文件是**判定后的审计层**：不改 matchFactV4 / JudgeM1 的判定口径（红线：
// 不得改检索判定口径使 FP「假性消失」），只在 v4 已判 hit 的结果上做确定性
// 语义一致性复核。所有规则纯字符串、固定顺序、无 LLM。
package main

import (
	"regexp"
	"strings"
)

// fpRefusalMarkers expected 为「拒绝/否定式回答」的标记（小写子串匹配）。
// 命中任一即视为 expected 在语义上是「信息不足 / 没有 / 拒绝回答」。
var fpRefusalMarkers = []string{
	"not enough",
	"no, ",
	"i don't",
	"there is no",
	"not mentioned",
	"do not have",
	"does not have",
	"didn't",
	"cannot",
	"can't",
	"not provided",
	"unable",
}

// fpWordBoundary 用于词界短语包含判定（碎片化护栏）。
var fpWordBoundary = regexp.MustCompile(`[a-z0-9]+`)

// fpTemporalRe 时序型短答案识别（年份/月份/相对时间词）。
// 命中即豁免碎片化护栏——时序推理是合法命中路径。
var fpTemporalRe = regexp.MustCompile(`(19|20)\d{2}|\b(january|february|march|april|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b|end of|start of|early |late |last |next |this week|yesterday|tomorrow`)

// FPAnswerSide 取 fact 的「回答侧」：含 "=>" 取其后半段（Q=>A 事实格式），
// 否则整体。否定一致性只看回答内容，不看问题镜像。
func FPAnswerSide(got string) string {
	s := strings.TrimSpace(got)
	if i := strings.Index(s, "=>"); i >= 0 {
		return strings.TrimSpace(s[i+len("=>"):])
	}
	return s
}

// IsRefusalExpected 判断 expected 是否为拒绝/否定式回答，返回命中的标记列表。
func IsRefusalExpected(expected string) []string {
	e := strings.ToLower(expected)
	var hits []string
	for _, m := range fpRefusalMarkers {
		if strings.Contains(e, m) {
			hits = append(hits, m)
		}
	}
	return hits
}

// fpPhraseContained 词界短语包含判定：exp 的每个内容词都在 got 的词元集合中。
// 用于碎片化护栏的「整词落地」要求（比 v4 短词元路径更严：要求 exp 的全部
// 内容词逐词出现在 got 中，而非 v4 的同名要求——此处额外排除对话 speaker 噪音）。
func fpPhraseContained(exp, got string) bool {
	need := factContentTokens(exp)
	if len(need) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, w := range fpWordBoundary.FindAllString(strings.ToLower(got), -1) {
		have[w] = true
	}
	for _, w := range need {
		if !have[w] {
			return false
		}
	}
	return true
}

// FPVeto 对 v4 已判 hit 的 (expected, got) 做假阳性复核。
//
// 返回 (vetoed, rule)：
//   - vetoed=true：该 hit 在语义上矛盾，审计口径应剔除（不计 hit）；
//   - rule="negation_conflict"：规则1 否定一致性——expected 是拒绝式回答而 got
//     回答侧无任何拒绝标记（给出了肯定答案），二者语义矛盾；
//   - rule="fragment"：规则2 碎片化护栏强化——expected 极短（≤3 内容词），
//     got 又无 "=>" 回答结构（纯对话碎片），且 expected 未整词落在 got 中；
//   - vetoed=false：审计层放行（与 v4 判定一致）。
//
// 护栏：expected 无拒绝标记时规则1 不触发；got 回答侧同样含拒绝标记时规则1
// 不触发（真命中：取到的就是那条拒绝记忆本身）。
func FPVeto(expected, got string) (bool, string) {
	expMarkers := IsRefusalExpected(expected)
	if len(expMarkers) > 0 {
		ansMarkers := IsRefusalExpected(FPAnswerSide(got))
		if len(ansMarkers) == 0 {
			return true, "negation_conflict"
		}
	}
	// 规则2：短事实碎片化护栏。
	// 豁免**时序型短答案**（"in 2022"/"July 2023"）：v4 的 matchFactTemporal 会
	// 把 got 的相对时间（last year/next month）结合 anchor 推成绝对日期，属合法
	// 时序推理命中，不是碎片化假阳性（R6-C 实测 6 条时序命中被误伤，加此豁免）。
	if ct := factContentTokens(expected); len(ct) > 0 && len(ct) <= 3 && !fpTemporalRe.MatchString(strings.ToLower(expected)) {
		if !strings.Contains(got, "=>") && !fpPhraseContained(expected, got) {
			return true, "fragment"
		}
	}
	return false, ""
}

// FPAuditRow 单条 v4-hit 的审计结果（入报告）。
type FPAuditRow struct {
	ID       string `json:"id"`
	Expected string `json:"expected"`
	Got      string `json:"got_fact"`
	Vetoed   bool   `json:"vetoed"`
	Rule     string `json:"rule,omitempty"`
}

// FPAuditReport 假阳性审计汇总（C 独立口径）。
type FPAuditReport struct {
	V4Hits     int          `json:"v4_hits"`
	Vetoed     int          `json:"vetoed"`
	ResidualFP int          `json:"residual_fp"`
	Rows       []FPAuditRow `json:"rows,omitempty"`
}
