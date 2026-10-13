// kernel/i18n_hook.go — R8-A（P7-E）kernel 级 i18n 接入样板（只增不改）。
//
// 设计目标（任务书 §A.3.2）：
//   - kernel 包通过本文件**只增**一个可选的 i18n 钩子，供 kernel 内关键
//     错误/日志/校验消息接入翻译；
//   - **默认 locale 中文（任务书 A.1 优先）**：`KernelLocale` 默认 `i18n.LocaleZH`，
//     zh 栏 = **真正的中文本地化**（见 kernel/i18n/messages.go），en/ja 栏分别
//     本地化，三语相互独立；
//   - **kernel 既有英文硬编码保持不动**（红线「只增不改」）：i18n 化通过**新增**
//     调用点实现，不修改既有函数返回值/错误类型/日志格式语义，避免行为回归；
//   - 上层可 `SetKernelLocale` 切语言，切回默认 zh 即输出中文本地化文案。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import "github.com/lightmem/kernel/i18n"

// kernelLocale 当前 kernel 语言（包私有，默认 zh，见 DefaultLocale）。
var kernelLocale = i18n.DefaultLocale

// KernelLocale 返回当前 kernel 语言（只读）。
func KernelLocale() i18n.Locale { return kernelLocale.Normalize() }

// SetKernelLocale 设置 kernel 语言（幂等；未知值规范化为默认 zh）。
// 仅在确实需要切换 kernel 文案语言时调用；默认调用方无需设置。
func SetKernelLocale(l i18n.Locale) {
	kernelLocale = l.Normalize()
}

// T 用当前 kernel 语言翻译消息 key 并插值 args。
//
// 用法示例（接入样板）：新增调用点，用 T 替换既有硬编码英文的**文案部分**，
// 不改既有函数签名/错误类型，从而避免行为回归：
//
//	return fmt.Errorf("%s", T(i18n.MsgScanFailed, layer, err))
//
// 或用于日志：
//
//	log.Printf("%s", T(i18n.MsgLifecycleUpdateFailed, m.MemoryID, to, err))
//
// 说明：T 返回的字符串已含完整文案，直接交给 fmt.Errorf / log.Printf 即可；
// 默认 zh 输出中文本地化文案，`SetKernelLocale(LocaleEN)` 可切回英文。
func T(key i18n.MsgKey, args ...any) string {
	return i18n.T(kernelLocale, key, args...)
}
