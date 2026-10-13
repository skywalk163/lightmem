// R7-B v2 多视图生成器单元测试（确定性，无网络/无检索）。
package main

import (
	"testing"
)

func TestParseQueryStruct(t *testing.T) {
	cases := []struct {
		in       string
		wantBody string
		wantAnch string
	}{
		{"客户A的偏好是什么（周一）", "客户A的偏好是什么", "周一"},
		{"客户F偏好什么时间开会（决策人：决策人000）", "客户F偏好什么时间开会", "决策人：决策人000"},
		{"客户A把会议改到周一上午（2026-10-15 生效）", "客户A把会议改到周一上午", "2026-10-15 生效"},
		{"客户A的偏好是什么", "客户A的偏好是什么", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		got := parseQueryStruct(c.in)
		if got.Body != c.wantBody || got.Anchor != c.wantAnch {
			t.Errorf("parseQueryStruct(%q) = {body=%q anch=%q}, want {body=%q anch=%q}",
				c.in, got.Body, got.Anchor, c.wantBody, c.wantAnch)
		}
	}
}

func TestGenerateViewV1(t *testing.T) {
	// 有锚：锚前置
	qs := parseQueryStruct("客户A的偏好是什么（周一）")
	got := generateViewV1(qs)
	want := "周一：客户A的偏好是什么"
	if got != want {
		t.Errorf("v1 = %q, want %q", got, want)
	}
	// 无锚：回退 body
	qs2 := parseQueryStruct("客户A的偏好是什么")
	got2 := generateViewV1(qs2)
	if got2 != "客户A的偏好是什么" {
		t.Errorf("v1 no-anchor = %q, want body", got2)
	}
}

func TestGenerateViewV2(t *testing.T) {
	qs := parseQueryStruct("客户A的偏好是什么（周一）")
	got := generateViewV2(qs)
	want := "客户A（周一）"
	if got != want {
		t.Errorf("v2 = %q, want %q", got, want)
	}
	// multi-hop
	qs2 := parseQueryStruct("客户F偏好什么时间开会（决策人：决策人000）")
	got2 := generateViewV2(qs2)
	// entity 应抽出 "客户F"
	want2 := "客户F（决策人：决策人000）"
	if got2 != want2 {
		t.Errorf("v2 multihop = %q, want %q", got2, want2)
	}
}

func TestGenerateViewV3(t *testing.T) {
	facts := []string{"事实一", "事实二", "事实三"}
	got := generateViewV3(facts, 1)
	if len(got) != 1 || got[0] != "事实一" {
		t.Errorf("v3 k=1 = %v, want [事实一]", got)
	}
	got3 := generateViewV3(facts, 3)
	if len(got3) != 3 {
		t.Errorf("v3 k=3 len = %d, want 3", len(got3))
	}
	// 空 facts
	empty := generateViewV3(nil, 1)
	if len(empty) != 0 {
		t.Errorf("v3 empty = %v, want []", empty)
	}
}

func TestComputeOverlapV2(t *testing.T) {
	a := map[string][]string{
		"q1": {"id1", "id2", "id3"},
		"q2": {"id4", "id5"},
	}
	b := map[string][]string{
		"q1": {"id1", "id3", "id6"},
		"q2": {"id4"},
	}
	// q1: union=4 (id1,id2,id3,id6), intersect=2 (id1,id3) -> 池化
	// q2: union=2 (id4,id5), intersect=1 (id4)
	// 总池化: intersect=3, union=6 -> 3/6 = 0.5
	got := computeOverlapV2(a, b)
	want := 3.0 / 6.0
	if abs(got-want) > 1e-6 {
		t.Errorf("overlap = %.4f, want %.4f", got, want)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
