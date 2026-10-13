package main

import (
	"strings"
	"testing"
)

// R6-C 数据面压缩规则锁定：确定性、不增、护栏。

// Q=>A 事实只保留回答侧。
func TestR6C_DataFact_QA(t *testing.T) {
	got := CompressDataFact("How many engineers do I lead? => When you just started, you led 4 engineers. Now, you lead 5 engineers.")
	want := "When you just started, you led 4 engineers. Now, you lead 5 engineers"
	if got != want {
		t.Fatalf("Q=>A 压缩错误:\n got=%q\nwant=%q", got, want)
	}
}

// 对话体 speaker 前缀剥离 + 呼语碎片跳过 + 首句核心块保留。
func TestR6C_DataFact_SpeakerFirstSentence(t *testing.T) {
	in := "Sam: Hey Evan! I'm really getting into this healthier lifestyle. Just took my friends on an epic hiking trip last Friday!"
	got := CompressDataFact(in)
	if strings.HasPrefix(got, "Hey Evan") {
		t.Fatalf("呼语碎片未剔除: %q", got)
	}
	if !strings.Contains(got, "healthier lifestyle") {
		t.Fatalf("核心句未保留: %q", got)
	}
	if len([]rune(got)) > dataFactMaxRunes {
		t.Fatalf("超上限: %q (%d runes)", got, len([]rune(got)))
	}
}

// 护栏：空输入返回空；压缩结果为空时回退原文。
func TestR6C_DataFact_Fallback(t *testing.T) {
	if got := CompressDataFact(""); got != "" {
		t.Fatalf("空输入应空: %q", got)
	}
	if got := CompressDataFact("   "); got != "" {
		t.Fatalf("空白输入应空: %q", got)
	}
}

// 数据面压缩不得比原文更长（护栏）。
func TestR6C_DataFact_NeverLonger(t *testing.T) {
	facts := []string{
		"Q => A short answer",
		"John: We held some events and got to meet people. It was amazing. Second sentence filler.",
		"客户A的偏好是周一（上午）",
		"",
	}
	for _, f := range facts {
		c := CompressDataFact(f)
		if len(c) > len(f) && f != "" {
			t.Fatalf("压缩后更长: %q -> %q", f, c)
		}
	}
}

// 批量账目：数据面压缩率为正。
func TestR6C_DataPlaneAccount(t *testing.T) {
	facts := []string{
		"How many engineers? => You led 4 engineers.",
		"Sam: Hey Evan! Getting into a healthier lifestyle. Took friends on a hiking trip last Friday!",
	}
	acct := CompressDataPlaneAccount(facts)
	if acct.TokensAfter >= acct.TokensBefore {
		t.Fatalf("压缩后 token 未下降: before=%d after=%d", acct.TokensBefore, acct.TokensAfter)
	}
	if acct.Reduction <= 0 {
		t.Fatalf("压缩率应 >0: %f", acct.Reduction)
	}
}
