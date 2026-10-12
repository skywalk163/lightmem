package bench

import (
	"encoding/json"
	"os"

	"github.com/lightmem/kernel"
)

// LongMemEvalAdapter 解析 xiaowu0162/LongMemEval（oracle 变体）。
type LongMemEvalAdapter struct{}

type lmeRecord struct {
	QuestionID   string          `json:"question_id"`
	QuestionType string          `json:"question_type"`
	Question     string          `json:"question"`
	Answer       json.RawMessage `json:"answer"`
	QuestionDate string          `json:"question_date"`
}

func (LongMemEvalAdapter) Source() Source { return SourceLongMemEval }

func (LongMemEvalAdapter) Parse(path string) ([]RawConversation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var recs []lmeRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	var out []RawConversation
	for _, r := range recs {
		ans := RawToString(r.Answer)
		bucket, isTemporal := lmeBucket(r.QuestionType)
		memID := "lme-" + SanitizeID(r.QuestionID)
		mem := newMemory(memID, "t-lme", "u-lme", r.Question+" => "+ans, "longmemeval", r.Question, r.QuestionDate)
		q := RawQuestion{
			ID:                memID,
			Query:             r.Question,
			Expected:          []string{ans},
			ExpectedFact:      ans,
			Bucket:            bucket,
			IsTemporal:        isTemporal,
			ExpectedMemoryIDs: []string{memID},
		}
		// R2 任务B：LME temporal 样本补 as_of（= question_date）与 SecondVersion。
		// question_date 是「提问时刻」，作为 as_of 使查询只在此时点可见的记忆中检索。
		if isTemporal {
			if t, ok := parseDate(r.QuestionDate); ok && !t.IsZero() {
				q.AsOf = dateOnly(t)
				q.SecondVersion = makeSecondVersion(mem, q.AsOf)
			}
		}
		out = append(out, RawConversation{
			ID:     r.QuestionID,
			Tenant: "t-lme",
			User:   "u-lme",
			Turns:  []RawTurn{{Speaker: "user", Text: mem.Fact, Ts: r.QuestionDate}},
			Questions: []RawQuestion{
				q,
			},
			MemoryByID: map[string]kernel.Memory{memID: mem},
		})
	}
	return out, nil
}

func (LongMemEvalAdapter) ToMemories(conv *RawConversation) []kernel.Memory {
	out := make([]kernel.Memory, 0, len(conv.MemoryByID))
	for _, m := range conv.MemoryByID {
		out = append(out, m)
	}
	return out
}

func (LongMemEvalAdapter) ToQuestions(conv *RawConversation) []RawQuestion {
	return conv.Questions
}

func lmeBucket(qtype string) (bucket string, isTemporal bool) {
	switch qtype {
	case "temporal-reasoning", "knowledge_update":
		return BucketTemporal, true
	case "multi-session":
		return BucketMultiHop, false
	default: // single-session-* 及未知
		return BucketSingleHop, false
	}
}
