// kernel/i18n/locale.go — R8-A（P7-E）多语言国际化：Locale 类型与确定性解析。
//
// 设计目标（任务书 A.1/A.2）：
//   - **确定性**：无 time.Now / 无随机 / 无 map 迭代序；同输入恒等输出。
//   - **默认 locale 中文（任务书 A.1）**：默认 locale = zh-CN，`T(zh-CN, key)`
//     输出**真正的中文本地化**；kernel 既有英文硬编码**保持不动**（红线「只增
//     不改」），i18n 化通过新增入口实现，故默认态行为逐位不回归。
//   - **不调 LLM**：翻译表是静态词典（messages.go），不做任何机器翻译。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package i18n

import (
	"strings"
)

// Locale 语言标签（确定性枚举，替代任意字符串，避免拼写漂移）。
type Locale int

const (
	// LocaleUnknown 未知/未识别（回退到 zh-CN 默认）。
	LocaleUnknown Locale = iota
	// LocaleZH 简体中文（默认）。
	LocaleZH
	// LocaleEN 英语（本地化示例）。
	LocaleEN
	// LocaleJA 日语（本地化示例）。
	LocaleJA
)

// DefaultLocale 默认语言（中文，任务书 A.1）。
const DefaultLocale = LocaleZH

// validLocales 按 Locale 索引的规范标签（顺序与 Locale 枚举一致，确定性）。
var validLocales = []string{
	"unknown", // LocaleUnknown
	"zh-CN",   // LocaleZH
	"en-US",   // LocaleEN
	"ja-JP",   // LocaleJA
}

// localeAliases 别名表（确定性：非 map，用 slice 顺序线性查找，
// 避免 map 迭代序/哈希随机源污染确定性）。
type localeAlias struct {
	alias string
	l     Locale
}

var localeAliases = []localeAlias{
	{"zh", LocaleZH},
	{"zh-cn", LocaleZH},
	{"zh-CN", LocaleZH},
	{"cn", LocaleZH},
	{"chinese", LocaleZH},
	{"简体中文", LocaleZH},
	{"中文", LocaleZH},
	{"en", LocaleEN},
	{"en-us", LocaleEN},
	{"en-US", LocaleEN},
	{"english", LocaleEN},
	{"ja", LocaleJA},
	{"ja-jp", LocaleJA},
	{"ja-JP", LocaleJA},
	{"japanese", LocaleJA},
	{"日本語", LocaleJA},
}

// Resolve 把语言标签字符串确定性解析为 Locale。
//
// 规则（确定性，无随机源）：
//   - 空串 / 纯空白 → DefaultLocale（zh-CN）；
//   - 精确匹配规范标签（zh-CN/en-US/ja-JP）→ 对应 Locale；
//   - 大小写/别名匹配（zh/en/ja/english/中文…）→ 对应 Locale；
//   - 前缀匹配（如 "zh-TW"、"en-GB" → 主语言 zh/en）→ 对应 Locale；
//   - 其余 → LocaleUnknown（调用方回退 DefaultLocale）。
func Resolve(langTag string) Locale {
	s := strings.TrimSpace(langTag)
	if s == "" {
		return DefaultLocale
	}
	// 1. 精确规范标签
	for _, a := range validLocales {
		if s == a {
			return localeByIndex(indexOfLocale(a))
		}
	}
	// 2. 别名
	for _, a := range localeAliases {
		if s == a.alias {
			return a.l
		}
	}
	// 3. 大小写不敏感别名（规范化后线性比对）
	lower := strings.ToLower(s)
	for _, a := range localeAliases {
		if lower == strings.ToLower(a.alias) {
			return a.l
		}
	}
	// 4. 主语言前缀（zh-* / en-* / ja-*）
	switch {
	case strings.HasPrefix(lower, "zh"):
		return LocaleZH
	case strings.HasPrefix(lower, "en"):
		return LocaleEN
	case strings.HasPrefix(lower, "ja"):
		return LocaleJA
	}
	return LocaleUnknown
}

// localeByIndex 辅助：从 validLocales 索引反查 Locale 枚举值。
func localeByIndex(i int) Locale {
	switch i {
	case 1:
		return LocaleZH
	case 2:
		return LocaleEN
	case 3:
		return LocaleJA
	}
	return LocaleUnknown
}

// indexOfLocale 返回规范标签在 validLocales 中的下标（不存在返回 -1）。
func indexOfLocale(tag string) int {
	for i, s := range validLocales {
		if s == tag {
			return i
		}
	}
	return -1
}

// IsValid 判断 Locale 是否为有效语言（非 Unknown）。
func (l Locale) IsValid() bool {
	return l == LocaleZH || l == LocaleEN || l == LocaleJA
}

// Tag 返回 Locale 的规范语言标签（未知回退 DefaultLocale）。
func (l Locale) Tag() string {
	if int(l) >= 0 && int(l) < len(validLocales) {
		return validLocales[l]
	}
	return validLocales[DefaultLocale]
}

// Normalize 规范化：未知 → DefaultLocale；已知 → 原样（确定性幂等）。
func (l Locale) Normalize() Locale {
	if !l.IsValid() {
		return DefaultLocale
	}
	return l
}

// String 返回 Locale 的可读名（调试/日志用，确定性）。
func (l Locale) String() string {
	switch l {
	case LocaleZH:
		return "简体中文"
	case LocaleEN:
		return "English"
	case LocaleJA:
		return "日本語"
	}
	return "未知"
}
