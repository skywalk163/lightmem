package bench

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

// BeamAdapter 解析 mem0ai/memory-benchmarks BEAM（1M 评测题）。
// 原始数据不含时间字段（occurred_at 0/100），R2 任务B 用确定性序号为 temporal
// 样本派生 as_of 与 SecondVersion（起始 2024-01-01，逐 temporal 样本 +24h，不用 time.Now）。
type BeamAdapter struct {
	// temporalSeq temporal 样本的确定性序号（Parse 内部累加，跨样本唯一）。
	// 不参与并发；sample.go 单线程调用 Parse。
	temporalSeq int
}

type beamRecord struct {
	QuestionID        string   `json:"question_id"`
	QuestionType      string   `json:"question_type"`
	Difficulty        string   `json:"difficulty"`
	Question          string   `json:"question"`
	GroundTruthAnswer string   `json:"ground_truth_answer"`
	Rubric            []string `json:"rubric"`
}

func (BeamAdapter) Source() Source { return SourceBeam }

// beamBaseTime BEAM 时间序列起点（固定常量，不用 time.Now）。
var beamBaseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func (BeamAdapter) Parse(path string) ([]RawConversation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var recs []beamRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	var out []RawConversation
	a := BeamAdapter{}
	for _, r := range recs {
		bucket, isTemporal, keep := beamBucket(r.QuestionType)
		if !keep {
			continue
		}
		ans := r.GroundTruthAnswer
		if ans == "" && len(r.Rubric) > 0 {
			ans = strings.Join(r.Rubric, " | ")
		}
		memID := "beam-" + SanitizeID(r.QuestionID)
		mem := newMemory(memID, "t-beam", "u-beam", r.Question+" => "+ans, "beam", r.Question, "")
		q := RawQuestion{
			ID:                memID,
			Query:             r.Question,
			Expected:          []string{ans},
			ExpectedFact:      ans,
			Bucket:            bucket,
			IsTemporal:        isTemporal,
			ExpectedMemoryIDs: []string{memID},
		}
		// R2 任务B：BEAM temporal 样本派生 as_of 与 SecondVersion。
		// 原始数据无时间字段（occurred_at 0/100），用确定性序号派生：
		//   as_of = beamBaseTime + (temporalSeq-1)*24h，逐 temporal 样本递增。
		// 同时为当前 memory 补 occurred_at（取 as_of 前一日），使 freshness 信号可区分版本。
		if isTemporal {
			a.temporalSeq++
			asOf := beamBaseTime.AddDate(0, 0, a.temporalSeq-1).UTC()
			q.AsOf = dateOnly(asOf)
			prev := asOf.AddDate(0, 0, -1).UTC()
			mm := mem
			mm.OccurredAt = &prev
			mm.RecordedAt = prev
			q.SecondVersion = makeSecondVersion(mm, q.AsOf)
			out = append(out, RawConversation{
				ID:         r.QuestionID,
				Tenant:     "t-beam",
				User:       "u-beam",
				Turns:      []RawTurn{{Speaker: "user", Text: mm.Fact}},
				Questions:  []RawQuestion{q},
				MemoryByID: map[string]kernel.Memory{memID: mm},
			})
			continue
		}
		out = append(out, RawConversation{
			ID:         r.QuestionID,
			Tenant:     "t-beam",
			User:       "u-beam",
			Turns:      []RawTurn{{Speaker: "user", Text: mem.Fact}},
			Questions:  []RawQuestion{q},
			MemoryByID: map[string]kernel.Memory{memID: mem},
		})
	}
	return out, nil
}

func (BeamAdapter) ToMemories(conv *RawConversation) []kernel.Memory {
	out := make([]kernel.Memory, 0, len(conv.MemoryByID))
	for _, m := range conv.MemoryByID {
		out = append(out, m)
	}
	return out
}

func (BeamAdapter) ToQuestions(conv *RawConversation) []RawQuestion {
	return conv.Questions
}

func beamBucket(qt string) (bucket string, isTemporal bool, keep bool) {
	switch qt {
	case "temporal_reasoning", "event_ordering", "knowledge_update":
		return BucketTemporal, true, true
	case "multi_session_reasoning", "contradiction_resolution":
		return BucketMultiHop, false, true
	case "summarization":
		return BucketOpenDomain, false, true
	case "preference_following", "information_extraction", "instruction_following":
		return BucketSingleHop, false, true
	default: // abstention 等不可答题：剔除。
		return "", false, false
	}
}
