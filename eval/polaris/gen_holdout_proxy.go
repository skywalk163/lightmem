//go:build ignore

// 北极星代理语料生成器（R5-D，真实语料 holdout 未到位时的降级路径）。
//
// 用法（0.88 上，从 eval/polaris/ 目录）：
//
//	go run ./gen_holdout_proxy.go -out ../dataset/holdout_proxy.jsonl
//
// 行为（确定性，无随机源——不需要 seed，因为没有采样，全量 + 固定文件序）：
//   - 读取既有评测集（默认 ../dataset/all.jsonl + ../dataset/reference_frame.jsonl
//   - ../dataset/temporal.jsonl），按 item id 去重（保留首次出现）；
//   - 剔除对抗样本（has_injection / has_pollution——它们是投毒测试，
//     不是「真实 query」，不计入北极星分母）；
//   - 转换为 holdout 语料格式（{"query","expected",...}），expected 取
//     expected_fact（M1 的「正确事实」判定文本）。
//
// ⚠️ 代理语料的性质：这些样本参与过权重调优（非严格 holdout）。北极星
// **达标判定必须以真实语料 holdout 为准**（roadmap §3.3：用户 5 天内提供）。
// 代理语料只用于预跑 M1/M2 口径、验证链路与压缩实验。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lightmem/kernel"
)

// srcItem 与 eval/run_kpi.go 的 EvalItem 解析字段对齐（只读用）。
type srcItem struct {
	ID               string         `json:"id"`
	Query            string         `json:"query"`
	ExpectedMemoryID []string       `json:"expected_memory_ids"`
	ExpectedFact     string         `json:"expected_fact"`
	Bucket           string         `json:"bucket"`
	HasInjection     bool           `json:"has_injection"`
	HasPollution     bool           `json:"has_pollution"`
	Tenant           string         `json:"tenant"`
	User             string         `json:"user"`
	AsOf             string         `json:"as_of,omitempty"`
	Memory           *kernel.Memory `json:"memory"`
	Hop1Memory       *kernel.Memory `json:"hop1_memory,omitempty"`
	Hop2Memory       *kernel.Memory `json:"hop2_memory,omitempty"`
	SecondVersion    *kernel.Memory `json:"second_version,omitempty"`
}

// holdoutItem 输出语料格式（polaris_eval 的 HoldoutItem 字段子集）。
type holdoutItem struct {
	ID                string         `json:"id"`
	Query             string         `json:"query"`
	Expected          string         `json:"expected"`
	ExpectedMemoryIDs []string       `json:"expected_memory_ids"`
	Bucket            string         `json:"bucket"`
	Tenant            string         `json:"tenant"`
	User              string         `json:"user"`
	AsOf              string         `json:"as_of,omitempty"`
	Memory            *kernel.Memory `json:"memory,omitempty"`
	Hop1Memory        *kernel.Memory `json:"hop1_memory,omitempty"`
	Hop2Memory        *kernel.Memory `json:"hop2_memory,omitempty"`
	SecondVersion     *kernel.Memory `json:"second_version,omitempty"`
}

func main() {
	out := flag.String("out", "../dataset/holdout_proxy.jsonl", "输出 holdout 代理语料路径")
	flag.Parse()
	srcs := []string{
		"../dataset/all.jsonl",
		"../dataset/reference_frame.jsonl",
		"../dataset/temporal.jsonl",
	}
	seen := map[string]bool{}
	var out_items []holdoutItem
	adversarial, dup := 0, 0
	for _, src := range srcs {
		f, err := os.Open(src)
		if err != nil {
			log.Printf("skip %s: %v", src, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		n := 0
		for sc.Scan() {
			line := sc.Text()
			if len(line) == 0 {
				continue
			}
			var it srcItem
			if err := json.Unmarshal([]byte(line), &it); err != nil {
				log.Fatalf("parse %s: %v", src, err)
			}
			n++
			if it.HasInjection || it.HasPollution {
				adversarial++
				continue
			}
			if it.ID == "" || it.Query == "" || it.ExpectedFact == "" {
				continue
			}
			if seen[it.ID] {
				dup++
				continue
			}
			seen[it.ID] = true
			out_items = append(out_items, holdoutItem{
				ID:                it.ID,
				Query:             it.Query,
				Expected:          it.ExpectedFact,
				ExpectedMemoryIDs: it.ExpectedMemoryID,
				Bucket:            it.Bucket,
				Tenant:            it.Tenant,
				User:              it.User,
				AsOf:              it.AsOf,
				Memory:            it.Memory,
				Hop1Memory:        it.Hop1Memory,
				Hop2Memory:        it.Hop2Memory,
				SecondVersion:     it.SecondVersion,
			})
		}
		if err := sc.Err(); err != nil {
			log.Fatalf("scan %s: %v", src, err)
		}
		_ = f.Close()
		log.Printf("read %s: %d items", src, n)
	}
	// 写出（LF 换行；文件序即确定性）。
	wf, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create %s: %v", *out, err)
	}
	bw := bufio.NewWriter(wf)
	enc := json.NewEncoder(bw)
	for i := range out_items {
		if err := enc.Encode(&out_items[i]); err != nil {
			log.Fatalf("encode: %v", err)
		}
	}
	if err := bw.Flush(); err != nil {
		log.Fatalf("flush: %v", err)
	}
	if err := wf.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}
	buckets := map[string]int{}
	for _, it := range out_items {
		buckets[it.Bucket]++
	}
	fmt.Printf("holdout proxy written: %s\n  items=%d adversarial_skipped=%d dup_skipped=%d\n  buckets=%v\n",
		*out, len(out_items), adversarial, dup, buckets)
}
