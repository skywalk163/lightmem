package bench

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/lightmem/kernel"
)

// LoCoMoAdapter 解析 snap-research/locomo locomo10。
type LoCoMoAdapter struct{}

type lcQA struct {
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"`
	Evidence []string        `json:"evidence"`
	Category int             `json:"category"`
}

type lcTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

type lcConv struct {
	SampleID     string                     `json:"sample_id"`
	QA           []lcQA                     `json:"qa"`
	Conversation map[string]json.RawMessage `json:"conversation"`
}

func (LoCoMoAdapter) Source() Source { return SourceLoCoMo }

func (LoCoMoAdapter) Parse(path string) ([]RawConversation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var convs []lcConv
	if err := json.Unmarshal(b, &convs); err != nil {
		return nil, err
	}
	var out []RawConversation
	for ci := range convs {
		c := &convs[ci]
		turnByDia := map[string]lcTurn{}
		dateByDia := map[string]string{}
		var turns []RawTurn
		for k, v := range c.Conversation {
			if !strings.HasPrefix(k, "session_") || strings.HasSuffix(k, "_date_time") {
				continue
			}
			var sessionDate string
			if dv, ok := c.Conversation[k+"_date_time"]; ok {
				sessionDate = RawToString(dv)
			}
			var raw []lcTurn
			if err := json.Unmarshal(v, &raw); err != nil {
				continue
			}
			for _, t := range raw {
				turnByDia[t.DiaID] = t
				dateByDia[t.DiaID] = sessionDate
				turns = append(turns, RawTurn{Speaker: t.Speaker, Text: t.Speaker + ": " + t.Text})
			}
		}

		memByID := map[string]kernel.Memory{}
		var qs []RawQuestion
		for _, qa := range c.QA {
			bucket, isTemporal, keep := locomoBucket(qa.Category)
			if !keep {
				continue
			}
			ans := RawToString(qa.Answer)
			var expIDs []string
			firstEvidence := ""
			for _, ev := range qa.Evidence {
				t, found := turnByDia[ev]
				if !found {
					continue
				}
				if firstEvidence == "" {
					firstEvidence = ev
				}
				id := MemKey(c.SampleID, ev)
				if _, exists := memByID[id]; !exists {
					memByID[id] = newMemory(id, "t-lc", "u-lc", t.Speaker+": "+t.Text, "locomo", t.Speaker+": "+t.Text, dateByDia[ev])
				}
				expIDs = append(expIDs, id)
			}
			if len(expIDs) == 0 {
				continue
			}
			// R2 任务B：LoCoMo temporal(cat2) 样本补 as_of + SecondVersion。
			// as_of = 证据 turn 日期（若可解析）+1 天（提问时刻在事实发生之后），
			// SecondVersion 由首个证据记忆派生；无法解析日期则降级为不补。
			var qAsOf string
			var qSecond *kernel.Memory
			if isTemporal && len(expIDs) > 0 {
				if m0, ok := memByID[expIDs[0]]; ok {
					if t, ok2 := asOfDate(dateByDia[firstEvidence]); ok2 && !t.IsZero() {
						asOf := t.AddDate(0, 0, 1).UTC()
						qAsOf = dateOnly(asOf)
						qSecond = makeSecondVersion(m0, qAsOf)
					}
				}
			}
			qs = append(qs, RawQuestion{
				ID:                expIDs[0],
				Query:             qa.Question,
				Expected:          []string{ans},
				ExpectedFact:      ans,
				Bucket:            bucket,
				IsTemporal:        isTemporal,
				ExpectedMemoryIDs: expIDs,
				AsOf:              qAsOf,
				SecondVersion:     qSecond,
			})
		}

		out = append(out, RawConversation{
			ID:         c.SampleID,
			Tenant:     "t-lc",
			User:       "u-lc",
			Turns:      turns,
			Questions:  qs,
			MemoryByID: memByID,
		})
	}
	return out, nil
}

func (LoCoMoAdapter) ToMemories(conv *RawConversation) []kernel.Memory {
	out := make([]kernel.Memory, 0, len(conv.MemoryByID))
	for _, m := range conv.MemoryByID {
		out = append(out, m)
	}
	return out
}

func (LoCoMoAdapter) ToQuestions(conv *RawConversation) []RawQuestion {
	return conv.Questions
}

func locomoBucket(cat int) (bucket string, isTemporal bool, keep bool) {
	switch cat {
	case 1:
		return BucketSingleHop, false, true
	case 2:
		return BucketTemporal, true, true
	case 3:
		return BucketMultiHop, false, true
	case 4:
		return BucketOpenDomain, false, true
	default:
		return "", false, false
	}
}
