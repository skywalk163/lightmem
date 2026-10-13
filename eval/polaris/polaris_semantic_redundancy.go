// R7-A semantic 单点冗余评测（Q1）：逐路径消融 + 冗余度度量 + R6-D 对照。
//
// 目标（任务书 §A.5）：
//   - 多路径开启后整体 M1 不降（对齐 15fb854 北极星基线）；
//   - 任一路关闭降幅 ≤5pp（原 semantic 单点降 20.68pp，红线 5pp）；
//   - 与 R6-D 单路径消融同口径对照。
//
// 本文件是**常规包文件**（无 main），由 polaris_eval.go 分发调用，
// 复用 polaris_validity.go 的 runEvalOnce / M1 口径，逐位对齐 R6-D 消融。
//
// 确定性：无 shuffle / 无 time.Now / 无 map 迭代序；路径顺序固定。
package main

import (
	"context"
	"log"

	"github.com/lightmem/kernel"
)

// SemanticRedundancyReport R7-A 语义多路径冗余评测报告。
type SemanticRedundancyReport struct {
	// Baseline 默认态（redundant=false，对齐 R6-D 消融基线）。
	Baseline M1Report `json:"baseline"`
	// MultiOn 多路径开启态（三路投票，不消融）。
	MultiOn M1Report `json:"multi_on"`
	// Ablations 逐路径消融结果（redundant=true 下关一路）。
	Ablations []SemRedundantAblation `json:"ablations"`
	// DeltaCoveragePP 消融后 M1 降幅（百分点），负=降、正=升。
	DeltaCoveragePP float64 `json:"delta_coverage_pp"`
	// Pass 任一路关闭降幅 ≤5pp（对齐 R6-D 5pp 红线）。
	Pass bool `json:"pass"`
	// R6DBaselineCov R6-D 单路径消融的 semantic 基线 M1（0.7458）。
	R6DBaselineCov float64 `json:"r6d_baseline_coverage"`
	// R6DSemanticDelta R6-D 单路径消融 semantic 的降幅（20.68pp，对照）。
	R6DSemanticDelta float64 `json:"r6d_semantic_delta_pp"`
}

// SemRedundantAblation 单条语义路径消融结果。
type SemRedundantAblation struct {
	Path       string  `json:"path"`
	M1Coverage float64 `json:"m1_coverage"`
	DeltaCovPP float64 `json:"delta_coverage_pp"` // 基线 - 消融（百分点）
	Pass       bool    `json:"pass"`
}

// runSemanticRedundancy R7-A 语义多路径冗余评测主入口。
//
// 口径：与 R6-D 消融同链路（polaris_eval.go runEvalOnce + r6_ref 权重 + 默认 embedder auto）。
// 步骤：
//  1. 基线（SemanticRedundant=false）：对齐 R6-D 单路径消融基线 0.9525。
//  2. 多路径开启（redundant=true，不消融）：三路投票整体 M1。
//  3. 逐路径消融（redundant=true + ablate-path）：分别关 path0/path1/path2。
//
// 输出：JSON 报告 + log 明细。
func runSemanticRedundancy(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion,
	holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string,
	topK int, compressed bool, dir, prefix string, ablatePath string) {

	log.Printf("[sem-redun] R7-A 语义多路径冗余评测开始 (weights=%s embedder=%s)",
		weightsPath, embedderName)

	// ---- 步骤 1：基线（默认态，redundant=false） ----
	rf.SemanticRedundant = false
	rf.SemanticAblatePath = ""
	baseM1, _ := runEvalOnce(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_semredun_baseline")

	// ---- 步骤 2：多路径开启（redundant=true，不消融） ----
	rf.SemanticRedundant = true
	rf.SemanticAblatePath = ""
	multiM1, _ := runEvalOnce(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_semredun_multi")

	// ---- 步骤 3：逐路径消融 ----
	paths := kernel.SemanticPathNames()
	ablations := make([]SemRedundantAblation, 0, len(paths))
	maxDelta := 0.0
	passAll := true
	for _, p := range paths {
		rf.SemanticRedundant = true
		rf.SemanticAblatePath = p
		abM1, _ := runEvalOnce(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_semredun_ablate_"+p)
		delta := (baseM1.Coverage - abM1.Coverage) * 100.0 // 百分点
		pass := delta <= 5.0
		if !pass {
			passAll = false
		}
		if delta > maxDelta {
			maxDelta = delta
		}
		ablations = append(ablations, SemRedundantAblation{
			Path:       p,
			M1Coverage: abM1.Coverage,
			DeltaCovPP: delta,
			Pass:       pass,
		})
		log.Printf("[sem-redun] ablate %-16s M1=%.4f delta=%+.2fpp %s", p, abM1.Coverage, delta, boolPass(pass))
	}

	// 恢复默认态（不污染后续）
	rf.SemanticRedundant = false
	rf.SemanticAblatePath = ""
	srv.SetRankFusion(rf)

	// ---- 报告 ----
	rep := SemanticRedundancyReport{
		Baseline:         baseM1,
		MultiOn:          multiM1,
		Ablations:        ablations,
		DeltaCoveragePP:  maxDelta,
		Pass:             passAll,
		R6DBaselineCov:   0.7458, // R6-D 消融：关闭 semantic 后 M1
		R6DSemanticDelta: 20.68,  // R6-D 消融：关闭 semantic 降幅
	}
	writeJSONReport(dir, prefix+"_semantic_redundancy", rep)

	log.Printf("[sem-redun] 基线 M1=%.4f / 多路径 M1=%.4f / 最大降幅=%.2fpp / PASS=%v",
		baseM1.Coverage, multiM1.Coverage, maxDelta, passAll)
	log.Printf("[sem-redun] R7-A 语义多路径冗余评测完成")
}

func boolPass(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}
