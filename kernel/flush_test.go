package kernel

import (
	"reflect"
	"testing"
	"time"
)

// TestFlushReasonString 断言 FlushReason 的可读标签。
func TestFlushReasonString(t *testing.T) {
	cases := []struct {
		r    FlushReason
		want string
	}{
		{FlushSessionEnd, "session_end"},
		{FlushTopicSwitch, "topic_switch"},
		{FlushTaskDone, "task_done"},
		{FlushL0Capacity, "l0_capacity"},
		{FlushReason(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Fatalf("FlushReason(%d).String()=%q, want %q", int(c.r), got, c.want)
		}
	}
}

// TestFlushDefaultConfig 断言默认配置值（64 / 0.35 / 3）。
func TestFlushDefaultConfig(t *testing.T) {
	got := DefaultFlushConfig()
	want := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 3}
	if got != want {
		t.Fatalf("DefaultFlushConfig()=%+v, want %+v", got, want)
	}
}

// TestFlushSessionEndOnly 单独命中 SessionEnd 触发。
func TestFlushSessionEndOnly(t *testing.T) {
	sig := FlushSignal{SessionEnd: true, Items: []string{"m2", "m1"}}
	plan := DecideFlush(sig, DefaultFlushConfig())
	if !plan.Flush {
		t.Fatal("expected Flush=true")
	}
	want := []FlushReason{FlushSessionEnd}
	if !reflect.DeepEqual(plan.Reasons, want) {
		t.Fatalf("Reasons=%v, want %v", plan.Reasons, want)
	}
	if len(plan.HoldItems) != 0 {
		t.Fatalf("HoldItems should be empty for SessionEnd, got %v", plan.HoldItems)
	}
}

// TestFlushTopicSwitchOnly 单独命中 TopicSwitch 触发。
func TestFlushTopicSwitchOnly(t *testing.T) {
	sig := FlushSignal{
		PrevSnippet: "meeting about alpha",
		NewSnippet:  "dinner reservation tonight",
		Items:       []string{"m1"},
	}
	plan := DecideFlush(sig, DefaultFlushConfig())
	want := []FlushReason{FlushTopicSwitch}
	if !reflect.DeepEqual(plan.Reasons, want) {
		t.Fatalf("Reasons=%v, want %v", plan.Reasons, want)
	}
}

// TestFlushTaskDoneOnly 单独命中 TaskDone 触发。
func TestFlushTaskDoneOnly(t *testing.T) {
	sig := FlushSignal{TaskDone: true, Items: []string{"m1"}}
	plan := DecideFlush(sig, DefaultFlushConfig())
	want := []FlushReason{FlushTaskDone}
	if !reflect.DeepEqual(plan.Reasons, want) {
		t.Fatalf("Reasons=%v, want %v", plan.Reasons, want)
	}
}

// TestFlushL0CapacityOnly 单独命中 L0 容量阈值触发。
func TestFlushL0CapacityOnly(t *testing.T) {
	sig := FlushSignal{L0Count: 64, Items: []string{"m1"}}
	plan := DecideFlush(sig, DefaultFlushConfig())
	want := []FlushReason{FlushL0Capacity}
	if !reflect.DeepEqual(plan.Reasons, want) {
		t.Fatalf("Reasons=%v, want %v", plan.Reasons, want)
	}
}

// TestFlushNoTrigger 无任何触发时 Flush=false、Reasons 空、Items/HoldItems 非 nil 空切片。
func TestFlushNoTrigger(t *testing.T) {
	sig := FlushSignal{
		PrevSnippet: "meeting about alpha",
		NewSnippet:  "meeting about alpha and beta", // 高度相似，不应触发话题切换
		L0Count:     5,
		Items:       []string{"m1"},
	}
	plan := DecideFlush(sig, DefaultFlushConfig())
	if plan.Flush {
		t.Fatalf("expected Flush=false, got %+v", plan)
	}
	if len(plan.Reasons) != 0 {
		t.Fatalf("Reasons=%v, want empty", plan.Reasons)
	}
	if plan.Reasons == nil {
		t.Fatal("Reasons should be non-nil empty slice")
	}
}

// TestFlushMultiReasonsOrder 多触发并存时 Reasons 按枚举序稳定输出。
func TestFlushMultiReasonsOrder(t *testing.T) {
	sig := FlushSignal{
		SessionEnd:  true,
		TaskDone:    true,
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		L0Count:     100,
	}
	plan := DecideFlush(sig, DefaultFlushConfig())
	want := []FlushReason{FlushSessionEnd, FlushTopicSwitch, FlushTaskDone, FlushL0Capacity}
	if !reflect.DeepEqual(plan.Reasons, want) {
		t.Fatalf("Reasons=%v, want %v", plan.Reasons, want)
	}
}

// TestFlushL0CapacityBoundary L0Count==L0Limit 边界触发，L0Count==L0Limit-1 不触发。
func TestFlushL0CapacityBoundary(t *testing.T) {
	// 默认阈值 64
	if p := DecideFlush(FlushSignal{L0Count: 64}, DefaultFlushConfig()); !p.Flush {
		t.Fatal("L0Count == L0Limit should trigger")
	}
	if p := DecideFlush(FlushSignal{L0Count: 63}, DefaultFlushConfig()); p.Flush {
		t.Fatal("L0Count == L0Limit-1 should not trigger")
	}

	// 自定义阈值
	cfg := FlushConfig{L0Limit: 10, TopicSwitchOverlap: 0.35, KeepRecent: 3}
	if p := DecideFlush(FlushSignal{L0Count: 10}, cfg); !p.Flush {
		t.Fatal("custom L0Limit boundary should trigger")
	}
	if p := DecideFlush(FlushSignal{L0Count: 9}, cfg); p.Flush {
		t.Fatal("custom L0Limit-1 should not trigger")
	}
}

// TestFlushL0LimitFallback L0Limit<=0 回落默认 64；sig.L0Limit>0 优先于 cfg。
func TestFlushL0LimitFallback(t *testing.T) {
	// cfg.L0Limit<=0 回落默认 64
	cfg := FlushConfig{L0Limit: 0, TopicSwitchOverlap: 0.35, KeepRecent: 3}
	if p := DecideFlush(FlushSignal{L0Count: 64}, cfg); !p.Flush {
		t.Fatal("L0Limit<=0 should fallback to 64, boundary triggers")
	}
	if p := DecideFlush(FlushSignal{L0Count: 63}, cfg); p.Flush {
		t.Fatal("L0Limit<=0 should not trigger below 64")
	}

	// sig.L0Limit>0 优先于 cfg.L0Limit
	cfg2 := FlushConfig{L0Limit: 5, TopicSwitchOverlap: 0.35, KeepRecent: 3}
	if p := DecideFlush(FlushSignal{L0Count: 6, L0Limit: 10}, cfg2); p.Flush {
		t.Fatalf("sig.L0Limit should override cfg.L0Limit, got %+v", p)
	}
	if p := DecideFlush(FlushSignal{L0Count: 10, L0Limit: 10}, cfg2); !p.Flush {
		t.Fatal("sig.L0Limit override boundary should trigger")
	}
}

// TestFlushTopicSwitchOverlapThreshold 重叠率恰等于阈值时触发（<=），略大于阈值时不触发。
func TestFlushTopicSwitchOverlapThreshold(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 3}

	// 恰等于 0.35 = 7/20：触发。
	// a = {a1..a7, p1..p6}，b = {a1..a7, q1..q7}
	// intersection = 7, union = 7+6+7 = 20, jaccard = 7/20 = 0.35
	a := "a1 a2 a3 a4 a5 a6 a7 p1 p2 p3 p4 p5 p6"
	b := "a1 a2 a3 a4 a5 a6 a7 q1 q2 q3 q4 q5 q6 q7"
	if ov := TopicFingerprintOverlap(a, b); ov != 0.35 {
		t.Fatalf("overlap at threshold=%v, want 0.35", ov)
	}
	sig := FlushSignal{PrevSnippet: a, NewSnippet: b}
	if p := DecideFlush(sig, cfg); !p.Flush || len(p.Reasons) != 1 || p.Reasons[0] != FlushTopicSwitch {
		t.Fatalf("at threshold should trigger topic switch only, got %+v", p)
	}

	// 略大于 0.35 = 4/11 ≈ 0.3636：不触发。
	// a2 = {a1..a4, x1..x3}，b2 = {a1..a4, y1..y4}
	// intersection = 4, union = 4+3+4 = 11, jaccard = 4/11 ≈ 0.3636 > 0.35
	a2 := "a1 a2 a3 a4 x1 x2 x3"
	b2 := "a1 a2 a3 a4 y1 y2 y3 y4"
	if ov := TopicFingerprintOverlap(a2, b2); ov <= 0.35 {
		t.Fatalf("overlap above threshold=%v, want > 0.35", ov)
	}
	sig2 := FlushSignal{PrevSnippet: a2, NewSnippet: b2}
	if p := DecideFlush(sig2, cfg); p.Flush {
		t.Fatalf("just above threshold should not trigger, got %+v", p)
	}
}

// TestTopicFingerprintOverlapEdges Jaccard 边界用例：全空=1.0、单边空=0.0、
// 完全相同=1.0、完全无交集=0.0。
func TestTopicFingerprintOverlapEdges(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"", "", 1.0},
		{"   ", "", 1.0}, // 纯空白归一化后为空
		{"", "hello", 0.0},
		{"hello", "", 0.0},
		{"hello world", "hello world", 1.0},
		{"alpha beta", "gamma delta", 0.0},
	}
	for _, c := range cases {
		if got := TopicFingerprintOverlap(c.a, c.b); got != c.want {
			t.Fatalf("TopicFingerprintOverlap(%q, %q)=%v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestTopicFingerprintInsensitive 话题指纹对大小写、标点、多空格不敏感。
func TestTopicFingerprintInsensitive(t *testing.T) {
	cases := []struct {
		a, b string
	}{
		{"Hello World", "hello world"},
		{"Hello    World", "hello world"},
		{"Hello, World!", "hello world"},
		{"HELLO\tworld", "hello world"},
	}
	for _, c := range cases {
		if got := TopicFingerprintOverlap(c.a, c.b); got != 1.0 {
			t.Fatalf("TopicFingerprintOverlap(%q, %q)=%v, want 1.0", c.a, c.b, got)
		}
	}
}

// TestFlushIdempotent 幂等：同一 signal 连调 3 次结构完全一致。
func TestFlushIdempotent(t *testing.T) {
	sig := FlushSignal{
		Now:         time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		SessionEnd:  true,
		TaskDone:    true,
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		L0Count:     100,
		Items:       []string{"m3", "m1", "m2", "m1", "m3"},
	}
	cfg := DefaultFlushConfig()
	p1 := DecideFlush(sig, cfg)
	p2 := DecideFlush(sig, cfg)
	p3 := DecideFlush(sig, cfg)
	if !reflect.DeepEqual(p1, p2) || !reflect.DeepEqual(p2, p3) {
		t.Fatalf("not idempotent: %+v / %+v / %+v", p1, p2, p3)
	}
}

// TestFlushItemsDedupAndSort Items 去重 + 字典序。
func TestFlushItemsDedupAndSort(t *testing.T) {
	sig := FlushSignal{
		SessionEnd: true,
		Items:      []string{"m3", "m1", "m2", "m1", "m3", "m2", "m4"},
	}
	plan := DecideFlush(sig, DefaultFlushConfig())
	want := []string{"m1", "m2", "m3", "m4"}
	if !reflect.DeepEqual(plan.Items, want) {
		t.Fatalf("Items=%v, want %v", plan.Items, want)
	}
}

// TestFlushEmptyItemsNonNil 空输入时 Items / HoldItems / Reasons 均为非 nil 空切片。
func TestFlushEmptyItemsNonNil(t *testing.T) {
	plan := DecideFlush(FlushSignal{SessionEnd: true}, DefaultFlushConfig())
	if plan.Items == nil {
		t.Fatal("Items should be non-nil empty slice")
	}
	if len(plan.Items) != 0 {
		t.Fatalf("Items should be empty, got %v", plan.Items)
	}
	if plan.HoldItems == nil {
		t.Fatal("HoldItems should be non-nil empty slice")
	}
	if len(plan.HoldItems) != 0 {
		t.Fatalf("HoldItems should be empty, got %v", plan.HoldItems)
	}
}

// TestFlushHoldItemsAndItemsDisjoint 话题切换 + KeepRecent>0 时 HoldItems 取输入
// 顺序尾部 KeepRecent 条，与 Items 不相交。
func TestFlushHoldItemsAndItemsDisjoint(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 2}
	sig := FlushSignal{
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		Items:       []string{"m1", "m2", "m3", "m4"},
	}
	plan := DecideFlush(sig, cfg)

	wantHold := []string{"m3", "m4"}
	if !reflect.DeepEqual(plan.HoldItems, wantHold) {
		t.Fatalf("HoldItems=%v, want %v", plan.HoldItems, wantHold)
	}
	wantItems := []string{"m1", "m2"}
	if !reflect.DeepEqual(plan.Items, wantItems) {
		t.Fatalf("Items=%v, want %v", plan.Items, wantItems)
	}

	hSet := map[string]bool{}
	for _, h := range plan.HoldItems {
		hSet[h] = true
	}
	for _, it := range plan.Items {
		if hSet[it] {
			t.Fatalf("overlap between Items and HoldItems: %v", it)
		}
	}
}

// TestFlushHoldItemsUnsortedInput 输入未排序时 HoldItems 仍按输入顺序取尾部再字典序输出。
func TestFlushHoldItemsUnsortedInput(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 2}
	sig := FlushSignal{
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		Items:       []string{"m1", "m3", "m2", "m4", "m5"},
	}
	plan := DecideFlush(sig, cfg)
	// 输入顺序尾部 2 条 = [m4, m5]，字典序输出 = [m4, m5]
	wantHold := []string{"m4", "m5"}
	if !reflect.DeepEqual(plan.HoldItems, wantHold) {
		t.Fatalf("HoldItems=%v, want %v", plan.HoldItems, wantHold)
	}
	wantItems := []string{"m1", "m2", "m3"}
	if !reflect.DeepEqual(plan.Items, wantItems) {
		t.Fatalf("Items=%v, want %v", plan.Items, wantItems)
	}
}

// TestFlushNoHoldWhenNotTopicSwitch 非话题切换触发时 KeepRecent 不生效（HoldItems 空）。
func TestFlushNoHoldWhenNotTopicSwitch(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 3}
	sig := FlushSignal{
		SessionEnd: true,
		Items:      []string{"m1", "m2", "m3", "m4"},
	}
	plan := DecideFlush(sig, cfg)
	if len(plan.HoldItems) != 0 {
		t.Fatalf("HoldItems should be empty for non-topic-switch, got %v", plan.HoldItems)
	}
	want := []string{"m1", "m2", "m3", "m4"}
	if !reflect.DeepEqual(plan.Items, want) {
		t.Fatalf("Items=%v, want %v", plan.Items, want)
	}
}

// TestFlushKeepRecentGECount KeepRecent >= 去重后条数时 HoldItems 空。
func TestFlushKeepRecentGECount(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: 5}
	sig := FlushSignal{
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		Items:       []string{"m1", "m2", "m3", "m1"},
	}
	plan := DecideFlush(sig, cfg)
	if len(plan.HoldItems) != 0 {
		t.Fatalf("HoldItems should be empty when KeepRecent >= count, got %v", plan.HoldItems)
	}
	want := []string{"m1", "m2", "m3"}
	if !reflect.DeepEqual(plan.Items, want) {
		t.Fatalf("Items=%v, want %v", plan.Items, want)
	}
}

// TestFlushNilOrEmptySignalSafe 零值 signal 与零值 cfg 均不 panic。
func TestFlushNilOrEmptySignalSafe(t *testing.T) {
	// 零值 signal，默认 cfg：无任何触发
	plan := DecideFlush(FlushSignal{}, DefaultFlushConfig())
	if plan.Flush {
		t.Fatalf("zero signal should not flush, got %+v", plan)
	}
	if plan.Reasons == nil || len(plan.Reasons) != 0 {
		t.Fatalf("Reasons should be non-nil empty, got %v", plan.Reasons)
	}
	if plan.Items == nil || len(plan.Items) != 0 {
		t.Fatalf("Items should be non-nil empty, got %v", plan.Items)
	}
	if plan.HoldItems == nil || len(plan.HoldItems) != 0 {
		t.Fatalf("HoldItems should be non-nil empty, got %v", plan.HoldItems)
	}

	// 零值 cfg（L0Limit/TopicSwitchOverlap 均 <=0，回落到默认）
	plan2 := DecideFlush(FlushSignal{L0Count: 100}, FlushConfig{})
	if !plan2.Flush {
		t.Fatal("L0Count 100 with empty cfg should trigger L0Capacity via default 64")
	}
	wantReasons := []FlushReason{FlushL0Capacity}
	if !reflect.DeepEqual(plan2.Reasons, wantReasons) {
		t.Fatalf("Reasons=%v, want %v", plan2.Reasons, wantReasons)
	}
}

// TestFlushNegativeKeepRecent KeepRecent<0 视为 0，不产生 HoldItems。
func TestFlushNegativeKeepRecent(t *testing.T) {
	cfg := FlushConfig{L0Limit: 64, TopicSwitchOverlap: 0.35, KeepRecent: -5}
	sig := FlushSignal{
		PrevSnippet: "alpha",
		NewSnippet:  "omega",
		Items:       []string{"m1", "m2", "m3"},
	}
	plan := DecideFlush(sig, cfg)
	if len(plan.HoldItems) != 0 {
		t.Fatalf("negative KeepRecent should act as 0, got %v", plan.HoldItems)
	}
	want := []string{"m1", "m2", "m3"}
	if !reflect.DeepEqual(plan.Items, want) {
		t.Fatalf("Items=%v, want %v", plan.Items, want)
	}
}

// TestFlushSnippetEmptyNotTrigger PrevSnippet 或 NewSnippet 归一化后为空时不触发话题切换。
func TestFlushSnippetEmptyNotTrigger(t *testing.T) {
	cfg := DefaultFlushConfig()
	cases := []FlushSignal{
		{PrevSnippet: "", NewSnippet: "hello"},
		{PrevSnippet: "hello", NewSnippet: ""},
		{PrevSnippet: "   ", NewSnippet: "hello"},
		{PrevSnippet: "hello", NewSnippet: "  \t "},
	}
	for i, sig := range cases {
		if p := DecideFlush(sig, cfg); p.Flush {
			t.Fatalf("case %d: empty snippet should not trigger, got %+v", i, p)
		}
	}
}

// TestFlushNowInjected Now 字段由调用方注入，本决策不依赖时间。
func TestFlushNowInjected(t *testing.T) {
	sig1 := FlushSignal{Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SessionEnd: true}
	sig2 := FlushSignal{Now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), SessionEnd: true}
	p1 := DecideFlush(sig1, DefaultFlushConfig())
	p2 := DecideFlush(sig2, DefaultFlushConfig())
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("Now should not affect decision: %+v vs %+v", p1, p2)
	}
}
