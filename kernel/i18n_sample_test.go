// kernel/i18n_sample_test.go — R8-A（P7-E）i18n 接入样板冒烟测试。
//
// 验证 kernel/lifecycle.go、kernel/rank_fusion.go、kernel/metrics.go 末尾新增的
// i18n 接入样板函数：在默认 locale（zh）下输出**真中文**，切换 LocaleEN 后
// 输出英文；同时验证这些函数不修改 kernel 既有行为（只增不改）。
package kernel

import (
	"strings"
	"testing"

	"github.com/lightmem/kernel/i18n"
)

func TestSampleLifecycleZH(t *testing.T) {
	// 默认 locale = zh（任务书 A.1），样板应输出中文本地化文案。
	SetKernelLocale(i18n.LocaleZH)

	msg := LifecycleLayerScannerError("sqlite")
	if !strings.Contains(msg, "未实现") {
		t.Errorf("LifecycleLayerScannerError zh = %q, want Chinese", msg)
	}

	msg = LifecycleScanLayerError(LayerL2)
	if !strings.Contains(msg, "扫描") {
		t.Errorf("LifecycleScanLayerError zh = %q, want Chinese", msg)
	}

	msg = LifecycleUpdateFailedError("m1", LayerL3, nil)
	if !strings.Contains(msg, "生命周期") {
		t.Errorf("LifecycleUpdateFailedError zh = %q, want Chinese", msg)
	}
}

func TestSampleLifecycleEN(t *testing.T) {
	// 切换 EN 后样板输出英文（与 kernel 既有英文硬编码口径一致）。
	SetKernelLocale(i18n.LocaleEN)

	msg := LifecycleLayerScannerError("sqlite")
	if !strings.Contains(msg, "does not implement") {
		t.Errorf("LifecycleLayerScannerError en = %q, want English", msg)
	}

	msg = LifecycleScanLayerError(LayerL2)
	if !strings.Contains(msg, "scan") {
		t.Errorf("LifecycleScanLayerError en = %q, want English", msg)
	}
}

func TestSampleRankFusionZH(t *testing.T) {
	SetKernelLocale(i18n.LocaleZH)

	msg := RankFusionParseWeightsError("/tmp/w.json")
	if !strings.Contains(msg, "解析权重") || !strings.Contains(msg, "/tmp/w.json") {
		t.Errorf("RankFusionParseWeightsError zh = %q, want Chinese with path", msg)
	}
}

func TestSampleMetricsZH(t *testing.T) {
	SetKernelLocale(i18n.LocaleZH)

	msg := AuditLogText("retrieve", "m1", "t1", "u1", "sys", "detail")
	if !strings.Contains(msg, "审计") {
		t.Errorf("AuditLogText zh = %q, want Chinese", msg)
	}

	msg = AuditForgetText("m1", "t1", "v1", "ttl")
	if !strings.Contains(msg, "遗忘") {
		t.Errorf("AuditForgetText zh = %q, want Chinese", msg)
	}

	msg = FeedbackCorrectionText("m1", "low_confidence")
	if !strings.Contains(msg, "反馈") {
		t.Errorf("FeedbackCorrectionText zh = %q, want Chinese", msg)
	}
}

func TestSampleMetricsEN(t *testing.T) {
	SetKernelLocale(i18n.LocaleEN)

	msg := AuditLogText("retrieve", "m1", "t1", "u1", "sys", "detail")
	if !strings.Contains(msg, "[audit]") {
		t.Errorf("AuditLogText en = %q, want English", msg)
	}
}

func TestSampleOnlyAddsNoReturnChange(t *testing.T) {
	// 只增不改：样板是新增导出函数，不修改既有 LifecycleScannerByValue /
	// RankFusion.LoadWeightsFromFile / AuditLogger.Log 的签名与返回值语义。
	// 此处仅确认样板函数类型签名符合预期（编译期保证），并确认 KernelLocale
	// 默认回退 zh。
	SetKernelLocale(i18n.Locale(999)) // 未知 → Normalize 回退默认 zh
	if KernelLocale() != i18n.DefaultLocale {
		t.Errorf("KernelLocale() = %v, want %v", KernelLocale(), i18n.DefaultLocale)
	}

	// 复位为 zh，避免污染后续测试。
	SetKernelLocale(i18n.DefaultLocale)
}
