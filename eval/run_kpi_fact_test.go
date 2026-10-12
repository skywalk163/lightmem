//go:build ignore

// R4-C：fact 口径 v3/v4 确定性匹配单测。
//
// 运行方式（与 run_kpi.go 同为 //go:build ignore，需显式列文件编译）：
//
//	cd eval && go test run_kpi.go run_kpi_fact_test.go
//
// 覆盖（任务书 §3 C.3）：
//  1. utf8.RuneCountInString 门槛 bug（中文 15 rune 边界 / 英文 15 字符边界）；
//  2. matchFactV3 中文 / 英文 / 混合样本；
//  3. 数值 / 日期归一；
//  4. 时间推理（锚点 occurred_at + 相对时间词）；
//  5. beam fp 过滤（isTemplateExpected）；
//  6. v4 短词元包含 / 候选扩展（matchFactTop5）。
//
// 全部为纯字符串与日历推算断言，不调 LLM，无随机源。
package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lightmem/kernel"
)

// withThreshold 临时设置 factV3Threshold 并返回还原函数（用例隔离）。
func withThreshold(t *testing.T, thr float64) {
	t.Helper()
	old := factV3Threshold
	factV3Threshold = thr
	t.Cleanup(func() { factV3Threshold = old })
}

// withNormalize 临时设置 R5-B fact 边界归一化开关（-fact-normalize）并注册还原（用例隔离）。
// 用于把「边界归一化 + 边界对齐护栏」这一路增益与既有 v2/v3/v4 路径隔离。
func withNormalize(t *testing.T, on bool) {
	t.Helper()
	old := factNormalizeEnabled
	factNormalizeEnabled = on
	t.Cleanup(func() { factNormalizeEnabled = old })
}

// 1. RuneCountInString 门槛（中文 15 rune 边界 / 英文 15 字符边界）。
//
// 关键：把阈值压到 0.0，使「是否越过长度门槛」成为唯一变量——
// 若实现误用 Go len()（字节数）判长度，则 5 个中文字（15 字节）会被误判为
// 「≥15 字符」而进入相似度判定并命中，本用例即失败。
func TestMatchFactV3_RuneThreshold(t *testing.T) {
	withThreshold(t, 0.0)
	// 本用例只考察「长度门槛」这一变量，关闭 R5-B 边界归一化与护栏（两者正交）。
	withNormalize(t, false)

	// 中文 5 rune（=15 字节）：修复后应被门槛拒绝（RuneCount 5 < 15 边界）。
	shortCJK := "你好世界啊"  // 5 rune / 15 byte
	gotShort := "你好吗世界啊" // 插入 吗 破坏双向包含，但 trigram 仍有交集
	if matchFactV3(gotShort, shortCJK) {
		t.Fatalf("中文 5 rune 应被门槛拒绝（len(bytes)=%d, runes=%d）",
			len(shortCJK), utf8.RuneCountInString(shortCJK))
	}

	// 中文 14 rune：仍 < 15，应被拒绝（字节数 42 会骗过旧实现）。
	cjk14 := strings.Repeat("字", 14)
	gotCJK14 := "换" + strings.Repeat("字", 13) // 首字替换，破坏包含，保留 trigram 交集
	if matchFactV3(gotCJK14, cjk14) {
		t.Fatalf("中文 14 rune 应被门槛拒绝（runes=%d）", utf8.RuneCountInString(cjk14))
	}

	// 中文 15 rune：恰好越界，进入相似度判定 → 阈值 0.0 下命中。
	cjk15 := strings.Repeat("字", 15)
	gotCJK15 := "换" + strings.Repeat("字", 14)
	if !matchFactV3(gotCJK15, cjk15) {
		t.Fatalf("中文 15 rune 应越过门槛并命中（runes=%d）", utf8.RuneCountInString(cjk15))
	}

	// 英文 14 字符：< 15，拒绝。
	en14 := "abcdefghijklmn"
	if matchFactV3("x"+en14[1:], en14) {
		t.Fatalf("英文 14 字符应被门槛拒绝: %q", en14)
	}

	// 英文 15 字符：越界 → 命中。
	en15 := "abcdefghijklmno"
	if !matchFactV3("x"+en15[1:], en15) {
		t.Fatalf("英文 15 字符应越过门槛并命中: %q", en15)
	}
}

// 2. matchFactV3 中文 / 英文 / 混合样本。
func TestMatchFactV3_LangSamples(t *testing.T) {
	withThreshold(t, 0.55)

	cases := []struct {
		name string
		got  string
		exp  string
		want bool
	}{
		{"中文包含命中", "用户说他很喜欢武侠小说和科幻小说", "武侠小说", true},
		{"英文包含命中", "speaker: i bought a cooking book last week", "cooking book", true},
		{"混合包含命中", "Alice 说她买了一本 cooking book 作为礼物", "cooking book", true},
		{"英文 == 忽略大小写", "The Meeting Is At Noon", "the meeting is at noon", true},
		{"完全无关", "completely different unrelated text here", "quarterly revenue projection", false},
		{"空 expected", "some got text", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchFactV3(c.got, c.exp); got != c.want {
				t.Fatalf("matchFactV3(%q,%q)=%v, want %v", c.got, c.exp, got, c.want)
			}
		})
	}
}

// 3. 数值 / 日期归一。
func TestNormalizeFactNumbers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$3,750", "3750"},
		{"Price: $1,234,567", "price: 1234567"},
		{"€99", "99"},
		{"2023-04-01", "2023 04 01"},
		{"2023/4/1", "2023 4 1"},
		{"no numbers here", "no numbers here"},
	}
	for _, c := range cases {
		if got := normalizeFactNumbers(c.in); got != c.want {
			t.Fatalf("normalizeFactNumbers(%q)=%q, want %q", c.in, got, c.want)
		}
	}

	// 数值归一后应能被 matchFactV3 的包含判定覆盖（形态差被抹平）。
	// 阈值压到 1.0：v3 直接判定因形态差（$ / ,）必然 <1.0 而 miss，
	// 归一后两串相等 → matchFact 精确相等命中，隔离出「数值归一」这一路增益。
	withThreshold(t, 1.0)
	if !matchFactV4("the total is 3750", "the total is $3,750", nil) {
		t.Fatalf("数值归一后应命中: got=%q exp=%q", "the total is 3750", "the total is $3,750")
	}
}

// 4. 时间推理（锚点 occurred_at + 相对时间词）。
func TestMatchFactTemporal(t *testing.T) {
	apr9 := time.Date(2023, 4, 9, 0, 0, 0, 0, time.UTC)
	jul15 := time.Date(2023, 7, 15, 0, 0, 0, 0, time.UTC)
	y2022 := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		got    string
		exp    string
		anchor *time.Time
		want   bool
	}{
		{"月名日期 命中锚点", "we met on that day", "on 9 April, 2023", &apr9, true},
		{"月名日期 未命中", "we met on that day", "on 1 April, 2023", &apr9, false},
		{"月份粒度 命中", "it happened then", "July 2023", &jul15, true},
		{"年份粒度 命中", "it happened back then", "in 2022", &y2022, true},
		{"相对时间词 yesterday", "we met yesterday", "April 8, 2023", &apr9, true},
		{"相对时间词 tomorrow", "the event is tomorrow", "April 10, 2023", &apr9, true},
		{"锚点为空", "we met yesterday", "April 8, 2023", nil, false},
		{"无日期规格", "we met yesterday", "probably yes", &apr9, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchFactTemporal(c.got, c.exp, c.anchor); got != c.want {
				t.Fatalf("matchFactTemporal(%q,%q)=%v, want %v", c.got, c.exp, got, c.want)
			}
		})
	}

	// ISO 日期（YYYY-MM-DD）粒度解析。
	iso := time.Date(2022, 10, 30, 0, 0, 0, 0, time.UTC)
	if !matchFactTemporal("ended late that month", "2022-10-30", &iso) {
		t.Fatalf("ISO 日期应命中: exp=2022-10-30 anchor=%s", iso.Format("2006-01-02"))
	}
}

// 5. beam fp 过滤（isTemplateExpected）。
func TestIsTemplateExpected(t *testing.T) {
	cases := []struct {
		exp  string
		want bool
	}{
		{"LLM response should contain: the answer is 42", true},
		{"llm response should contain: ok", true},
		{"  LLM Response Should Contain: spaced  ", true},
		{"LLM response should state: there is contradictory information", true},
		{"LLM response should mention: the key point", true},
		{"a cooking book", false},
		{"the answer is 42", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isTemplateExpected(c.exp); got != c.want {
			t.Fatalf("isTemplateExpected(%q)=%v, want %v", c.exp, got, c.want)
		}
	}
}

// 6. v4 短词元包含 + 候选扩展。
func TestMatchFactV4ShortTokensAndTop5(t *testing.T) {
	withThreshold(t, 0.55)

	// 短 expected 的实义词元全部出现即命中（"a" 为停用词被忽略）。
	// got 故意不含子串 "a cooking book" 也不含 "cooking book"（非连续），
	// 排除 v2 包含 / v3 相似度的干扰，隔离出「短词元包含」这一路。
	got := "i love cooking and read a book"
	if !matchFactV4(got, "a cooking book", nil) {
		t.Fatalf("短词元包含应命中: got=%q exp=%q", got, "a cooking book")
	}
	if matchFactShortTokens("unrelated sentence", "cooking book") {
		t.Fatalf("无关文本不应命中短词元包含")
	}
	if matchFactShortTokens("任意中文内容", "武侠小说") {
		t.Fatalf("CJK 短词元包含应直接返回 false（避免误配）")
	}

	// 候选扩展：主口径 miss 时在 top-5 候选内命中（短 expected 才扩展）。
	topFacts := []string{"noise one", "noise two", "i read a cooking book for fun"}
	if !matchFactTop5(topFacts, "cooking book", nil) {
		t.Fatalf("候选扩展应在 top-5 内命中短 expected")
	}
	// >=15 rune 的 expected 不进入候选扩展（返回 false）。
	longExp := "this expected fact is clearly longer than fifteen runes"
	if matchFactTop5([]string{longExp}, longExp, nil) {
		t.Fatalf(">=15 rune 的 expected 不应进入候选扩展")
	}
}

// ==================================================================
// R5-B：fact 边界归一化（-fact-normalize）单测。
// 覆盖任务书 §2 B.3.4：数值 / 日期 / 单位 / 大小写 / 混合样本。
// ==================================================================

// 7. normalizeFactBoundary：数值 / 日期 / 单位 / 大小写 / 混合。
func TestNormalizeFactBoundary(t *testing.T) {
	cases := []struct{ in, want string }{
		// 数值：去货币符号 + 去千分位逗号（含多位千分位）。
		{"$3,750", "3750"},
		{"Price: $1,234,567", "price: 1234567"},
		{"€99", "99"},
		{"¥1,000", "1000"},
		// 日期：去分隔符（ISO / 斜杠两种形态统一为紧凑数字）。
		{"2023-04-01", "20230401"},
		{"2023/04/01", "20230401"},
		// 单位：大小写统一 + 去尾随点。
		{"3.5KG.", "3.5kg"},
		{"12GB.", "12gb"},
		{"60%", "60%"},
		// 大小写：整体小写。
		{"HELLO World", "hello world"},
		// 混合：数值 + 日期 + 单位 + 大小写同时出现。
		{"Total: $3,750 on 2023-04-01 (12GB.)", "total: 3750 on 20230401 (12gb)"},
		// 空白压缩。
		{"  a   b  ", "a b"},
	}
	for _, c := range cases {
		if got := normalizeFactBoundary(c.in); got != c.want {
			t.Fatalf("normalizeFactBoundary(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// 8. factBoundaryGuard：边界对齐护栏（CJK 放行 / 词元覆盖 / 碎片拒绝 / 单词元）。
func TestFactBoundaryGuard(t *testing.T) {
	cases := []struct {
		name string
		got  string
		exp  string
		want bool
	}{
		// CJK expected 无法拉丁词元化 → 直接放行。
		{"CJK 放行", "任意中文内容", "中文内容", true},
		// 多词元：需 ≥2 个实义词元整词出现。
		{"词元覆盖达标", "the total revenue for this quarter is up", "total revenue for this quarter", true},
		{"词元覆盖不足", "totally unrelated revenue stream", "total revenue for this quarter", false},
		// 单词元：需该词元整词出现。
		{"单词元命中", "we discussed revenue today", "revenue", true},
		{"单词元未命中", "we discussed profits today", "revenue", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := factBoundaryGuard(c.got, c.exp); got != c.want {
				t.Fatalf("factBoundaryGuard(%q,%q)=%v, want %v", c.got, c.exp, got, c.want)
			}
		})
	}
}

// 9. matchFactV3 在 -fact-normalize 开 / 关下的差异。
//   - 开：数值形态差被边界归一化抹平 → 命中；
//   - 关：模糊路径碎片误配被护栏拒绝 → 不命中。
func TestMatchFactV3_FactNormalizeSwitch(t *testing.T) {
	withThreshold(t, 1.0)

	// 数值/日期形态差：阈值 1.0 下 v3 直判必然 miss，边界归一化后应命中。
	gotNum, expNum := "the total is 3750", "the total is $3,750"
	withNormalize(t, true)
	if !matchFactV3(gotNum, expNum) {
		t.Fatalf("开启边界归一化后应命中: got=%q exp=%q", gotNum, expNum)
	}
	withNormalize(t, false)
	if matchFactV3(gotNum, expNum) {
		t.Fatalf("关闭边界归一化后形态差应 miss: got=%q exp=%q", gotNum, expNum)
	}

	// 护栏：模糊命中但边界不对齐（碎片误配）→ 开启时拒绝，关闭时命中。
	withThreshold(t, 0.5)
	gotFrag, expFrag := "abcdefghijklmnx", "abcdefghijklmnop"
	withNormalize(t, true)
	if matchFactV3(gotFrag, expFrag) {
		t.Fatalf("护栏应拒绝碎片误配: got=%q exp=%q", gotFrag, expFrag)
	}
	withNormalize(t, false)
	if !matchFactV3(gotFrag, expFrag) {
		t.Fatalf("关闭护栏后模糊相似度应命中: got=%q exp=%q", gotFrag, expFrag)
	}
}

// 10. multi-hop 主 / 跳板口径 MRR 的 rank 辅助（firstRank / mrrOfRanks）。
func TestFirstRankAndMRR(t *testing.T) {
	fused := []kernel.FusedResult{
		{MemoryID: "hop1-000"},
		{MemoryID: "noise"},
		{MemoryID: "hop2-000"},
	}
	// 主口径：期望最终答案 hop2-000，应命中 rank=3。
	if r := firstRank(fused, []string{"hop2-000"}); r != 3 {
		t.Fatalf("firstRank 主口径 = %d, want 3", r)
	}
	// 跳板口径：期望 hop1-000，应命中 rank=1。
	if r := firstRank(fused, []string{"hop1-000"}); r != 1 {
		t.Fatalf("firstRank 跳板口径 = %d, want 1", r)
	}
	// 未命中 rank=0。
	if r := firstRank(fused, []string{"absent"}); r != 0 {
		t.Fatalf("firstRank 未命中 = %d, want 0", r)
	}
	// MRR：[(3),(1),(0)] → (1/3 + 1 + 0)/3 = 0.444…。
	got := mrrOfRanks([]int{3, 1, 0})
	want := (1.0/3.0 + 1.0) / 3.0
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("mrrOfRanks = %v, want %v", got, want)
	}
	if m := mrrOfRanks(nil); m != 0 {
		t.Fatalf("空 rank 序列 MRR = %v, want 0", m)
	}
}
