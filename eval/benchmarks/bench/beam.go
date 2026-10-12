package bench

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/lightmem/kernel"
)

// BeamAdapter 解析 mem0ai/memory-benchmarks BEAM（1M 评测题）。
type BeamAdapter struct{}

type beamRecord struct {
	QuestionID        string   `json:"question_id"`
	QuestionType      string   `json:"question_type"`
	Difficulty        string   `json:"difficulty"`
	Question          string   `json:"question"`
	GroundTruthAnswer string   `json:"ground_truth_answer"`
	Rubric            []string `json:"rubric"`
}

func (BeamAdapter) Source() Source { return SourceBeam }

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
		out = append(out, RawConversation{
			ID:     r.QuestionID,
			Tenant: "t-beam",
			User:   "u-beam",
			Turns:  []RawTurn{{Speaker: "user", Text: mem.Fact}},
			Questions: []RawQuestion{{
				ID:                memID,
				Query:             r.Question,
				Expected:          []string{ans},
				ExpectedFact:      ans,
				Bucket:            bucket,
				IsTemporal:        isTemporal,
				ExpectedMemoryIDs: []string{memID},
			}},
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
