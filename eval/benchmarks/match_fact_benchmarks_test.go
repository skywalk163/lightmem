// R8-B：公开基准适配判定口径单测（⚠️ 非北极星口径）。
//
// 运行：
//
//	cd eval && go test lightmem/eval/benchmarks
//
// 覆盖：基础归一 / 数值日期边界归一 / `=>` 答案段提取 / 包含判定正负例 /
// 空 expected / 形态假阴性（question => answer）修复。
//
// 全部为纯字符串断言，确定性、无随机源、不调 LLM。
package benchmarks

import "testing"

// 1. NormalizeBenchFact 基础归一（小写 + 压缩空白）。
func TestNormalizeBenchFact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Hello   World  ", "hello world"},
		{"The Meeting Is At Noon", "the meeting is at noon"},
		{"", ""},
		{"\t a\n b ", "a b"},
	}
	for _, c := range cases {
		if got := NormalizeBenchFact(c.in); got != c.want {
			t.Fatalf("NormalizeBenchFact(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// 2. NormalizeBenchFactBoundary 数值 / 日期边界归一。
func TestNormalizeBenchFactBoundary(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$3,750", "3750"},
		{"€99", "99"},
		{"¥1,000", "1000"},
		{"Price: $1,234", "price: 1234"},
		{"2023-04-01", "20230401"},
		{"2023/04/01", "20230401"},
		{"no numbers here", "no numbers here"},
		{"  Mixed $5,000 on 2023/12/31 ", "mixed 5000 on 20231231"},
	}
	for _, c := range cases {
		if got := NormalizeBenchFactBoundary(c.in); got != c.want {
			t.Fatalf("NormalizeBenchFactBoundary(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// 3. AnswerSegment 提取 `=>` 答案段；无 `=>` 返回原串。
func TestAnswerSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"What did I buy? => a cooking book", " a cooking book"},
		{"no arrow here", "no arrow here"},
		{"prefix => suffix => tail", " suffix => tail"},
		{"=>only", "only"},
	}
	for _, c := range cases {
		if got := AnswerSegment(c.in); got != c.want {
			t.Fatalf("AnswerSegment(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// 4. MatchFactBenchmarks 判定正负例（形态假阴性修复 + 边界 + 负例）。
func TestMatchFactBenchmarks(t *testing.T) {
	cases := []struct {
		name string
		got  string
		exp  string
		want bool
	}{
		// 形态假阴性修复：`question => answer` 中 expected 是纯答案。
		{"箭头答案段包含", "What did I buy last week? => a cooking book", "a cooking book", true},
		{"箭头答案段包含（大小写）", "Q: Where? => In New York City", "in new york city", true},
		// LoCoMo `speaker: turn` 形态：expected 直接出现在整段中。
		{"speaker turn 包含", "John: I bought a cooking book as a gift", "cooking book", true},
		{"完全相等", "The answer is 42", "The answer is 42", true},
		// 数值 / 日期边界归一。
		{"货币形态差", "the total is 3750", "the total is $3,750", true},
		{"日期形态差", "it started on 20230401", "2023-04-01", true},
		// 负例。
		{"无关文本", "completely unrelated text here", "quarterly revenue projection", false},
		{"无关子串缺失", "the showcase was nice", "revenue", false},
		{"空 expected", "any got text", "", false},
		{"空白 expected", "any got text", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchFactBenchmarks(c.got, c.exp); got != c.want {
				t.Fatalf("MatchFactBenchmarks(%q,%q)=%v, want %v", c.got, c.exp, got, c.want)
			}
		})
	}
}

// 5. 确定性：同输入恒等（无随机源）。
func TestMatchFactBenchmarksDeterministic(t *testing.T) {
	got, exp := "Q? => the total is 3750", "the total is $3,750"
	first := MatchFactBenchmarks(got, exp)
	for i := 0; i < 100; i++ {
		if MatchFactBenchmarks(got, exp) != first {
			t.Fatalf("MatchFactBenchmarks 非确定：第 %d 次结果与首次不一致", i)
		}
	}
	if !first {
		t.Fatalf("货币形态差应命中：got=%q exp=%q", got, exp)
	}
}
