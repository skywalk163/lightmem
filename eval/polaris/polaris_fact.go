// fact 判定口径移植（M1「拿到正确事实」的判定器）。
//
// 来源：eval/run_kpi.go R2→R3-C→R4-C→R5-B 演进的事实匹配链（v2 包含 +
// v3 char trigram 软包含 + v4 模板剔除/时间推理/数值日期归一/短词元包含 +
// R5-B 边界归一化与边界对齐护栏）。
//
// 为什么移植而不 import：run_kpi.go 是 //go:build ignore 的单文件 main，
// 不可被 import；北极星 M1 必须与 KPI 评测的 fact 口径**同源**，否则
// 「零 token 覆盖率」与「fact 准确率」不可比。移植保持逐位一致的判定语义，
// 任何口径演进须两处同步（本文件头部注明来源行号区间 run_kpi.go:1370-2005）。
//
// 纯字符串与日历推算：不调 LLM（红线 4），无随机源（seed 无关，确定性）。
package main

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// factV3Threshold 是 matchFactV3 的 char trigram 相似度阈值（与 run_kpi.go 默认一致）。
var factV3Threshold = 0.55

// factV3SimAlgo 相似度算法：dice|contain（与 run_kpi.go 默认一致）。
var factV3SimAlgo = "dice"

// factNormalizeEnabled R5-B 边界归一化 + 边界对齐护栏开关（北极星默认开启，
// 与 run_kpi.go R5 后默认值一致）。
var factNormalizeEnabled = true

// matchFact 判定 gotFact 是否匹配 expectedFact（忽略大小写与空白）。
// 策略：归一化精确相等 → 短 expected answer 段边界匹配 → 长 expected 双向包含。
func matchFact(gotFact, expectedFact string) bool {
	if expectedFact == "" {
		return false
	}
	a := normalizeFact(gotFact)
	b := normalizeFact(expectedFact)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(b) < 4 {
		return shortAnswerHit(a, b)
	}
	if len(a) < len(b)*3/10 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// shortAnswerHit 短 expected（数字/单词）在 fact 的 answer 段内做边界匹配。
func shortAnswerHit(fact, exp string) bool {
	_, answer, ok := strings.Cut(fact, "=>")
	if !ok {
		answer = fact
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return false
	}
	if answer == exp {
		return true
	}
	return strings.HasPrefix(answer, exp+" ") ||
		strings.HasSuffix(answer, " "+exp) ||
		strings.Contains(" "+answer+" ", " "+exp+" ")
}

// normalizeFact 归一化：小写、去首尾空白、压缩连续空白。
func normalizeFact(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

// charNGramSimilarity 计算两段归一化文本的 char n-gram Dice 系数。
func charNGramSimilarity(a, b string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	gramsA := make(map[string]struct{})
	gramsB := make(map[string]struct{})
	ra := []rune(a)
	rb := []rune(b)
	for i := 0; i+n <= len(ra); i++ {
		gramsA[string(ra[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rb); i++ {
		gramsB[string(rb[i:i+n])] = struct{}{}
	}
	if len(gramsA) == 0 || len(gramsB) == 0 {
		return 0.0
	}
	inter := 0
	for g := range gramsA {
		if _, ok := gramsB[g]; ok {
			inter++
		}
	}
	return 2.0 * float64(inter) / float64(len(gramsA)+len(gramsB))
}

// charNGramContainment 计算 containment（软包含）：exp 的 n-gram 有多少出现在 got 里。
func charNGramContainment(exp, got string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	gramsExp := make(map[string]struct{})
	gramsGot := make(map[string]struct{})
	re := []rune(exp)
	rg := []rune(got)
	for i := 0; i+n <= len(re); i++ {
		gramsExp[string(re[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rg); i++ {
		gramsGot[string(rg[i:i+n])] = struct{}{}
	}
	if len(gramsExp) == 0 {
		return 0.0
	}
	inter := 0
	for g := range gramsExp {
		if _, ok := gramsGot[g]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(gramsExp))
}

// factV3Score 按当前 factV3SimAlgo 计算相似度分数。
func factV3Score(got, exp string) float64 {
	if factV3SimAlgo == "contain" {
		return charNGramContainment(exp, got, 3)
	}
	return charNGramSimilarity(got, exp, 3)
}

// matchFactV3 = matchFact（v2 包含式）+ 边界归一化复判 + char trigram 软包含。
func matchFactV3(gotFact, expectedFact string) bool {
	if matchFact(gotFact, expectedFact) {
		return true
	}
	if factNormalizeEnabled &&
		matchFact(normalizeFactBoundary(gotFact), normalizeFactBoundary(expectedFact)) {
		return true
	}
	b := normalizeFact(expectedFact)
	if utf8.RuneCountInString(b) < 15 {
		return false
	}
	a := normalizeFact(gotFact)
	if a == "" {
		return false
	}
	sim := factV3Score(a, b)
	if sim < factV3Threshold {
		return false
	}
	if factNormalizeEnabled {
		return factBoundaryGuard(gotFact, expectedFact)
	}
	return true
}

// factBoundaryGuard R5-B：char-trigram 模糊路径命中后的边界对齐护栏。
func factBoundaryGuard(gotFact, expectedFact string) bool {
	if hasCJK(expectedFact) {
		return true
	}
	ct := factContentTokens(expectedFact)
	gt := make(map[string]struct{})
	for _, w := range factContentTokens(gotFact) {
		gt[w] = struct{}{}
	}
	need := len(ct)
	if len(ct) >= 2 {
		need = 2
	}
	seen := make(map[string]struct{})
	shared := 0
	for _, w := range ct {
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		if _, ok := gt[w]; ok {
			shared++
		}
	}
	return shared >= need
}

// ------------------------------------------------------------------
// R4-C fact 口径 v4：模板剔除 / 时间推理 / 数值日期归一 / 短词元包含。
// ------------------------------------------------------------------

// factTemplatePrefix 评测集模板 expected 的固定前缀（小写归一后）。
const factTemplatePrefix = "llm response should"

// isTemplateExpected 判定 expected 是否为评测模板。
func isTemplateExpected(expectedFact string) bool {
	return strings.HasPrefix(normalizeFact(expectedFact), factTemplatePrefix)
}

// factMonthNames 月名 → 月份号（全名 + 三字母缩写）。
var factMonthNames = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
}

var (
	factReYMD       = regexp.MustCompile(`(\d{4})-(\d{1,2})-(\d{1,2})`)
	factReDMY       = regexp.MustCompile(`(\d{1,2})\s+([a-z]+),?\s+(\d{4})`)
	factReMDY       = regexp.MustCompile(`([a-z]+)\s+(\d{1,2}),?\s+(\d{4})`)
	factReMonthYear = regexp.MustCompile(`([a-z]+),?\s+(\d{4})`)
	factReYear      = regexp.MustCompile(`\b(19|20)\d{2}\b`)
)

// factDateSpec expected 中的日期规格。Kind: day|month|year。
type factDateSpec struct {
	Kind    string
	Year    int
	Month   int
	Day     int
	Present bool
}

// parseFactDateSpec 从 expected 文本解析日期规格（确定性）。
func parseFactDateSpec(expectedFact string) factDateSpec {
	s := normalizeFact(expectedFact)
	if m := factReYMD.FindStringSubmatch(s); m != nil {
		return factDateSpec{Kind: "day", Year: atoi(m[1]), Month: atoi(m[2]), Day: atoi(m[3]), Present: true}
	}
	if m := factReDMY.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[2]]; ok {
			return factDateSpec{Kind: "day", Year: atoi(m[3]), Month: mo, Day: atoi(m[1]), Present: true}
		}
	}
	if m := factReMDY.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[1]]; ok {
			return factDateSpec{Kind: "day", Year: atoi(m[3]), Month: mo, Day: atoi(m[2]), Present: true}
		}
	}
	if m := factReMonthYear.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[1]]; ok {
			return factDateSpec{Kind: "month", Year: atoi(m[2]), Month: mo, Present: true}
		}
	}
	if m := factReYear.FindString(s); m != "" {
		return factDateSpec{Kind: "year", Year: atoi(m), Present: true}
	}
	return factDateSpec{}
}

// atoi 内部最小整数解析（输入均为正则捕获的数字）。
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// factTemporalOffsets 相对时间词 → (粒度, 偏移量)。
var factTemporalOffsets = []struct {
	KW    string
	Kind  string
	Delta int
}{
	{"yesterday", "day", -1}, {"today", "day", 0}, {"tomorrow", "day", 1},
	{"last week", "day", -7}, {"next week", "day", 7},
	{"last month", "month", -1}, {"next month", "month", 1},
	{"last year", "year", -1}, {"next year", "year", 1},
}

// shiftFactDate 在锚点日期上按 (kind, n) 做确定性日历加减。
func shiftFactDate(y, mo, d int, kind string, n int) (int, int, int) {
	switch kind {
	case "day":
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
		return t.Year(), int(t.Month()), t.Day()
	case "month":
		tot := y*12 + (mo - 1) + n
		return tot / 12, tot%12 + 1, d
	default: // year
		return y + n, mo, d
	}
}

// matchFactTemporal 用锚点日期对 expected 日期规格做确定性日历比对。
func matchFactTemporal(gotFact, expectedFact string, anchor *time.Time) bool {
	if anchor == nil {
		return false
	}
	spec := parseFactDateSpec(expectedFact)
	if !spec.Present {
		return false
	}
	ay, amo, ad := anchor.Year(), int(anchor.Month()), anchor.Day()
	cands := [][3]int{{ay, amo, ad}}
	g := normalizeFact(gotFact)
	for _, off := range factTemporalOffsets {
		if strings.Contains(g, off.KW) {
			y, mo, d := shiftFactDate(ay, amo, ad, off.Kind, off.Delta)
			cands = append(cands, [][3]int{{y, mo, d}}...)
		}
	}
	for _, c := range cands {
		switch spec.Kind {
		case "day":
			if c[0] == spec.Year && c[1] == spec.Month && c[2] == spec.Day {
				return true
			}
		case "month":
			if c[0] == spec.Year && c[1] == spec.Month {
				return true
			}
		case "year":
			if c[0] == spec.Year {
				return true
			}
		}
	}
	return false
}

var (
	factReCurrency  = regexp.MustCompile(`[$€£¥]`)
	factReThousands = regexp.MustCompile(`(\d),(\d{3})`)
	factReDateSep   = regexp.MustCompile(`(\d{4})[-/](\d{1,2})[-/](\d{1,2})`)
)

// normalizeFactNumbers 归一化数值/日期文本（去货币符号/千分位/统一日期分隔符）。
func normalizeFactNumbers(s string) string {
	s = strings.ToLower(s)
	s = factReCurrency.ReplaceAllString(s, "")
	for factReThousands.MatchString(s) {
		s = factReThousands.ReplaceAllString(s, "$1$2")
	}
	s = factReDateSep.ReplaceAllString(s, "$1 $2 $3")
	return strings.Join(strings.Fields(s), " ")
}

// factReUnitTail 尾随单位点（如 "3.5kg." / "12gb." 的句末点）。
var factReUnitTail = regexp.MustCompile(`([0-9])([a-z%]+)\.`)

// normalizeFactBoundary R5-B：fact 匹配前的确定性边界归一化。
func normalizeFactBoundary(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = factReCurrency.ReplaceAllString(s, "")
	for factReThousands.MatchString(s) {
		s = factReThousands.ReplaceAllString(s, "$1$2")
	}
	s = factReDateSep.ReplaceAllString(s, "$1$2$3")
	s = factReUnitTail.ReplaceAllString(s, "$1$2")
	return strings.Join(strings.Fields(s), " ")
}

// factStopWords 短词元包含判定的停用词。
var factStopWords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "of": {}, "in": {}, "on": {}, "at": {}, "to": {},
	"for": {}, "and": {}, "or": {}, "is": {}, "was": {}, "are": {}, "were": {},
	"be": {}, "been": {}, "by": {}, "with": {}, "from": {}, "as": {}, "it": {},
	"its": {}, "this": {}, "that": {}, "these": {}, "those": {}, "his": {},
	"her": {}, "their": {}, "my": {}, "your": {}, "our": {},
}

var factReWord = regexp.MustCompile(`[a-z0-9]+`)

// factContentTokens 抽取文本的实义词元（小写、去停用词）。
func factContentTokens(s string) []string {
	var out []string
	for _, w := range factReWord.FindAllString(strings.ToLower(s), -1) {
		if _, stop := factStopWords[w]; stop {
			continue
		}
		out = append(out, w)
	}
	return out
}

// hasCJK 判定文本是否含中日韩表意字符。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// matchFactShortTokens 短 expected（非中文）词元包含判定。
func matchFactShortTokens(gotFact, expectedFact string) bool {
	if hasCJK(expectedFact) {
		return false
	}
	ct := factContentTokens(expectedFact)
	if len(ct) < 2 {
		return false
	}
	gt := make(map[string]struct{})
	for _, w := range factContentTokens(gotFact) {
		gt[w] = struct{}{}
	}
	for _, w := range ct {
		if _, ok := gt[w]; !ok {
			return false
		}
	}
	return true
}

// matchFactV4 = matchFactV3 + 确定性残留补齐（数值日期归一 / 时间推理 / 短词元包含）。
// anchor 为 top-1 候选 occurred_at（非 ground truth）。
func matchFactV4(gotFact, expectedFact string, anchor *time.Time) bool {
	if matchFactV3(gotFact, expectedFact) {
		return true
	}
	if expectedFact == "" {
		return false
	}
	if matchFactV3(normalizeFactNumbers(gotFact), normalizeFactNumbers(expectedFact)) {
		return true
	}
	if matchFactTemporal(gotFact, expectedFact, anchor) {
		return true
	}
	return matchFactShortTokens(gotFact, expectedFact)
}

// matchFactTop5 候选扩展辅助口径：短 expected 在候选 fact 列表上逐一判定。
func matchFactTop5(facts []string, expectedFact string, anchor *time.Time) bool {
	if utf8.RuneCountInString(normalizeFact(expectedFact)) >= 15 {
		return false
	}
	for _, f := range facts {
		if matchFactV4(f, expectedFact, anchor) {
			return true
		}
		if shortAnswerHit(normalizeFact(f), normalizeFact(expectedFact)) {
			return true
		}
	}
	return false
}
