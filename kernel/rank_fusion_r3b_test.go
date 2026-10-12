// R3-B：temporal as_of 时序一致性——signalFreshness 未来记忆、signalFeedbackHistory
// rf.Now 确定性 的单测。任务书 §2.5 验收要求 go test 全绿。
package kernel

import (
	"context"
	"testing"
	"time"
)

func mkTimeCand(id string, occurredAt time.Time) RecalledCandidate {
	c := mkCand(id, "test fact", "entity1", KindFact, 1.0)
	c.Memory.OccurredAt = &occurredAt
	return c
}

// R3-B-1：signalFreshness 在 rf.Now=as_of 时，未来记忆（occurred_at > now）必须得 0 分。
// 原实现 clamp age<0 → exp(0)=1.0，未来记忆反而满分（R3-B 发现的时序 bug）。
func TestSignalFreshnessFutureMemoryZero(t *testing.T) {
	now := time.Date(2023, 5, 26, 0, 0, 0, 0, time.UTC)
	rf := &RankFusion{Now: now, HalfLife: 30 * 24 * time.Hour}

	past := time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC)
	future := time.Date(2023, 5, 30, 0, 0, 0, 0, time.UTC)

	cands := []RecalledCandidate{mkTimeCand("past", past), mkTimeCand("future", future)}
	got := rf.signalFreshness(context.Background(), "q", cands)

	if got["future"] != 0.0 {
		t.Errorf("future memory freshness = %v, want 0.0", got["future"])
	}
	if got["past"] <= 0 {
		t.Errorf("past memory freshness = %v, want > 0", got["past"])
	}
}

// R3-B-2：signalFreshness 在 as_of 之前（未来）的记忆不应超过 as_of 之前的记忆。
// 即：过去时间越近，freshness 越高；未来恒为 0。
func TestSignalFreshnessMonotonicInPast(t *testing.T) {
	now := time.Date(2023, 5, 26, 0, 0, 0, 0, time.UTC)
	rf := &RankFusion{Now: now, HalfLife: 30 * 24 * time.Hour}

	t1 := now.Add(-24 * time.Hour)
	t2 := now.Add(-10 * 24 * time.Hour)
	cands := []RecalledCandidate{mkTimeCand("recent", t1), mkTimeCand("older", t2)}
	got := rf.signalFreshness(context.Background(), "q", cands)

	if got["recent"] <= got["older"] {
		t.Errorf("recent(%v) should be fresher than older(%v)", got["recent"], got["older"])
	}
}

// R3-B-3：signalFeedbackHistory 使用 rf.Now 判断「90 天内近期」，不使用 time.Now()。
// 构造：rf.Now=2023-01-01；feedback 时间=2022-12-01（距 rf.Now 31 天，在 90 天内→recent）。
// 若误用真实当前时间（2026+），该 feedback 会超过 90 天，退化为不 recent，
// 影响 adopted 记忆的反馈分。此处验证 recentAdopted 逻辑生效。
func TestSignalFeedbackHistoryUsesRFNow(t *testing.T) {
	rfNow := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	fbTime := time.Date(2022, 12, 1, 0, 0, 0, 0, time.UTC) // 31 天前，在 90 天半衰期内

	c := mkCand("adopted", "fact", "e", KindFact, 1.0)
	c.Memory.FeedbackRecords = []FeedbackRecord{{Status: FbAdopted, Time: fbTime, Operator: "u"}}

	rf := &RankFusion{Now: rfNow}
	got := rf.signalFeedbackHistory(context.Background(), "q", []RecalledCandidate{c})

	// 只有 adopted 无 rejected → base = 1.0；recentAdopted=1 且 recentRejected=0 → ratio=1.0
	// 混合：base = 0.7*1.0 + 0.3*1.0 = 1.0
	if got["adopted"] != 1.0 {
		t.Errorf("adopted feedback = %v, want 1.0 (recent window active under rf.Now)", got["adopted"])
	}
}

// R3-B-4：signalFeedbackHistory 在 rf.Now 为零时回退 time.Now().UTC()（向后兼容）。
func TestSignalFeedbackHistoryFallbackWhenNowZero(t *testing.T) {
	fbTime := time.Now().UTC().Add(-24 * time.Hour)
	c := mkCand("adopted", "fact", "e", KindFact, 1.0)
	c.Memory.FeedbackRecords = []FeedbackRecord{{Status: FbAdopted, Time: fbTime, Operator: "u"}}

	rf := &RankFusion{} // Now 零值
	got := rf.signalFeedbackHistory(context.Background(), "q", []RecalledCandidate{c})

	if got["adopted"] != 1.0 {
		t.Errorf("adopted feedback = %v, want 1.0", got["adopted"])
	}
}

// R3-B-5：signalFeedbackHistory 无反馈记忆返回中性 0.5。
func TestSignalFeedbackHistoryNoFeedbackNeutral(t *testing.T) {
	c := mkCand("none", "fact", "e", KindFact, 1.0)
	rf := &RankFusion{Now: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)}
	got := rf.signalFeedbackHistory(context.Background(), "q", []RecalledCandidate{c})
	if got["none"] != 0.5 {
		t.Errorf("no-feedback = %v, want 0.5", got["none"])
	}
}
