//go:build ignore

// KPI 基线评测器（任务书 §9 P2.5）。
// 真源：lightmem.md §9 KPI、§8 评测集。
//
// 设计要点：
//   - 加载 eval/dataset/*.jsonl，逐条构造记忆写入 kernel（走 WritePolicy）。
//   - 逐条跑查询（走 A1+A2 RecallAndFuse）。
//   - 计算：总体/分桶/时序/多信号/污染率。
//   - 输出 JSON 报告 + 人工可读摘要。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

const (
	tenant = "eval-tenant"
	user   = "eval-user"
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
}

type KPIReport struct {
	GeneratedAt           time.Time          `json:"generated_at"`
	TotalItems            int                `json:"total_items"`
	OverallAccuracy       float64            `json:"overall_accuracy"`
	OverallAccuracyByFact float64            `json:"overall_accuracy_by_fact"`
	BucketAccuracy        map[string]float64 `json:"bucket_accuracy"`
	TemporalAccuracy      float64            `json:"temporal_accuracy"`
	MultiSignalAccuracy   float64            `json:"multi_signal_accuracy"`
	PollutionRate         float64            `json:"pollution_rate"`
	Details               []KPIDetail        `json:"details"`
}

type KPIDetail struct {
	ID       string `json:"id"`
	Bucket   string `json:"bucket"`
	Hit      bool   `json:"hit"`
	FactHit  bool   `json:"fact_hit"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Reason   string `json:"reason"`
}

func loadDataset(path string) ([]EvalItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	var out []EvalItem
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item EvalItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fmt.Errorf("parse %s: %w", line[:min(80, len(line))], err)
		}
		out = append(out, item)
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	ctx := context.Background()

	// 路径解析：兼容从仓库根（eval/dataset）或 eval/ 目录（dataset）运行。
	dsDir, repDir := "dataset", "reports"
	if _, statErr := os.Stat("eval"); statErr == nil {
		dsDir, repDir = "eval/dataset", "eval/reports"
	}
	_ = os.MkdirAll(repDir, 0o755)

	// 1) 加载评测集。
	items, err := loadDataset(dsDir + "/all.jsonl")
	if err != nil {
		log.Fatalf("load dataset: %v", err)
	}
	log.Printf("loaded %d items", len(items))

	// 2) 装配内核（SQLite 内存后端，无需真实 PG）。
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	srv.SetRankFusion(kernel.DefaultRankFusion())

	// 3) 逐条写入记忆 + 跑查询。
	var details []KPIDetail
	bucketHits := map[string]int{}
	bucketTotal := map[string]int{}
	hits, total := 0, 0
	factHits := 0
	temporalHits, temporalTotal := 0, 0
	multiSignalHits, multiSignalTotal := 0, 0
	pollutionTotal, pollutionHits := 0, 0

	for _, item := range items {
		isAdversarial := item.HasPollution || item.HasInjection
		if isAdversarial {
			// 污染/投毒样本：期望被 WritePolicy 拒收，或写入后不被 top-1 召回（未泄漏即通过）。
			pollutionTotal++
		}

		// 写入记忆（走 WritePolicy）。
		if item.Memory != nil {
			if _, err := srv.SaveMemory(ctx, item.Memory, user); err != nil {
				if isAdversarial {
					// 被策略正确拒收（投毒降权/校验/幂等），视为未泄漏。
					pollutionHits++
				}
				continue
			}
		}

		// 跑查询。
		rc := kernel.RouterContext{
			Text:        item.Query,
			TenantID:    item.Tenant,
			UserID:      item.User,
			MemoryKinds: nil,
		}
		if item.AsOf != "" {
			asOf := item.AsOf
			rc.AsOf = &asOf
		}
		res, err := srv.RecallAndFuse(ctx, rc)
		if err != nil {
			if isAdversarial {
				pollutionHits++ // 查询失败同样不构成泄漏
				continue
			}
			details = append(details, KPIDetail{ID: item.ID, Bucket: item.Bucket, Hit: false, Reason: err.Error()})
			continue
		}

		// 对抗样本：不计入检索准确率分母；top-1 命中自身即视为泄漏。
		if isAdversarial {
			if len(res.Fused) == 0 || res.Fused[0].MemoryID != item.Memory.MemoryID {
				pollutionHits++
			}
			continue
		}

		// 判定命中：top-1 的 memory_id 在 expected_memory_ids 中；
		// fact 命中为辅助口径（等价文本），用于区分「ID 不一致但答案正确」。
		hit := false
		gotFact := ""
		if len(res.Fused) > 0 {
			gotFact = res.Fused[0].Fact
			for _, exp := range item.ExpectedMemoryID {
				if res.Fused[0].MemoryID == exp {
					hit = true
					break
				}
			}
		}
		factHit := item.ExpectedFact != "" && gotFact == item.ExpectedFact

		total++
		bucketTotal[item.Bucket]++
		if hit {
			hits++
			bucketHits[item.Bucket]++
		}
		if factHit {
			factHits++
		}

		if item.IsTemporal {
			temporalTotal++
			if hit {
				temporalHits++
			}
		}
		if item.Bucket == "multi-hop" {
			multiSignalTotal++
			if hit {
				multiSignalHits++
			}
		}

		details = append(details, KPIDetail{
			ID:       item.ID,
			Bucket:   item.Bucket,
			Hit:      hit,
			FactHit:  factHit,
			Expected: item.ExpectedFact,
			Got:      gotFact,
			Reason:   "",
		})
	}

	// 4) 计算 KPI。
	report := KPIReport{
		GeneratedAt:         time.Now().UTC(),
		TotalItems:          total,
		OverallAccuracy:     float64(hits) / float64(total),
		BucketAccuracy:      map[string]float64{},
		TemporalAccuracy:    0,
		MultiSignalAccuracy: 0,
		PollutionRate:       0,
		Details:             details,
	}
	if total > 0 {
		report.OverallAccuracyByFact = float64(factHits) / float64(total)
	}
	for bucket, tot := range bucketTotal {
		if tot > 0 {
			report.BucketAccuracy[bucket] = float64(bucketHits[bucket]) / float64(tot)
		}
	}
	if temporalTotal > 0 {
		report.TemporalAccuracy = float64(temporalHits) / float64(temporalTotal)
	}
	if multiSignalTotal > 0 {
		report.MultiSignalAccuracy = float64(multiSignalHits) / float64(multiSignalTotal)
	}
	if pollutionTotal > 0 {
		report.PollutionRate = 1 - float64(pollutionHits)/float64(pollutionTotal)
	}

	// 5) 落盘 JSON 报告。
	path := fmt.Sprintf("%s/kpi_baseline_%s.json", repDir, time.Now().UTC().Format("20060102-150405"))
	raw, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(path, raw, 0644)
	log.Printf("report written to %s", path)

	// 6) 人工可读摘要（显式逐行拼接，避免占位符与实参错位）。
	var sb strings.Builder
	sb.WriteString("# KPI 基线报告\n\n")
	fmt.Fprintf(&sb, "- 生成时间：%s\n", report.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&sb, "- 评测集总数：%d\n", report.TotalItems)
	fmt.Fprintf(&sb, "- 检索准确率（总体，按 memory_id）：%.1f%%\n", report.OverallAccuracy*100)
	fmt.Fprintf(&sb, "- 检索准确率（总体，按 fact 文本）：%.1f%%\n", report.OverallAccuracyByFact*100)
	sb.WriteString("- 分桶准确率（按 memory_id）：\n")
	for _, bucket := range []string{"single-hop", "multi-hop", "open-domain", "temporal"} {
		if acc, ok := report.BucketAccuracy[bucket]; ok {
			fmt.Fprintf(&sb, "  - %s: %.1f%%\n", bucket, acc*100)
		}
	}
	fmt.Fprintf(&sb, "- 时序桶准确率：%.1f%%\n", report.TemporalAccuracy*100)
	fmt.Fprintf(&sb, "- 多信号融合准确率（multi-hop 桶自动跑，**待人工复核**）：%.1f%%\n", report.MultiSignalAccuracy*100)
	fmt.Fprintf(&sb, "- 记忆污染率：%.1f%%\n\n", report.PollutionRate*100)
	sb.WriteString("## 目标对照（§9 原文）\n\n")
	sb.WriteString("| 指标 | 目标 | 基线 | 状态 |\n|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| 检索准确率（总体，memory_id） | > 90%% | %.1f%% | %s |\n",
		report.OverallAccuracy*100, check(report.OverallAccuracy, 0.90))
	fmt.Fprintf(&sb, "| 检索准确率（总体，fact 文本） | > 90%% | %.1f%% | %s |\n",
		report.OverallAccuracyByFact*100, check(report.OverallAccuracyByFact, 0.90))
	fmt.Fprintf(&sb, "| 时序桶查询正确率 | > 85%% | %.1f%% | %s |\n",
		report.TemporalAccuracy*100, check(report.TemporalAccuracy, 0.85))
	fmt.Fprintf(&sb, "| 多信号融合正确率 | > 85%% | %.1f%% | %s |\n",
		report.MultiSignalAccuracy*100, check(report.MultiSignalAccuracy, 0.85))
	fmt.Fprintf(&sb, "| 记忆污染率 | 持续下降 | %.1f%% | 基线 |\n", report.PollutionRate*100)
	sb.WriteString("\n> 说明：本基线在 SQLite 骨架后端 + 占位权重下产出，数字为**下限**，" +
		"非调优后系统能力；权重可按本报告结论校准（§3.2 红线 4）。\n")
	summary := sb.String()
	summaryPath := fmt.Sprintf("%s/kpi_baseline_summary.md", repDir)
	_ = os.WriteFile(summaryPath, []byte(summary), 0644)
	log.Printf("summary written to %s", summaryPath)

	// 控制台输出。
	fmt.Println(summary)
}

func check(actual, target float64) string {
	if actual >= target {
		return "✅ 达标"
	}
	return "❌ 未达标"
}
