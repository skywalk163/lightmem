// Package benchmarks 提供公开基准（LongMemEval / LoCoMo / BEAM）复跑所需的
// 确定性适配判定口径。
//
// ⚠️ 非北极星口径：本文件的 MatchFactBenchmarks 是**公开基准适配判定口径**，
// 与北极星判定口径（eval/polaris 的 matchFactV4 / JudgeM1）相互独立、并存不混用，
// 不参与北极星 M1 判定。它只解决公开基准的一个**形态假阴性**问题：
//
//	LongMemEval 的 got 文本是 `question => answer` 形态（LoCoMo 为 `speaker: turn`），
//	而 expected 是纯答案片段。若用「got == expected」的精确比对，准确率会被形态差
//	压到接近 0%（假 0%）。本口径改为「expected 以确定性文本包含关系出现在 got
//	（或其 `=>` 之后的答案段）中即判命中」。
//
// 红线：纯字符串归一 + 包含判定，确定性、无随机源、不调 LLM。
package benchmarks

import (
	"regexp"
	"strings"
)

// 数值 / 日期边界归一（与 eval/run_kpi.go 同源的确定性归一口径，本包自包含）。
// 说明：Go 的 regexp 替换为「最左非重叠」语义，故多位千分位（如 1,234,567）只会
// 消去第一段逗号；该行为在被比对的两侧一致，不影响判定的确定性。
var (
	benchCurrency  = regexp.MustCompile(`[$€£¥]`)
	benchThousands = regexp.MustCompile(`(\d),(\d{3})`)
	benchDateSep   = regexp.MustCompile(`(\d{4})[-/](\d{1,2})[-/](\d{1,2})`)
)

// NormalizeBenchFact 公开基准适配口径的基础归一：整体小写 + 压缩空白。
func NormalizeBenchFact(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// NormalizeBenchFactBoundary 在基础归一之上再做数值/日期边界归一：
//   - 去货币符号：$3,750 → 3750
//   - 去千分位逗号：1,234 → 1234
//   - 日期分隔符压缩：2023-04-01 / 2023/04/01 → 20230401
//
// 使 "got=$3,750" 与 "expected=3750"、"got=20230401" 与 "expected=2023-04-01"
// 这类形态差被抹平。
func NormalizeBenchFactBoundary(s string) string {
	s = NormalizeBenchFact(s)
	s = benchCurrency.ReplaceAllString(s, "")
	s = benchThousands.ReplaceAllString(s, "$1$2")
	s = benchDateSep.ReplaceAllString(s, "$1$2$3")
	return s
}

// AnswerSegment 取 got 中 `=>` 之后的答案段（LongMemEval 形态）；
// 无 `=>` 时返回原串（LoCoMo `speaker: turn` 形态）。
func AnswerSegment(got string) string {
	if i := strings.Index(got, "=>"); i >= 0 {
		return got[i+len("=>"):]
	}
	return got
}

// MatchFactBenchmarks 公开基准适配判定口径（⚠️ 非北极星口径）。
//
// 判定顺序（任一步命中即 true），全部确定性、不调 LLM：
//  1. 基础归一后 expected ⊆ got；
//  2. 基础归一后 expected ⊆ got 的 `=>` 答案段；
//  3. 数值/日期边界归一后 expected ⊆ got；
//  4. 数值/日期边界归一后 expected ⊆ got 的 `=>` 答案段。
//
// expected 为空（或归一后为空）返回 false，不计入命中。
func MatchFactBenchmarks(got, expected string) bool {
	if strings.TrimSpace(expected) == "" {
		return false
	}
	b := NormalizeBenchFact(expected)
	if b == "" {
		return false
	}
	a := NormalizeBenchFact(got)
	if a == b || strings.Contains(a, b) {
		return true
	}
	seg := NormalizeBenchFact(AnswerSegment(got))
	if seg == b || strings.Contains(seg, b) {
		return true
	}
	ab := NormalizeBenchFactBoundary(got)
	bb := NormalizeBenchFactBoundary(expected)
	if bb != "" && (ab == bb || strings.Contains(ab, bb)) {
		return true
	}
	segB := NormalizeBenchFactBoundary(AnswerSegment(got))
	if bb != "" && (segB == bb || strings.Contains(segB, bb)) {
		return true
	}
	return false
}
