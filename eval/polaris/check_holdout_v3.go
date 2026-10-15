//go:build ignore

// check_holdout_v3.go — R15-C 路 holdout_v3.jsonl 语料自检（确定性、仅标准库）。
//
// 带 //go:build ignore，不进入 go build ./... / go test ./...；运行：
//
//	go run eval/polaris/check_holdout_v3.go -holdout eval/holdout/holdout_v3.jsonl
//
// 断言全部写死为常量；任何一条不满足即非零退出，并打印实际值。
// 口径与 polaris_mergence.go 完全一致（实体归一化同口径），但本文件不 import kernel。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// ---- 写死的验收常量（任务书 C.3）----
const (
	wantMinRows         = 800  // 语料总条数 ≥800
	wantMinL2           = 120  // L2 记忆 ≥120
	wantMinAxisRate     = 0.90 // entity/scene/task 三轴非空率 ≥90%
	wantMinEntityGroups = 20   // 实体分组数 ≥20
	wantMinPerGroup     = 10   // 每组记忆 ≥10 条
	wantRecordedNonZero = 1.00 // recorded_at 非零时间戳率 = 100%
)

type refFrame struct {
	Entity string `json:"entity"`
	Scene  string `json:"scene"`
	Task   string `json:"task"`
}

type mem struct {
	Layer          string    `json:"layer"`
	Fact           string    `json:"fact"`
	ReferenceFrame refFrame  `json:"reference_frame"`
	RecordedAt     time.Time `json:"recorded_at"`
	MemoryID       string    `json:"memory_id"`
}

type item struct {
	ID            string `json:"id"`
	Memory        *mem   `json:"memory,omitempty"`
	Hop1Memory    *mem   `json:"hop1_memory,omitempty"`
	Hop2Memory    *mem   `json:"hop2_memory,omitempty"`
	SecondVersion *mem   `json:"second_version,omitempty"`
}

// normalizeEntity 与 kernel.normalizeEntity / polaris_mergence.go 同口径：
// 小写 + 去标点/空白/符号。
func normalizeEntity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if r <= 0x20 || r == '.' || r == ',' || r == '!' || r == '?' || r == ';' ||
			r == ':' || r == '"' || r == '\'' || r == '(' || r == ')' || r == '[' || r == ']' ||
			r == '{' || r == '}' || r == '-' || r == '_' || r == '/' || r == '\\' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func main() {
	holdoutFlag := flag.String("holdout", "eval/holdout/holdout_v3.jsonl", "待自检的 holdout NDJSON")
	flag.Parse()

	raw, err := os.ReadFile(*holdoutFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL read:", err)
		os.Exit(2)
	}

	var rows []item
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			fmt.Fprintln(os.Stderr, "FATAL parse line:", err)
			os.Exit(2)
		}
		rows = append(rows, it)
	}

	// 收集记忆（与 polaris_mergence.collectMemories 同口径，按 MemoryID 去重）。
	seen := map[string]bool{}
	mems := []mem{}
	add := func(m *mem) {
		if m == nil || m.MemoryID == "" || seen[m.MemoryID] {
			return
		}
		seen[m.MemoryID] = true
		mems = append(mems, *m)
	}
	for _, it := range rows {
		add(it.Memory)
		add(it.Hop1Memory)
		add(it.Hop2Memory)
		add(it.SecondVersion)
	}

	total := len(mems)
	l2, entOK, sceneOK, taskOK, recordedNonZero := 0, 0, 0, 0, 0
	groups := map[string]int{}
	for _, m := range mems {
		if m.Layer == "L2" {
			l2++
		}
		if e := normalizeEntity(m.ReferenceFrame.Entity); e != "" {
			entOK++
			groups[e]++
		}
		if strings.TrimSpace(m.ReferenceFrame.Scene) != "" {
			sceneOK++
		}
		if strings.TrimSpace(m.ReferenceFrame.Task) != "" {
			taskOK++
		}
		if !m.RecordedAt.IsZero() {
			recordedNonZero++
		}
	}

	rate := func(n int) float64 { return float64(n) / float64(total) }
	minPerGroup := 1 << 30
	for _, c := range groups {
		if c < minPerGroup {
			minPerGroup = c
		}
	}
	if len(groups) == 0 {
		minPerGroup = 0
	}

	type check struct {
		name string
		ok   bool
		got  string
		want string
	}
	checks := []check{
		{"rows >= min", total >= wantMinRows, fmt.Sprintf("%d", total), fmt.Sprintf(">=%d", wantMinRows)},
		{"l2 >= min", l2 >= wantMinL2, fmt.Sprintf("%d", l2), fmt.Sprintf(">=%d", wantMinL2)},
		{"entity non-empty rate", rate(entOK) >= wantMinAxisRate, fmt.Sprintf("%.4f", rate(entOK)), fmt.Sprintf(">=%.2f", wantMinAxisRate)},
		{"scene non-empty rate", rate(sceneOK) >= wantMinAxisRate, fmt.Sprintf("%.4f", rate(sceneOK)), fmt.Sprintf(">=%.2f", wantMinAxisRate)},
		{"task non-empty rate", rate(taskOK) >= wantMinAxisRate, fmt.Sprintf("%.4f", rate(taskOK)), fmt.Sprintf(">=%.2f", wantMinAxisRate)},
		{"entity groups >= min", len(groups) >= wantMinEntityGroups, fmt.Sprintf("%d", len(groups)), fmt.Sprintf(">=%d", wantMinEntityGroups)},
		{"memories per group >= min", minPerGroup >= wantMinPerGroup, fmt.Sprintf("%d", minPerGroup), fmt.Sprintf(">=%d", wantMinPerGroup)},
		{"recorded_at non-zero rate", rate(recordedNonZero) >= wantRecordedNonZero, fmt.Sprintf("%.4f", rate(recordedNonZero)), fmt.Sprintf(">=%.2f", wantRecordedNonZero)},
	}

	failures := 0
	fmt.Printf("check_holdout_v3: %s\n", *holdoutFlag)
	fmt.Printf("  rows=%d memories=%d l2=%d entity_groups=%d min_per_group=%d\n",
		len(rows), total, l2, len(groups), minPerGroup)
	fmt.Printf("  axis rates: entity=%.4f scene=%.4f task=%.4f recorded_nonzero=%.4f\n",
		rate(entOK), rate(sceneOK), rate(taskOK), rate(recordedNonZero))
	for _, c := range checks {
		status := "PASS"
		if !c.ok {
			status = "FAIL"
			failures++
		}
		fmt.Printf("  [%s] %-28s got=%-12s want=%s\n", status, c.name, c.got, c.want)
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "check_holdout_v3: %d assertion(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("check_holdout_v3: ALL PASS")
}
