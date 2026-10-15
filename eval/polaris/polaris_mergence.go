//go:build ignore

// polaris_mergence.go — R13-B 路合并度（mergence）指标基线 runner（独立主程序）。
//
// 带 //go:build ignore，不进入 go build ./... / go test ./...；运行：
//
//	go run eval/polaris/polaris_mergence.go \
//	  -holdout ./eval/holdout/holdout_v2.jsonl \
//	  -report r13b_mergence_baseline
//
// 两个指标（B.5）：
//  1. mean_edges_per_memory = 有效位移边数 / L2 记忆数。
//     R13 首次跑时 holdout 记忆全为 L1、reference_frame 无 entity/task、recorded_at
//     为零值，启发式建边结果为 0——此即「后续建边质量对照基线」。
//  2. mean_memories_per_entity = 按 entity 字符串归一化（小写去标点）分组后，
//     每组记忆数的均值。本期 B 路自走极简字符串分组，不依赖 A.3 entity_align。
//
// 确定性：固定 clock 2026-10-14；不调 LLM；同输入逐位一致。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

// mergeItem 与 polaris_holdout.go 同构的最小子集（只取记忆相关字段）。
type mergeItem struct {
	ID            string         `json:"id"`
	Memory        *kernel.Memory `json:"memory,omitempty"`
	Hop1Memory    *kernel.Memory `json:"hop1_memory,omitempty"`
	Hop2Memory    *kernel.Memory `json:"hop2_memory,omitempty"`
	SecondVersion *kernel.Memory `json:"second_version,omitempty"`
}

type emergenceReport struct {
	Report                string    `json:"report"`
	GeneratedAt           time.Time `json:"generated_at"`
	HoldoutPath           string    `json:"holdout_path"`
	TotalMemories         int       `json:"total_memories"`
	L2Memories            int       `json:"l2_memories"`
	L1Memories            int       `json:"l1_memories"`
	EdgesTotalRows        int       `json:"edges_total_rows"`
	EdgesAlive            int       `json:"edges_alive"`
	EdgesInvalidated      int       `json:"edges_invalidated"`
	MeanEdgesPerMemory    float64   `json:"mean_edges_per_memory"`
	MemoriesWithEntity    int       `json:"memories_with_entity"`
	EntityGroups          int       `json:"entity_groups"`
	MeanMemoriesPerEntity float64   `json:"mean_memories_per_entity"`
	Note                  string    `json:"note"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[mergence] ")

	holdoutFlag := flag.String("holdout", "", "评测集 NDJSON（默认 eval/holdout/holdout_v2.jsonl）")
	reportFlag := flag.String("report", "r13b_mergence_baseline", "报告文件名前缀")
	reportsDir := flag.String("reports-dir", "", "报告输出目录（默认自动探测 eval/reports）")
	nowFixed := flag.String("now-fixed", "2026-10-14", "建边 clock 固定基准日")
	flag.Parse()

	holdoutPath := *holdoutFlag
	if holdoutPath == "" {
		holdoutPath = resolvePath("eval/holdout/holdout_v2.jsonl", "../holdout/holdout_v2.jsonl")
	}
	if holdoutPath == "" {
		log.Fatal("未找到 holdout（用 -holdout 指定）")
	}

	items, err := loadMergeItems(holdoutPath)
	if err != nil {
		log.Fatalf("load %s: %v", holdoutPath, err)
	}
	log.Printf("loaded %d items from %s", len(items), holdoutPath)

	// 收集全部记忆节点（每条 holdout 可能含主记忆 + hop1/hop2/第二版本）。
	mems := collectMemories(items)

	// 固定 clock：建边时间确定。
	now, err := time.Parse("2006-01-02", *nowFixed)
	if err != nil {
		log.Fatalf("parse -now-fixed: %v", err)
	}
	store := kernel.NewEdgeStore(0, func() time.Time { return now.UTC() })
	added := kernel.BuildEdgesHeuristic(mems, store)
	sum := store.Summarize()

	l2, l1 := 0, 0
	entityGroup := map[string]int{}
	for _, m := range mems {
		switch m.Layer {
		case kernel.LayerL2:
			l2++
		case kernel.LayerL1:
			l1++
		}
		if e := normalizeEntity(m.ReferenceFrame.Entity); e != "" {
			entityGroup[e]++
		}
	}

	meanEdges := 0.0
	if l2 > 0 {
		meanEdges = float64(sum.AliveRows) / float64(l2)
	}
	meanPerEntity := 0.0
	memWithEntity := 0
	for _, c := range entityGroup {
		memWithEntity += c
	}
	if len(entityGroup) > 0 {
		meanPerEntity = float64(memWithEntity) / float64(len(entityGroup))
	}

	rep := emergenceReport{
		Report:                *reportFlag,
		GeneratedAt:           now.UTC(),
		HoldoutPath:           holdoutPath,
		TotalMemories:         len(mems),
		L2Memories:            l2,
		L1Memories:            l1,
		EdgesTotalRows:        sum.TotalRows,
		EdgesAlive:            sum.AliveRows,
		EdgesInvalidated:      sum.InvalidatedRows,
		MeanEdgesPerMemory:    meanEdges,
		MemoriesWithEntity:    memWithEntity,
		EntityGroups:          len(entityGroup),
		MeanMemoriesPerEntity: meanPerEntity,
		Note: fmt.Sprintf(
			"R13-B 首次基线：BuildEdgesHeuristic 实际新增 %d 边；holdout 多为 L1 且无 entity/task，"+
				"mean_edges_per_memory 分母(L2)=%d、非空实体组数=%d，作为后续建边质量对照。",
			added, l2, len(entityGroup)),
	}

	dir := resolveReportsDir(*reportsDir)
	_ = os.MkdirAll(dir, 0o755)
	writeReport(dir, *reportFlag, rep)
	log.Printf("done: mean_edges_per_memory=%.4f mean_memories_per_entity=%.4f edges_alive=%d",
		rep.MeanEdgesPerMemory, rep.MeanMemoriesPerEntity, rep.EdgesAlive)
}

func collectMemories(items []mergeItem) []kernel.Memory {
	seen := map[string]bool{}
	out := make([]kernel.Memory, 0, len(items)*2)
	add := func(m *kernel.Memory) {
		if m == nil || m.MemoryID == "" || seen[m.MemoryID] {
			return
		}
		seen[m.MemoryID] = true
		out = append(out, *m)
	}
	for _, it := range items {
		add(it.Memory)
		add(it.Hop1Memory)
		add(it.Hop2Memory)
		add(it.SecondVersion)
	}
	return out
}

func loadMergeItems(path string) ([]mergeItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []mergeItem
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it mergeItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		items = append(items, it)
	}
	return items, nil
}

// normalizeEntity 与 kernel.normalizeEntity 同口径（小写去标点）；本 runner 独立实现，
// 避免依赖未导出函数。
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

func resolvePath(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func resolveReportsDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if d := resolvePath("eval/reports", "../reports", "reports"); d != "" {
		return d
	}
	return "reports"
}

func writeReport(dir, prefix string, v any) {
	// 时间戳用固定基准日派生（确定性文件名到秒级由运行时刻决定，与其它 polaris runner 一致）。
	ts := time.Now().UTC().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.json", prefix, ts))
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	log.Printf("report written: %s", path)
}
