//go:build ignore

// R1 外部基准数据集：固定 seed 子集抽取与确定性处理（B2）+ 落盘（B3）。
//
// 适配器层（B1）在 ./bench 子包：bench.Adapter + LongMemEval/LoCoMo/BEAM 三个适配器，
// 把官方原始数据解析为 LIMS Memory Schema。本脚本只做：固定 seed=42 分层采样、
// 跨来源 query 去重、落盘四份 JSONL。不调 LLM。
//
// 运行（在 lightmem/eval/benchmarks/ 下）：
//
//	go run ./sample.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"

	"lightmem/eval/benchmarks/bench"

	"github.com/lightmem/kernel"
)

// EvalItem 与 eval/run_kpi.go 的 Schema 对齐，供下游 P7-D 复用。
type EvalItem struct {
	ID               string         `json:"id"`
	Query            string         `json:"query"`
	ExpectedMemoryID []string       `json:"expected_memory_ids"`
	ExpectedFact     string         `json:"expected_fact"`
	Bucket           string         `json:"bucket"`
	IsTemporal       bool           `json:"is_temporal"`
	HasInjection     bool           `json:"has_injection"`
	HasPollution     bool           `json:"has_pollution"`
	Tenant           string         `json:"tenant"`
	User             string         `json:"user"`
	AsOf             string         `json:"as_of,omitempty"`
	Memory           *kernel.Memory `json:"memory"`
	Hop1Memory       *kernel.Memory `json:"hop1_memory,omitempty"`
}

// sourcePool 按来源归集已归一化的问题（含其记忆）。
type sourcePool struct {
	items []EvalItem
}

func main() {
	rawDir, outDir := "raw", "dataset"
	if _, err := os.Stat("eval"); err == nil {
		rawDir, outDir = "eval/benchmarks/raw", "eval/benchmarks/dataset"
	}
	_ = os.MkdirAll(outDir, 0o755)

	// 1) 三适配器解析（B1）。
	lme := mustParse(bench.LongMemEvalAdapter{}, rawDir+"/longmemeval.json")
	lc := mustParse(bench.LoCoMoAdapter{}, rawDir+"/locomo.json")
	bm := mustParse(bench.BeamAdapter{}, rawDir+"/beam.json")

	// 2) 归一化为 EvalItem 候选池。
	lmeItems := toItems(lme)
	lcItems := toItems(lc)
	bmItems := toItems(bm)

	// 3) 固定 seed=42 分层采样（LME 取全量 500；locomo/beam 按配额）。
	r := rand.New(rand.NewSource(42))
	lmeSampled := lmeItems // oracle 全量即主变体积子集
	lcSampled := stratifiedSample(r, lcItems, map[string]int{
		bench.BucketOpenDomain: 25, bench.BucketMultiHop: 12,
		bench.BucketTemporal: 8, bench.BucketSingleHop: 5,
	})
	bmSampled := stratifiedSample(r, bmItems, map[string]int{
		bench.BucketOpenDomain: 30, bench.BucketSingleHop: 25,
		bench.BucketTemporal: 25, bench.BucketMultiHop: 20,
	})

	log.Printf("parsed: longmemeval=%d locomo=%d beam=%d | sampled: %d/%d/%d",
		len(lmeItems), len(lcItems), len(bmItems), len(lmeSampled), len(lcSampled), len(bmSampled))

	// 4) id 唯一化 + 去重落盘。
	//    LoCoMo 多跳题可共享同一证据（memory_id），因此 RawQuestion.ID 与 memory_id 可能
	//    重复；落盘前先把 eval 条目 id 唯一化（追加 [dup#n] 后缀，不改写语义）。
	ensureUniqueIDs(lmeSampled)
	ensureUniqueIDs(lcSampled)
	ensureUniqueIDs(bmSampled)
	writeJSONL(outDir+"/longmemeval.jsonl", dedupQueries(lmeSampled))
	writeJSONL(outDir+"/locomo.jsonl", dedupQueries(lcSampled))
	writeJSONL(outDir+"/beam.jsonl", dedupQueries(bmSampled))

	all := append(append(append([]EvalItem{}, lmeSampled...), lcSampled...), bmSampled...)
	ensureUniqueIDs(all)
	all = dedupQueries(all)
	writeJSONL(outDir+"/all.jsonl", all)
	log.Printf("TOTAL all.jsonl: %d items", len(all))
}

func mustParse(a bench.Adapter, path string) []bench.RawConversation {
	convs, err := a.Parse(path)
	if err != nil {
		log.Fatalf("%s parse %s: %v", a.Source(), path, err)
	}
	return convs
}

// toItems 把归一化对话展开为 EvalItem；多证据问题把首条作为 Memory、其余挂 Hop1Memory。
func toItems(convs []bench.RawConversation) []EvalItem {
	var out []EvalItem
	for i := range convs {
		conv := &convs[i]
		mems := conv.MemoryByID
		for _, q := range conv.Questions {
			item := EvalItem{
				ID:               q.ID,
				Query:            q.Query,
				ExpectedMemoryID: q.ExpectedMemoryIDs,
				ExpectedFact:     q.ExpectedFact,
				Bucket:           q.Bucket,
				IsTemporal:       q.IsTemporal,
				Tenant:           conv.Tenant,
				User:             conv.User,
			}
			if m, ok := mems[q.ExpectedMemoryIDs[0]]; ok {
				mm := m
				item.Memory = &mm
			}
			if len(q.ExpectedMemoryIDs) > 1 {
				if m, ok := mems[q.ExpectedMemoryIDs[1]]; ok {
					mm := m
					item.Hop1Memory = &mm
				}
			}
			out = append(out, item)
		}
	}
	return out
}

// bucketOrder 固定四类分桶的遍历顺序。
// 关键：不能用 range 遍历 quotas map（Go map 迭代顺序随机），
// 否则 r.Shuffle 的随机序列会被不同桶顺序打乱，seed=42 无法产生确定性输出。
var bucketOrder = []string{
	bench.BucketSingleHop, bench.BucketMultiHop, bench.BucketTemporal, bench.BucketOpenDomain,
}

// stratifiedSample 按桶配额用固定 r 确定性洗牌取前 n。
func stratifiedSample(r *rand.Rand, items []EvalItem, quotas map[string]int) []EvalItem {
	byBucket := map[string][]EvalItem{}
	for _, it := range items {
		byBucket[it.Bucket] = append(byBucket[it.Bucket], it)
	}
	var out []EvalItem
	for _, bucket := range bucketOrder {
		pool := byBucket[bucket]
		n := quotas[bucket]
		sh := make([]EvalItem, len(pool))
		copy(sh, pool)
		r.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
		if n > len(sh) {
			n = len(sh)
		}
		out = append(out, sh[:n]...)
	}
	return out
}

// ensureUniqueIDs eval 条目 id 两两唯一：重复项追加 “ [dup#n]” 后缀（不改写语义）。
// 共享同一证据的多跳题（LoCoMo）允许 memory_id 复用，但 eval 条目 id 必须唯一，
// 否则下游按 id 索引的评测器会后写覆盖前写。
func ensureUniqueIDs(items []EvalItem) {
	seen := map[string]int{}
	for i := range items {
		cur := items[i].ID
		n := seen[cur]
		if n > 0 {
			items[i].ID = fmt.Sprintf("%s [dup#%d]", cur, n+1)
		}
		seen[cur] = n + 1
	}
}

// dedupQueries query 两两唯一：重复项追加 “ [id#n]” 后缀（不改写语义）。
func dedupQueries(items []EvalItem) []EvalItem {
	seen := map[string]int{}
	for i := range items {
		q := items[i].Query
		c := seen[q]
		if c > 0 {
			items[i].Query = fmt.Sprintf("%s [%s#%d]", q, items[i].ID, c+1)
		}
		seen[q] = c + 1
	}
	return items
}

func writeJSONL(path string, items []EvalItem) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	for _, it := range items {
		b, _ := json.Marshal(it)
		f.Write(b)
		f.WriteString("\n")
	}
	log.Printf("wrote %s: %d items", path, len(items))
}
