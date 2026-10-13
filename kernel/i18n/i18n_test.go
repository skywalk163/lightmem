// kernel/i18n/i18n_test.go — R8-A（P7-E）i18n 冒烟测试。
//
// 覆盖（任务书 §A.3.3）：
//   - 三语翻译正确性；
//   - 回退链（未知 locale / 未知 key / 未知 Locale 枚举值）；
//   - 参数插值；
//   - 确定性（同输入恒等输出）；
//   - 翻译表自检（三语占位符一致性、非空性）。
package i18n

import (
	"strings"
	"testing"
)

func TestResolveBasic(t *testing.T) {
	cases := []struct {
		in   string
		want Locale
	}{
		{"", LocaleZH},           // 空串回退默认
		{"   ", LocaleZH},        // 空白回退默认
		{"zh-CN", LocaleZH},      // 规范标签
		{"en-US", LocaleEN},      // 规范标签
		{"ja-JP", LocaleJA},      // 规范标签
		{"zh", LocaleZH},         // 别名
		{"en", LocaleEN},         // 别名
		{"ja", LocaleJA},         // 别名
		{"中文", LocaleZH},         // 中文别名
		{"简体中文", LocaleZH},       // 中文别名
		{"日本語", LocaleJA},        // 日文别名
		{"EN-US", LocaleEN},      // 大小写不敏感
		{"zh-TW", LocaleZH},      // 前缀匹配
		{"en-GB", LocaleEN},      // 前缀匹配
		{"fr-FR", LocaleUnknown}, // 未支持语言
		{"klingon", LocaleUnknown},
	}
	for _, c := range cases {
		if got := Resolve(c.in); got != c.want {
			t.Errorf("Resolve(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTagRoundTrip(t *testing.T) {
	// 规范标签往返。
	for _, l := range []Locale{LocaleZH, LocaleEN, LocaleJA} {
		tag := l.Tag()
		if Resolve(tag) != l {
			t.Errorf("Tag roundtrip failed for %v (tag=%q -> %v)", l, tag, Resolve(tag))
		}
	}
	// 未知枚举值规范化。
	if Locale(99).Normalize() != LocaleZH {
		t.Errorf("Locale(99).Normalize() should be zh, got %v", Locale(99).Normalize())
	}
	if LocaleUnknown.IsValid() {
		t.Error("LocaleUnknown should not be valid")
	}
	if !LocaleZH.IsValid() {
		t.Error("LocaleZH should be valid")
	}
}

func TestTranslateThreeLangs(t *testing.T) {
	// lifecycle 错误消息三语。
	// 关键决策（任务书 A.1 优先）：zh-CN 栏 = 真正的中文本地化（默认 locale 中文），
	//           en-US 栏 = 英文，ja-JP 栏 = 日文，三语相互独立、逐语本地化。
	// kernel 既有错误消息保持英文硬编码不动（红线「只增不改」），i18n 化通过新增入口实现。
	args := []any{"sqlite", "L2", "L3", "io error"}
	zh := T(LocaleZH, MsgLifecycleUpdateFailed, args...)
	en := T(LocaleEN, MsgLifecycleUpdateFailed, args...)
	ja := T(LocaleJA, MsgLifecycleUpdateFailed, args...)
	if !strings.Contains(zh, "生命周期") {
		t.Errorf("zh should be real Chinese localization: %q", zh)
	}
	if !strings.Contains(en, "[lifecycle] update") {
		t.Errorf("en missing '[lifecycle] update': %q", en)
	}
	if !strings.Contains(ja, "ライフサイクル") {
		t.Errorf("ja missing ライフサイクル: %q", ja)
	}
	// zh 必须 ≠ en（默认 locale 中文 ≠ 英文）。
	if zh == en {
		t.Errorf("zh(%q) should NOT equal en(%q): default locale must be Chinese, not English", zh, en)
	}
	// 三语插值均生效（不应残留 %s）。
	for name, s := range map[string]string{"zh": zh, "en": en, "ja": ja} {
		if strings.Contains(s, "%s") || strings.Contains(s, "%v") {
			t.Errorf("%s interpolation failed: %q", name, s)
		}
	}
}

func TestTranslateFallbackChain(t *testing.T) {
	// 1. 未知 Locale 枚举值 → 回退 zh（zh 栏 = 真中文，默认 locale 中文）。
	if got := T(Locale(999), MsgMemoryNotFound, "m1"); !strings.Contains(got, "未找到") {
		t.Errorf("unknown Locale should fallback to zh (Chinese), got %q", got)
	}
	// 2. 未知 key → 返回 key 本身（不为空串、不 panic）。
	unknownKey := MsgKey("__no_such_key__")
	if got := T(LocaleEN, unknownKey); got != string(unknownKey) {
		t.Errorf("unknown key should return key itself, got %q", got)
	}
	// 3. 空 args → 原模板（无插值需求）。
	if got := T(LocaleEN, MsgDbNotConnected); !strings.Contains(got, "db not connected") {
		t.Errorf("empty args should return template, got %q", got)
	}
	// 4. args 数量多于占位符 → 不 panic，模板可用。
	if got := T(LocaleZH, MsgMemoryNotFound, "m1", "extra"); !strings.Contains(got, "m1") {
		t.Errorf("extra args should not break interpolation, got %q", got)
	}
}

func TestTranslateNoArgs(t *testing.T) {
	// 无参数消息三语正确（zh = 真中文）。
	if got := T(LocaleZH, MsgDbNotConnected); got != "数据库未连接" {
		t.Errorf("zh db_not_connected = %q", got)
	}
	if got := T(LocaleEN, MsgDbNotConnected); got != "db not connected" {
		t.Errorf("en db_not_connected = %q", got)
	}
	if got := T(LocaleJA, MsgDbNotConnected); got != "データベース未接続" {
		t.Errorf("ja db_not_connected = %q", got)
	}
	// zh 必须 ≠ en（默认 locale 中文 ≠ 英文）。
	if T(LocaleZH, MsgDbNotConnected) == T(LocaleEN, MsgDbNotConnected) {
		t.Error("default locale invariant broken: zh == en for db_not_connected")
	}
}

func TestDeterminism(t *testing.T) {
	// 同输入多跑，结果恒等（确定性）。
	args := []any{"sqlite", "L2", "L3", "err"}
	first := T(LocaleEN, MsgLifecycleUpdateFailed, args...)
	for i := 0; i < 100; i++ {
		if got := T(LocaleEN, MsgLifecycleUpdateFailed, args...); got != first {
			t.Fatalf("non-deterministic at iter %d: %q vs %q", i, got, first)
		}
	}
	// Resolve 同样确定。
	r0 := Resolve("EN-us")
	for i := 0; i < 100; i++ {
		if Resolve("EN-us") != r0 {
			t.Fatal("Resolve non-deterministic")
		}
	}
}

func TestTranslationTableIntegrity(t *testing.T) {
	keys := SupportedKeys()
	if len(keys) < 20 {
		t.Errorf("translation table too small: %d entries (want >=20)", len(keys))
	}
	if Count() != len(keys) {
		t.Errorf("Count()=%d != len(SupportedKeys)=%d", Count(), len(keys))
	}
	// 每条消息三语非空 + 占位符一致。
	for _, k := range keys {
		zh := T(LocaleZH, k)
		en := T(LocaleEN, k)
		ja := T(LocaleJA, k)
		if zh == "" || en == "" || ja == "" {
			t.Errorf("empty translation for %s: zh=%q en=%q ja=%q", k, zh, en, ja)
		}
		// 占位符数量三语一致（用计数）。
		if phCount(zh) != phCount(en) || phCount(zh) != phCount(ja) {
			t.Errorf("placeholder mismatch for %s: zh=%d en=%d ja=%d", k, phCount(zh), phCount(en), phCount(ja))
		}
		// 三语相互独立、逐语本地化：zh != en、ja != en、ja != zh。
		// （默认 locale 中文，zh 栏必须是真中文，而非 kernel 英文原文。）
		if zh == en {
			t.Errorf("zh not localized for %s: zh(%q) == en(%q)", k, zh, en)
		}
		if ja == en {
			t.Errorf("ja not localized for %s: ja(%q) == en(%q)", k, ja, en)
		}
		if ja == zh {
			t.Errorf("ja not localized for %s: ja(%q) == zh(%q)", k, ja, zh)
		}
	}
}

// phCount 统计占位符数量（%s / %v / %d / %q / %w / %f / %t 等）。
func phCount(s string) int {
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+1 < len(s) {
			// 跳过 %% 转义。
			if s[i+1] == '%' {
				i++
				continue
			}
			count++
			i++ // 跳过格式符（宽泛）
		}
	}
	return count
}

func TestConvenienceHelpers(t *testing.T) {
	// zh 栏 = 真中文（默认 locale 中文）。
	if !strings.Contains(TZH(MsgMemoryNotFound, "m1"), "未找到") {
		t.Error("TZH failed (should return Chinese)")
	}
	if !strings.Contains(TEN(MsgMemoryNotFound, "m1"), "memory") {
		t.Error("TEN failed")
	}
	if !strings.Contains(TJA(MsgMemoryNotFound, "m1"), "メモリ") {
		t.Error("TJA failed")
	}
	if !HasKey(MsgDbNotConnected) {
		t.Error("HasKey(MsgDbNotConnected) = false, want true")
	}
	if HasKey(MsgKey("__nope__")) {
		t.Error("HasKey(unknown) = true, want false")
	}
	if LocaleOf("ja") != LocaleJA {
		t.Error("LocaleOf('ja') != LocaleJA")
	}
}
