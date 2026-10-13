// M2 口径：控制面 token 占比。
//
// 定义（任务书 §D.3.2；定位文档 §7 M2 / §8.3 分离度量）：
//
//		m2 = control_tokens / total_tokens
//
//	  - 控制面（control）：query + 路由 prompt + 召回 prompt 的表达 token
//	    （当前 Go kernel 编排是代码 0 token，此处用指令模板占位表达，
//	     用于与 Python+Prompt 基线同口径对比，见 polaris_token.go 头部说明）；
//	  - 数据面（data）：召回候选记忆正文（top-K fact 文本）token；
//	  - total = control + data。
//
// 聚合口径（任务书「控制面 token 占比」的总量形式）：
//
//	m2 = Σcontrol / Σ(control+data)   （按查询汇总，非逐条比值的平均）
//
// 逐条比值另输出 mean_per_query_m2 供对照。低 M2 = 控制面开销占比小
// = 光明语言表意密度优势的量化面（定位文档 §7：独有指标）。
package main

// M2Report M2 口径统计结果。
type M2Report struct {
	// Queries 计入统计的查询数。
	Queries int `json:"queries"`
	// ControlTokens / DataTokens / TotalTokens 总量（token 估算口径）。
	ControlTokens int `json:"control_tokens"`
	DataTokens    int `json:"data_tokens"`
	TotalTokens   int `json:"total_tokens"`
	// M2 控制面 token 占比 = control / total（总量聚合口径）。
	M2 float64 `json:"m2_control_share"`
	// MeanPerQueryM2 逐条 m2 比值的算术平均（对照口径）。
	MeanPerQueryM2 float64 `json:"mean_per_query_m2"`
	// AvgControlTokens / AvgDataTokens 单查询均值。
	AvgControlTokens float64 `json:"avg_control_tokens"`
	AvgDataTokens    float64 `json:"avg_data_tokens"`
	// ByBucket 分桶统计（定位哪类查询控制面占比高）。
	ByBucket map[string]M2Bucket `json:"by_bucket,omitempty"`
	// Compressed 本次运行控制面是否走了压缩口径（query 压缩 + 指令短化）。
	Compressed bool `json:"compressed"`
}

// M2Bucket 分桶 M2 统计。
type M2Bucket struct {
	Queries       int     `json:"queries"`
	ControlTokens int     `json:"control_tokens"`
	DataTokens    int     `json:"data_tokens"`
	TotalTokens   int     `json:"total_tokens"`
	M2            float64 `json:"m2_control_share"`
}

// M2Accumulator M2 累计器。
type M2Accumulator struct {
	Compressed  bool
	Queries     int
	Control     int
	Data        int
	PerQuerySum float64
	ByBucket    map[string]*M2Bucket
}

// NewM2Accumulator 构造累计器。compressed 标记本次运行的控制面压缩状态。
func NewM2Accumulator(compressed bool) *M2Accumulator {
	return &M2Accumulator{Compressed: compressed, ByBucket: map[string]*M2Bucket{}}
}

// AddAccount 累加单条查询的 token 账目。
func (a *M2Accumulator) AddAccount(bucket string, acct TokenAccount) {
	if bucket == "" {
		bucket = "default"
	}
	a.Queries++
	a.Control += acct.ControlTokens
	a.Data += acct.DataTokens
	if acct.TotalTokens > 0 {
		a.PerQuerySum += float64(acct.ControlTokens) / float64(acct.TotalTokens)
	}
	b := a.ByBucket[bucket]
	if b == nil {
		b = &M2Bucket{}
		a.ByBucket[bucket] = b
	}
	b.Queries++
	b.ControlTokens += acct.ControlTokens
	b.DataTokens += acct.DataTokens
	b.TotalTokens += acct.TotalTokens
}

// Report 汇总输出。
func (a *M2Accumulator) Report() M2Report {
	rep := M2Report{
		Queries:       a.Queries,
		ControlTokens: a.Control,
		DataTokens:    a.Data,
		Compressed:    a.Compressed,
		ByBucket:      map[string]M2Bucket{},
	}
	rep.TotalTokens = a.Control + a.Data
	if rep.TotalTokens > 0 {
		rep.M2 = float64(a.Control) / float64(rep.TotalTokens)
	}
	if a.Queries > 0 {
		rep.MeanPerQueryM2 = a.PerQuerySum / float64(a.Queries)
		rep.AvgControlTokens = float64(a.Control) / float64(a.Queries)
		rep.AvgDataTokens = float64(a.Data) / float64(a.Queries)
	}
	for k, v := range a.ByBucket {
		b := M2Bucket{Queries: v.Queries, ControlTokens: v.ControlTokens,
			DataTokens: v.DataTokens, TotalTokens: v.TotalTokens}
		if b.TotalTokens > 0 {
			b.M2 = float64(b.ControlTokens) / float64(b.TotalTokens)
		}
		rep.ByBucket[k] = b
	}
	return rep
}
