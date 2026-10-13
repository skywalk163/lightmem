package main

import "testing"

// R6-C 回归锁定：历史 3 条残留 FP（P4）必须被审计层 veto；真命中变体不得误伤。

const r6cRefusalExpected = "The information provided is not enough. You mentioned starting the role as Senior Software Engineer but not Software Engineer Manager."

// FP-1/2（lme-031748ae_abs 历史态）：expected 拒绝式，got 是肯定答案 → veto。
func TestR6C_FPVeto_NegationConflict(t *testing.T) {
	got := "How many engineers do I lead when I just started my new role as Senior Software Engineer? => When you just started your new role as Senior Software Engineer, you led 4 engineers. Now, you lead 5 engineers."
	vetoed, rule := FPVeto(r6cRefusalExpected, got)
	if !vetoed || rule != "negation_conflict" {
		t.Fatalf("历史否定冲突 FP 未被 veto: vetoed=%v rule=%q", vetoed, rule)
	}
}

// 同 expected 的真命中：取到的就是那条拒绝记忆本身 → 不得误伤。
func TestR6C_FPVeto_RefusalTrueHit(t *testing.T) {
	got := "How many engineers do I lead when I just started my new role as Software Engineer Manager? => The information provided is not enough. You mentioned starting the role as Senior Software Engineer but not Software Engineer Manager."
	if vetoed, rule := FPVeto(r6cRefusalExpected, got); vetoed {
		t.Fatalf("真命中被误 veto: rule=%q", rule)
	}
}

// FP-3（lc-conv-49-D1_16 历史态）：极短 expected + 无关对话碎片 → fragment veto。
func TestR6C_FPVeto_Fragment(t *testing.T) {
	exp := "friend's advice"
	got := "Sam: Hey Evan! I'm really getting into this healthier lifestyle—just took my friends on an epic hiking trip last Friday!"
	vetoed, rule := FPVeto(exp, got)
	if !vetoed || rule != "fragment" {
		t.Fatalf("历史碎片化 FP 未被 veto: vetoed=%v rule=%q", vetoed, rule)
	}
}

// 带 => 结构锚定的短事实 → 不触发碎片化 veto。
func TestR6C_FPVeto_AnchoredShortFact(t *testing.T) {
	got := "How did Evan get into watercolor painting? => friend's advice"
	if vetoed, rule := FPVeto("friend's advice", got); vetoed {
		t.Fatalf("结构锚定短事实被误 veto: rule=%q", rule)
	}
}

// 普通肯定问答命中 → 不触发任何 veto。
func TestR6C_FPVeto_PlainHit(t *testing.T) {
	exp := "Paris is the capital of France."
	got := "What is the capital of France? => Paris is the capital of France."
	if vetoed, rule := FPVeto(exp, got); vetoed {
		t.Fatalf("普通命中被误 veto: rule=%q", rule)
	}
}

// FPAnswerSide：Q=>A 格式取回答侧；无 => 整体返回。
func TestR6C_FPAnswerSide(t *testing.T) {
	if got := FPAnswerSide("Q => A text"); got != "A text" {
		t.Fatalf("Q=>A 回答侧提取错误: %q", got)
	}
	if got := FPAnswerSide("plain fact"); got != "plain fact" {
		t.Fatalf("无 => 应整体返回: %q", got)
	}
}

// IsRefusalExpected：拒绝式识别边界。
func TestR6C_IsRefusalExpected(t *testing.T) {
	if len(IsRefusalExpected(r6cRefusalExpected)) == 0 {
		t.Fatal("拒绝式 expected 未识别")
	}
	if len(IsRefusalExpected("Paris is the capital of France.")) != 0 {
		t.Fatal("普通 expected 被误判为拒绝式")
	}
}

// 时序型短答案（"July 2023"）走 matchFactTemporal 合法命中，不得被碎片化护栏误伤。
func TestR6C_FPVeto_TemporalExempt(t *testing.T) {
	cases := []struct{ exp, got string }{
		{"July 2023", "Dave: I booked a trip to a mountainous region for next month! Gonna be amazing!"},
		{"in 2022", "Jolene: Sorry about your loss. My mother also passed away last year."},
		{"on 9 April, 2023", "Deborah: Hey Jolene! Had a blast biking nearby with my neighbor last week."},
	}
	for _, c := range cases {
		if vetoed, rule := FPVeto(c.exp, c.got); vetoed {
			t.Fatalf("时序合法命中被误 veto: exp=%q rule=%q", c.exp, rule)
		}
	}
}
