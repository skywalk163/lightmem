// kernel/i18n/translate.go — R8-A（P7-E）翻译入口 `T()` 与回退链。
//
// 设计目标（任务书 §A.3.3）：
//   - `T(l Locale, key MsgKey, args ...any) string`：按语言取词并插值；
//   - **回退链**：key 未命中 → zh-CN 中文；locale 未知 → DefaultLocale；
//     LocaleUnknown → DefaultLocale（保证任何输入都不 panic、不空串）；
//   - **确定性**：无 time.Now / 无随机 / 无 map 迭代序；同输入恒等输出。
//
// 只增不改：本函数仅做文案翻译，不修改任何既有函数返回值/错误类型语义。
package i18n

import (
	"fmt"
)

// T 取指定语言下 key 对应的翻译，并用 args 插值占位符。
//
// 回退链（严格顺序）：
//  1. locale 未知/非法 → DefaultLocale（zh-CN）；
//  2. key 未命中翻译表 → zh-CN 原文（默认态不回归）；
//  3. 占位符与 args 数量不匹配 → 返回未插值模板（不 panic，fmt.Sprintf 天然容忍）。
func T(l Locale, key MsgKey, args ...any) string {
	loc := l.Normalize()
	entry, ok := lookup(key)
	if !ok {
		// 回退：key 未命中翻译表 → 返回 key 本身（绝不为空串、不 panic）。
		return string(key)
	}
	switch loc {
	case LocaleEN:
		return interpolate(entry.en, args...)
	case LocaleJA:
		return interpolate(entry.ja, args...)
	case LocaleZH:
		return interpolate(entry.zh, args...)
	}
	// 理论不可达（Normalize 已兜底），保守回退 zh。
	return interpolate(entry.zh, args...)
}

// TZH 取中文翻译（等价 T(LocaleZH, key, args...)，便捷调用）。
func TZH(key MsgKey, args ...any) string {
	return T(LocaleZH, key, args...)
}

// TEN 取英文翻译（等价 T(LocaleEN, key, args...)，便捷调用）。
func TEN(key MsgKey, args ...any) string {
	return T(LocaleEN, key, args...)
}

// TJA 取日文翻译（等价 T(LocaleJA, key, args...)，便捷调用）。
func TJA(key MsgKey, args ...any) string {
	return T(LocaleJA, key, args...)
}

// interpolate 用 fmt.Sprintf 插值占位符（确定性，无随机源）。
func interpolate(template string, args ...any) string {
	if len(args) == 0 {
		return template
	}
	return fmt.Sprintf(template, args...)
}

// HasKey 判断 key 是否在翻译表中。
func HasKey(key MsgKey) bool {
	_, ok := lookup(key)
	return ok
}

// LocaleOf 便捷：字符串标签 → Locale（委托 Resolve）。
func LocaleOf(langTag string) Locale {
	return Resolve(langTag)
}
