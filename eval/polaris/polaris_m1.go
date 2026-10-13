// M1 口径：零 token 覆盖率（北极星主指标）。
//
// 定义（任务书 §D.3.1）：在 N 条真实 query 上，统计检索中**不消耗生成 token
// 即拿到正确事实**的比例：
//
//	m1_coverage = hits / N
//
// 命中判定（确定性，不调 LLM）：
//   - 一次「检索试验」= 对该 query 跑一次 RecallAndFuse（无任何 LLM 生成调用，
//     生成 token 消耗恒为 0——这就是「零 token」的含义：答案由记忆检索直接给出）；
//   - hit = expected 事实文本在 top-K 候选 fact 中被 matchFactV4 判定命中
//     （与 run_kpi.go 的 fact 口径 v4 同源，见 polaris_fact.go 头部）；
//   - K 由 -topk 注入（默认 5）：取 top-1 时即 hit@1 口径，另单独统计。
//
// 辅助口径（用于定位覆盖率缺口，不覆盖主口径）：
//   - hit_at_1：top-1 fact 命中（最严格：一次交互直接答对）；
//   - id_hit：top-1 MemoryID ∈ expected_memory_ids（语料带 ID 时）；
//   - mean_rr：期望记忆首次命中 rank 的倒数均值（语料带 ID 时）。
package main

import "time"

// M1Report M1 口径统计结果。
type M1Report struct {
	// N 试验总数（= 计入分母的 query 数；对抗样本不计入）。
	N int `json:"n"`
	// Hits top-K 内拿到正确事实的次数（分子）。
	Hits int `json:"hits"`
	// Coverage 零 token 覆盖率 = hits / N（北极星指标，目标 ≥0.95）。
	Coverage float64 `json:"m1_coverage"`
	// HitAt1 top-1 即命中的次数与比率（最严格口径）。
	HitAt1     int     `json:"hit_at_1"`
	HitAt1Rate float64 `json:"hit_at_1_rate"`
	// IDHit top-1 MemoryID 命中次数（语料带 expected_memory_ids 时统计）。
	IDHit     int     `json:"id_hit,omitempty"`
	IDTotal   int     `json:"id_total,omitempty"`
	IDHitRate float64 `json:"id_hit_rate,omitempty"`
	// MeanRR 期望记忆首次命中 rank 的倒数均值（未命中计 0）。
	MeanRR float64 `json:"mean_rr,omitempty"`
	// ByBucket 分桶覆盖率（定位缺口用；桶按语料 bucket 字段聚合）。
	ByBucket map[string]M1Bucket `json:"by_bucket,omitempty"`
	// TopK 本次运行的候选窗口。
	TopK int `json:"topk"`
}

// M1Bucket 分桶 M1 统计。
type M1Bucket struct {
	N        int     `json:"n"`
	Hits     int     `json:"hits"`
	Coverage float64 `json:"coverage"`
	HitAt1   int     `json:"hit_at_1"`
}

// M1Trial 单条 query 的检索试验结果（M1 判定 + M2 账目共用）。
type M1Trial struct {
	Query     string
	Expected  string
	Bucket    string
	TopFacts  []string   // top-K 候选 fact（按融合序）
	TopIDs    []string   // top-K 候选 memory_id
	Hit       bool       // top-K 内 fact 命中（零 token 拿到正确事实）
	HitAt1    bool       // top-1 fact 命中
	FirstRank int        // expected_memory_ids 首次命中 rank（0=未命中）
	HasIDs    bool       // 语料是否带 expected_memory_ids（决定 ID 口径是否适用）
	Anchor    *time.Time // top-1 候选 occurred_at（时间推理锚点）
	IDHitAt1  bool       // top-1 MemoryID ∈ expected（有 ID 语料时）
}

// JudgeM1 对单条试验做 M1 判定（纯函数，确定性）。
// expectedIDs 为空时跳过 ID 口径（FirstRank=0、IDHitAt1=false、HasIDs=false）。
func JudgeM1(expected string, expectedIDs []string, topFacts []string, topIDs []string, topK int, anchor *time.Time) M1Trial {
	t := M1Trial{
		Expected:  expected,
		TopFacts:  topFacts,
		TopIDs:    topIDs,
		FirstRank: 0,
		HasIDs:    len(expectedIDs) > 0,
		Anchor:    anchor,
	}
	if topK <= 0 {
		topK = 5
	}
	// fact 命中：top-K 窗口内任一候选 fact 通过 v4 判定。
	for i, f := range topFacts {
		if i >= topK {
			break
		}
		if matchFactV4(f, expected, anchor) {
			t.Hit = true
			break
		}
	}
	// hit@1：top-1 fact 命中。
	if len(topFacts) > 0 && matchFactV4(topFacts[0], expected, anchor) {
		t.HitAt1 = true
	}
	// ID 口径：期望 ID 首次出现 rank + top-1 ID 命中。
	if len(expectedIDs) > 0 {
		for i, id := range topIDs {
			if sliceContains(expectedIDs, id) {
				t.FirstRank = i + 1
				break
			}
		}
		if len(topIDs) > 0 && sliceContains(expectedIDs, topIDs[0]) {
			t.IDHitAt1 = true
		}
	}
	return t
}

// sliceContains 判断 s 中是否存在元素 target（与 run_kpi.go 同源）。
func sliceContains(s []string, target string) bool {
	if target == "" {
		return false
	}
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// M1Accumulator M1 累计器（逐条试验累加，最后汇总）。
type M1Accumulator struct {
	TopK     int
	N        int
	Hits     int
	HitAt1   int
	IDHit    int
	IDTotal  int
	RRSum    float64
	ByBucket map[string]*M1Bucket
}

// NewM1Accumulator 构造累计器。
func NewM1Accumulator(topK int) *M1Accumulator {
	if topK <= 0 {
		topK = 5
	}
	return &M1Accumulator{TopK: topK, ByBucket: map[string]*M1Bucket{}}
}

// Add 累加单条试验。bucket 为空时归入 "default"。
// ID 口径（IDHit/IDTotal/MeanRR）仅统计带 expected_memory_ids 的试验，
// 避免无 ID 语料稀释命中率。
func (a *M1Accumulator) Add(bucket string, t M1Trial) {
	if bucket == "" {
		bucket = "default"
	}
	a.N++
	if t.Hit {
		a.Hits++
	}
	if t.HitAt1 {
		a.HitAt1++
	}
	if t.HasIDs {
		a.IDTotal++
		if t.FirstRank > 0 {
			a.IDHit++
			a.RRSum += 1.0 / float64(t.FirstRank)
		}
	}
	b := a.ByBucket[bucket]
	if b == nil {
		b = &M1Bucket{}
		a.ByBucket[bucket] = b
	}
	b.N++
	if t.Hit {
		b.Hits++
	}
	if t.HitAt1 {
		b.HitAt1++
	}
}

// Report 汇总输出。IDTotal=0 表示语料无 ID 字段，IDHitRate/MeanRR 不适用。
func (a *M1Accumulator) Report() M1Report {
	rep := M1Report{N: a.N, Hits: a.Hits, HitAt1: a.HitAt1, TopK: a.TopK,
		ByBucket: map[string]M1Bucket{}}
	if a.N > 0 {
		rep.Coverage = float64(a.Hits) / float64(a.N)
		rep.HitAt1Rate = float64(a.HitAt1) / float64(a.N)
	}
	rep.IDHit, rep.IDTotal = a.IDHit, a.IDTotal
	if a.IDTotal > 0 {
		rep.IDHitRate = float64(a.IDHit) / float64(a.IDTotal)
		rep.MeanRR = a.RRSum / float64(a.IDTotal)
	}
	for k, v := range a.ByBucket {
		b := M1Bucket{N: v.N, Hits: v.Hits, HitAt1: v.HitAt1}
		if v.N > 0 {
			b.Coverage = float64(v.Hits) / float64(v.N)
		}
		rep.ByBucket[k] = b
	}
	return rep
}
