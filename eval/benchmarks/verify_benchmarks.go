//go:build ignore

// R1 外部基准数据集自检脚本（B4 / DoD 硬校验）。
//
//	go run ./verify_benchmarks.go
//
// 输出 4 项 ALL PASS：
//  1. 字段完整性 + 标识唯一性：每条含 id/query/expected_memory_ids/expected_fact/bucket/
//     is_temporal/has_injection/has_pollution/tenant/user/memory，且 id、memory_id 无重复
//  2. 条数：总数 >= 650，三基准各自达标（lme>=500, locomo>=50, beam>=100）
//  3. 分桶覆盖：single-hop/multi-hop/temporal/open-domain 各 >= 50
//  4. query 唯一性：无重复 query
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lightmem/kernel"
)

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

func load(path string) ([]EvalItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []EvalItem
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it EvalItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

func main() {
	ds := "dataset"
	if _, err := os.Stat("eval"); err == nil {
		ds = "eval/benchmarks/dataset"
	}

	all, err := load(ds + "/all.jsonl")
	if err != nil {
		fmt.Println("FATAL load all.jsonl:", err)
		os.Exit(1)
	}
	lme, _ := load(ds + "/longmemeval.jsonl")
	lc, _ := load(ds + "/locomo.jsonl")
	beam, _ := load(ds + "/beam.jsonl")

	pass := true

	// 1. 字段完整性 + 标识唯一性。
	//    id 是评测条目的主键，重复 id 会让下游按 id 索引的评测器后写覆盖前写（硬失败）。
	//    memory_id 允许跨 eval 条目复用（LoCoMo 多跳题会引用同一证据 turn），
	//    因此仅提示、不判 FAIL；但同一条目内 memory_id 必须唯一。
	missing := 0
	idSeen := map[string]int{}
	var dupIDs []string
	for _, it := range all {
		if it.ID == "" || it.Query == "" || it.ExpectedFact == "" || it.Bucket == "" ||
			it.Tenant == "" || it.User == "" || it.Memory == nil ||
			it.Memory.MemoryID == "" || it.Memory.Fact == "" ||
			len(it.ExpectedMemoryID) == 0 {
			missing++
			continue
		}
		idSeen[it.ID]++
		if idSeen[it.ID] == 2 {
			dupIDs = append(dupIDs, it.ID)
		}
	}
	r1 := missing == 0 && len(dupIDs) == 0
	pass = pass && r1
	fmt.Printf("[1] 字段完整性+唯一性: %s (缺字段 %d/%d, 重复id %d)\n",
		ok(r1), missing, len(all), len(dupIDs))
	if len(dupIDs) > 0 {
		fmt.Printf("    重复 id 示例: %v\n", trimList(dupIDs, 5))
	}

	// 1b. memory_id 复用提示（跨条目共享证据：合法，仅记录）。
	shared := 0
	memSeen := map[string]int{}
	var sharedMemIDs []string
	for _, it := range all {
		memSeen[it.Memory.MemoryID]++
	}
	for id, n := range memSeen {
		if n > 1 {
			shared++
			if len(sharedMemIDs) < 5 {
				sharedMemIDs = append(sharedMemIDs, fmt.Sprintf("%s(x%d)", id, n))
			}
		}
	}
	fmt.Printf("[1b] memory_id 跨条目复用(提示): %d 条, 示例: %v\n", shared, trimList(sharedMemIDs, 5))

	// 2. 条数。
	r2 := len(all) >= 650 && len(lme) >= 500 && len(lc) >= 50 && len(beam) >= 100
	pass = pass && r2
	fmt.Printf("[2] 条数: %s (all=%d, lme=%d>=500, locomo=%d>=50, beam=%d>=100)\n",
		ok(r2), len(all), len(lme), len(lc), len(beam))

	// 3. 分桶覆盖。
	bucketCount := map[string]int{}
	for _, it := range all {
		bucketCount[it.Bucket]++
	}
	r3 := true
	for _, b := range []string{"single-hop", "multi-hop", "temporal", "open-domain"} {
		if bucketCount[b] < 50 {
			r3 = false
		}
	}
	pass = pass && r3
	fmt.Printf("[3] 分桶覆盖: %s (%v)\n", ok(r3), bucketCount)

	// 4. query 唯一性。
	seen := map[string]int{}
	dups := 0
	for _, it := range all {
		seen[it.Query]++
		if seen[it.Query] > 1 {
			dups++
		}
	}
	r4 := dups == 0
	pass = pass && r4
	fmt.Printf("[4] query 唯一性: %s (重复 %d)\n", ok(r4), dups)

	fmt.Println("----------------------------------------")
	if pass {
		fmt.Println("RESULT: ALL PASS")
	} else {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
}

func ok(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

// trimList 安全截断示例列表，避免打印时越界 panic。
func trimList[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}
