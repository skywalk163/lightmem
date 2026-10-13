// 北极星评测子包确定性单测（R5-D）。
//
// 覆盖：token 估算确定性、压缩规则（不增 token/非空/规则生效）、
// fact 判定与 run_kpi.go 口径对拍的关键用例、M1/M2 累计器算术。
// 全部纯函数，无 IO、无随机源。
package main

import (
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// token 估算。
// ------------------------------------------------------------------

func TestEstimateTokensDeterministic(t *testing.T) {
	inputs := []string{
		"", "客户A的偏好是周一", "What are customer A's preferences?",
		"路由查询到场景与实体：客户A的偏好是什么\n为以下查询检索相关记忆：客户A的偏好是什么",
		"2023-04-01 $3,750 3.5kg.",
	}
	for _, in := range inputs {
		a := EstimateTokens(in)
		b := EstimateTokens(in)
		if a != b {
			t.Fatalf("EstimateTokens(%q) 非确定: %d vs %d", in, a, b)
		}
	}
}

func TestEstimateTokensKnownValues(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"客户A偏好", 5},   // 4 CJK + 1 ASCII word
		{"ab cd", 2},   // 2 words
		{"ab, cd!", 2}, // 标点不计
		{"周1", 2},      // 1 CJK + 1 word
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

// ------------------------------------------------------------------
// 控制面压缩。
// ------------------------------------------------------------------

func TestCompressQueryNeverGrowsTokens(t *testing.T) {
	queries := []string{
		"客户A的偏好是什么",
		"请问帮我查一下客户B的偏好是什么",
		"What are the customer preferences for project Alpha?",
		"是否有效",
		"2023年4月1日发生了什么",
		"a",
		"的",
	}
	for _, q := range queries {
		before := ControlPlaneTokens(q, false)
		after := ControlPlaneTokens(q, true)
		if after > before {
			t.Errorf("ControlPlaneTokens 压缩后反增: %q before=%d after=%d", q, before, after)
		}
		c := CompressQuery(q)
		if strings.TrimSpace(c) == "" {
			t.Errorf("CompressQuery(%q) 压成空串（违反安全护栏）", q)
		}
		if again := CompressQuery(c); again != c {
			t.Errorf("CompressQuery 非幂等: %q -> %q -> %q", q, c, again)
		}
	}
}

func TestCompressQueryRules(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"客户A的偏好是什么", "客户A的偏好"},                                                                        // 句末疑问词 + 系词；的/了 不删（R4 已下线）
		{"请问客户B的偏好是什么", "客户B的偏好"},                                                                      // 填充前缀
		{"What are the customer preferences for project Alpha?", "customer preferences project Alpha"}, // 英文停用词 + 句末问号
		{"项目Alpha的截止日期是哪天", "项目Alpha的截止日期是哪天"},                                                         // 「哪」不在句末词表，句中不删
		{"客户A的偏好是什么（周三）", "客户A的偏好是什么（周三）"},                                                             // 锚点查询原样保留（R4 下线的主因）
	}
	for _, c := range cases {
		if got := CompressQuery(c.in); got != c.want {
			t.Errorf("CompressQuery(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestCompressControlPlaneRate(t *testing.T) {
	queries := []string{
		"客户A的偏好是什么",
		"请问帮我查一下项目Alpha的截止日期是哪天",
		"What are the customer preferences for project Alpha?",
	}
	rep := CompressControlPlane(queries)
	if rep.Queries != len(queries) {
		t.Fatalf("queries=%d want %d", rep.Queries, len(queries))
	}
	if rep.ControlAfter > rep.ControlBefore {
		t.Fatalf("压缩后 token 反增: %d > %d", rep.ControlAfter, rep.ControlBefore)
	}
	if rep.CompressionRate <= 0 {
		t.Fatalf("压缩率未为正: %v", rep.CompressionRate)
	}
	// 指令模板短化本身贡献固定压缩（冗长指令 12+11 token → 短指令 2+2 token）。
	if rep.ControlAfter == 0 {
		t.Fatalf("压缩后控制面 token 为 0，不合预期")
	}
}

// ------------------------------------------------------------------
// fact 判定与 run_kpi.go 口径对拍（关键用例）。
// ------------------------------------------------------------------

func TestMatchFactV4Parity(t *testing.T) {
	anchor := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		got      string
		expected string
		anchor   *time.Time
		want     bool
	}{
		{"精确相等", "客户A的偏好是周一", "客户A的偏好是周一", nil, true},
		{"包含命中", "speaker: turn 客户A的偏好是周一（补充）", "客户A的偏好是周一", nil, true},
		{"货币千分位归一", "3750", "$3,750", nil, true},
		{"日期分隔符归一", "20230401", "2023-04-01", nil, true},
		{"单位大小写", "3.5kg", "3.5KG.", nil, true},
		{"时间推理昨天", "yesterday", "2026-10-12", &anchor, true},
		{"时间推理不命中", "yesterday", "2026-10-01", &anchor, false},
		{"空 expected", "anything", "", nil, false},
		{"短答案边界", "How many? => 4", "4", nil, true},
	}
	for _, c := range cases {
		if got := matchFactV4(c.got, c.expected, c.anchor); got != c.want {
			t.Errorf("%s: matchFactV4(%q,%q)=%v want %v", c.name, c.got, c.expected, got, c.want)
		}
	}
}

func TestIsTemplateExpected(t *testing.T) {
	if !isTemplateExpected("LLM response should contain: foo") {
		t.Error("模板 expected 未识别")
	}
	if isTemplateExpected("客户A的偏好是周一") {
		t.Error("正常 expected 被误判为模板")
	}
}

// ------------------------------------------------------------------
// M1 / M2 累计器。
// ------------------------------------------------------------------

func TestM1Accumulator(t *testing.T) {
	acc := NewM1Accumulator(5)
	acc.Add("single-hop", M1Trial{Bucket: "single-hop", Hit: true, HitAt1: true, HasIDs: true, FirstRank: 1})
	acc.Add("single-hop", M1Trial{Bucket: "single-hop", Hit: true, HitAt1: false, HasIDs: true, FirstRank: 3})
	acc.Add("multi-hop", M1Trial{Bucket: "multi-hop", Hit: false, HasIDs: true, FirstRank: 0})
	acc.Add("default", M1Trial{Bucket: "default", Hit: false}) // 无 ID 语料
	rep := acc.Report()
	if rep.N != 4 || rep.Hits != 2 {
		t.Fatalf("N=%d Hits=%d want 4/2", rep.N, rep.Hits)
	}
	wantCov := 2.0 / 4.0
	if rep.Coverage != wantCov {
		t.Errorf("coverage=%v want %v", rep.Coverage, wantCov)
	}
	if rep.IDTotal != 3 || rep.IDHit != 2 {
		t.Errorf("IDTotal=%d IDHit=%d want 3/2", rep.IDTotal, rep.IDHit)
	}
	wantRR := (1.0 + 1.0/3.0) / 3.0
	if rep.MeanRR < wantRR-1e-9 || rep.MeanRR > wantRR+1e-9 {
		t.Errorf("mean_rr=%v want %v", rep.MeanRR, wantRR)
	}
	if b := rep.ByBucket["single-hop"]; b.N != 2 || b.Hits != 2 {
		t.Errorf("single-hop bucket=%+v", b)
	}
	if b := rep.ByBucket["default"]; b.N != 1 || b.Hits != 0 {
		t.Errorf("default bucket=%+v", b)
	}
}

func TestM2Accumulator(t *testing.T) {
	acc := NewM2Accumulator(false)
	acc.AddAccount("b", TokenAccount{ControlTokens: 10, DataTokens: 90})
	acc.AddAccount("b", TokenAccount{ControlTokens: 20, DataTokens: 180})
	rep := acc.Report()
	if rep.Queries != 2 || rep.ControlTokens != 30 || rep.DataTokens != 270 {
		t.Fatalf("rep=%+v", rep)
	}
	want := 30.0 / 300.0
	if rep.M2 != want {
		t.Errorf("m2=%v want %v", rep.M2, want)
	}
	if rep.M2 < 0 || rep.M2 > 1 {
		t.Errorf("m2 越界: %v", rep.M2)
	}
}

// ------------------------------------------------------------------
// JudgeM1 纯函数行为。
// ------------------------------------------------------------------

func TestJudgeM1(t *testing.T) {
	topFacts := []string{"无关事实", "客户A的偏好是周一", "另一条"}
	topIDs := []string{"m-1", "m-2", "m-3"}
	tr := JudgeM1("客户A的偏好是周一", []string{"m-2"}, topFacts, topIDs, 5, nil)
	if !tr.Hit || tr.HitAt1 {
		t.Errorf("top-2 命中应为 Hit=true HitAt1=false: %+v", tr)
	}
	if tr.FirstRank != 2 || !tr.HasIDs {
		t.Errorf("FirstRank=%d HasIDs=%v want 2/true", tr.FirstRank, tr.HasIDs)
	}
	tr2 := JudgeM1("客户A的偏好是周一", nil, topFacts[:1], topIDs[:1], 5, nil)
	if tr2.Hit || tr2.HitAt1 || tr2.HasIDs {
		t.Errorf("top-1 不含答案应全 false: %+v", tr2)
	}
	// topk=1 时即使答案在 top-2 也不命中。
	tr3 := JudgeM1("客户A的偏好是周一", nil, topFacts, topIDs, 1, nil)
	if tr3.Hit || tr3.HitAt1 {
		t.Errorf("topk=1 时 top-2 答案不应命中: %+v", tr3)
	}
}
