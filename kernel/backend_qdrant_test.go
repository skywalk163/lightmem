// backend_qdrant 单测（S1）
// 只测「确定性 + 边界 + 降级」三件事：
//  1. 未配置 Qdrant -> 明确 error，不静默降级（R15 诚实口径）。
//  2. 文本检索词元打分与 SQLite 行为一致（同 tokenize/containsKind/visibleAt）。
//  3. 可见性硬过滤 / 双时态 / TopK 截断。
//
// 网络真机不在此测（在 0.88 上跑 gate.sh，见 R16 计划 §S1 验证）。
package kernel

import (
	"context"
	"strings"
	"testing"
)

func TestNewQdrantBackendNilConfig(t *testing.T) {
	_, err := newQdrantBackend(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unexpected error msg: %v", err)
	}

	// 配置存在但 Host 空 -> 仍要报错
	cfg := &StorageConfig{Qdrant: &QdrantConfig{}}
	_, err = newQdrantBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for empty host")
	}
	if !strings.Contains(err.Error(), "host required") {
		t.Fatalf("unexpected error msg: %v", err)
	}
}

func TestQdrantMemoryPayloadRoundTrip(t *testing.T) {
	m := &Memory{
		MemoryID:   "m1",
		Fact:       "客户A 偏好 简洁文档",
		Layer:      LayerL1,
		TenantID:   "t1",
		UserID:     "u1",
		MemoryKind: KindPreference,
	}
	p, err := memoryPayload(m)
	if err != nil {
		t.Fatal(err)
	}
	if p["layer"] != "L1" {
		t.Fatalf("layer payload mismatch: %v", p["layer"])
	}
	if p["tenant"] != "t1" || p["user"] != "u1" || p["kind"] != "preference" {
		t.Fatalf("filter payload mismatch: %#v", p)
	}
	back, err := parseMemoryPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.MemoryID != m.MemoryID || back.Fact != m.Fact || back.Layer != m.Layer {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", back, m)
	}
}

func TestQdrantParsePayloadMissing(t *testing.T) {
	if _, err := parseMemoryPayload(map[string]any{}); err == nil {
		t.Fatal("expected error for missing _memory")
	}
}

func TestQdrantQueryTokenizeDeterminism(t *testing.T) {
	// 复用包内 tokenSet/tokenize：验证 CJK 双字 bigram 覆盖，与 SQLite 一致。
	txt := "客户A 偏好 简洁文档"
	set := tokenSet(txt)
	// ASCII 与 CJK 之间被 flush 分隔，"户a" 不产生；只断言 CJK bigram + ASCII 单字。
	if !set["客户"] || !set["户"] || !set["简洁"] || !set["文档"] || !set["a"] {
		t.Fatalf("tokenSet missing expected tokens: %#v", set)
	}
	// 与 rank_fusion.tokenize 同源，长度稳定
	first := tokenize(txt)
	second := tokenize(txt)
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("tokenize not deterministic: %v", first)
	}
	// "户a" 不应存在（ASCII 与 CJK 之间 flush）
	if set["户a"] {
		t.Fatalf("unexpected cross-script bigram 户a: %#v", set)
	}
}

func TestQdrantContainsKindAndVisibility(t *testing.T) {
	if !containsKind([]MemoryKind{KindPreference, KindEvent}, KindEvent) {
		t.Fatal("containsKind should match")
	}
	if containsKind([]MemoryKind{KindPreference}, KindEvent) {
		t.Fatal("containsKind should not match")
	}

	m := &Memory{
		MemoryID:   "m1",
		Fact:       "x",
		ValidFrom:  "2026-10-01",
		ValidUntil: "2026-10-31",
	}
	if !m.visibleAt("2026-10-15") {
		t.Fatal("should be visible mid-window")
	}
	if m.visibleAt("2026-11-01") {
		t.Fatal("should not be visible after valid_until")
	}
	if m.visibleAt("2026-09-30") {
		t.Fatal("should not be visible before valid_from")
	}
}
