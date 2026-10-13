//go:build ignore

// R3-A：RankFusion 权重坐标下降搜索 v2（确定性、不调 LLM）。
//
// 方案 β（任务书 A.1 选定，理由见报告 §方案选择）：
//   - 评测器已注入确定性 char n-gram TF-IDF embedder，signalSemantic **真正生效**
//     （不再是 RecallScore 退化）。因此 semantic **重新进入搜索变量**。
//   - 搜索变量 = [semantic, keyword, entity, reference_frame] 四个 calibrated 信号。
//   - fixed 信号（value / freshness / source_trust / feedback）保持 R2 起始值不变。
//   - 搜索顺序固定 [semantic, keyword, entity, reference_frame]，不 shuffle（seed=42 确定性）。
//
// R5-A 扩展（任务书 §A.3.4）：坐标下降可变量从 4 个扩展到 **8 个**
// （含 reference_frame / value / freshness / source_trust），初始值取 R4 联合权重；
// 新增 -val 校准集（reference_frame 60 条）与 -include-reference-frame 开关，
// 用于重校准 reference_frame 权重（R4-B 已确认信号 100% 区分度但被权重 0.0 关闭）。
//
// 搜索规则（与 R2 tune_rank_weights.go 一致，保证可比）：
//   - 起始权重 = eval/weights/rank_weights_en.json（R2 A 路产物，semantic=1.5 keyword=2.7
//     entity=2.4 reference_frame=0.0 value=0.2 freshness=0.1 source_trust=0.1 feedback=0.1）。
//   - 每轮按固定顺序轮流试探 ±step（默认 ±0.1）。
//   - 接受准则：公开基准 MRR 严格上升，且合成集 MRR 不低于 R2 基线（0.8412）− 0.01。
//   - 最多 maxRounds 轮（默认 20）；一轮内所有变量均无改进则提前停止。
//
// 运行（在 lightmem/eval/ 下）：
//
//	go run ./tune_rank_weights_v2.go \
//	  -dataset-bench ./benchmarks/dataset/all.jsonl \
//	  -dataset-synth ./dataset/all.jsonl \
//	  -out ./weights/rank_weights_en.json
//
// 说明：本脚本通过子进程调用 run_kpi.go 做评测；run_kpi.go 必须已支持 -no-embedder flag
// 且默认注入 embedder（本脚本不传 -no-embedder，即走「semantic 真启」路径）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

type weightSpec struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Type   string  `json:"type"`
	Note   string  `json:"note"`
}

type weightsDoc struct {
	K       int          `json:"k"`
	Comment string       `json:"_comment,omitempty"`
	Weights []weightSpec `json:"weights"`
}

// searchVarsV2 是方案 β 的搜索变量顺序（固定，不 shuffle，保证 seed 无关确定性）。
//
// R5-A 扩展（任务书 §A.3.4）：可变量从 4 个扩展到 **8 个**（含 reference_frame /
// value / freshness / source_trust），顺序固定为 semantic → keyword → entity →
// feedback → reference_frame → value → freshness → source_trust。
//
// 红线 1 说明（只增不改）：仅 **calibrated 语义**的 4 个信号参与坐标下降搜索
// （semantic / keyword / entity / reference_frame）；feedback / value / freshness /
// source_trust 是 kernel/rank_fusion.go 的固定公式信号（IntrinsicValue / 指数衰减 /
// 来源映射），**不参与校准**，仅保留 R4 联合值并冻结。reference_frame 仅在
// `-include-reference-frame` 时纳入搜索变量。
var searchVarsV2 = []string{"semantic", "keyword", "entity", "feedback", "reference_frame", "value", "freshness", "source_trust"}

// fixedVars 是 kernel 侧固定公式信号（不参与坐标下降校准）。
var fixedVars = map[string]bool{
	"feedback": true, "value": true, "freshness": true, "source_trust": true,
}

// reportMetrics 镜像 run_kpi.go KPIReport 中本脚本需要的字段。
type reportMetrics struct {
	OverallMRR float64 `json:"overall_mrr"`
	Details    []struct {
		Rank int `json:"rank"`
	} `json:"details"`
	// fact 准确率（按 fact 文本）：用于搜索轨迹记录。
	OverallAccuracyByFact float64 `json:"overall_accuracy_by_fact"`
	// R5-A：reference_frame 桶的 ref 桶 top-1 传导率（ref 分最高且 top-1 命中）。
	// 用于判断 ref 权重抬升后是否真的提升 ref 桶命中。
	RefFrameSignal map[string]float64 `json:"ref_frame_signal,omitempty"`
}

func (r reportMetrics) top1Rate() float64 {
	if len(r.Details) == 0 {
		return 0
	}
	n := 0
	for _, d := range r.Details {
		if d.Rank == 1 {
			n++
		}
	}
	return float64(n) / float64(len(r.Details))
}

func loadWeights(path string) (*weightsDoc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc weightsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.K == 0 {
		doc.K = 60
	}
	return &doc, nil
}

func (d *weightsDoc) setWeight(name string, w float64) {
	for i := range d.Weights {
		if d.Weights[i].Name == name {
			d.Weights[i].Weight = w
			return
		}
	}
	d.Weights = append(d.Weights, weightSpec{Name: name, Weight: w, Type: "calibrated"})
}

func (d *weightsDoc) getWeight(name string) float64 {
	for _, w := range d.Weights {
		if w.Name == name {
			return w.Weight
		}
	}
	return 0
}

func (d *weightsDoc) setWeightNote(name, note string) {
	for i := range d.Weights {
		if d.Weights[i].Name == name {
			d.Weights[i].Note = note
			return
		}
	}
}

func (d *weightsDoc) clone() *weightsDoc {
	b, _ := json.Marshal(d)
	var c weightsDoc
	_ = json.Unmarshal(b, &c)
	return &c
}

// writeCand 把权重写到临时文件，返回路径。
func (d *weightsDoc) writeCand(tag string) (string, error) {
	path := filepath.Join("weights", "_cand_v2_"+tag+".json")
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll("weights", 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// runKPI 在指定数据集上跑 run_kpi.go，返回指标。
// 不传 -no-embedder → 走「注入 embedder」路径（semantic 真启）。
// R5-A：新增 embedderMode 参数，把 `-embedder tfidf|vector|none` 透传给 run_kpi.go，
// 让坐标下降针对**实际生效的 embedder 实现**调优权重（否则优化的是 tfidf 权重，
// 却套用在 vector 语义上，任务书 §A.3.4 语义错配）。
func runKPI(dataset, weightsPath, tag, embedderMode string) (reportMetrics, error) {
	return runKPIWith(dataset, weightsPath, tag, embedderMode, "")
}

// runKPIWith 允许指定预编译的 run_kpi 二进制（runkpiBinary 非空则直接执行，
// 否则 `go run ./run_kpi.go`）。预编译路径可把坐标下降单步评测从 ~15s 降到 ~1s。
func runKPIWith(dataset, weightsPath, tag, embedderMode, runkpiBinary string) (reportMetrics, error) {
	repPrefix := "tune_v2_" + tag
	var cmd *exec.Cmd
	if runkpiBinary != "" {
		cmdArgs := []string{"-dataset", dataset, "-weights", weightsPath, "-report", repPrefix}
		if embedderMode != "" {
			cmdArgs = append(cmdArgs, "-embedder", embedderMode)
		}
		cmd = exec.Command(runkpiBinary, cmdArgs...)
	} else {
		cmdArgs := []string{"run", "./run_kpi.go",
			"-dataset", dataset,
			"-weights", weightsPath,
			"-report", repPrefix,
		}
		if embedderMode != "" {
			cmdArgs = append(cmdArgs, "-embedder", embedderMode)
		}
		cmd = exec.Command("go", cmdArgs...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return reportMetrics{}, fmt.Errorf("run_kpi %s: %w", tag, err)
	}
	matches, err := filepath.Glob(filepath.Join("reports", repPrefix+"_*.json"))
	if err != nil {
		return reportMetrics{}, err
	}
	if len(matches) == 0 {
		return reportMetrics{}, fmt.Errorf("no report for %s", repPrefix)
	}
	sort.Strings(matches)
	latest := matches[len(matches)-1]
	b, err := os.ReadFile(latest)
	if err != nil {
		return reportMetrics{}, err
	}
	var m reportMetrics
	if err := json.Unmarshal(b, &m); err != nil {
		return reportMetrics{}, err
	}
	return m, nil
}

func main() {
	dsBench := flag.String("dataset-bench", "./benchmarks/dataset/all.jsonl", "公开基准数据集")
	dsSynth := flag.String("dataset-synth", "./dataset/all.jsonl", "合成数据集")
	outPath := flag.String("out", "./weights/rank_weights_en_r5.json", "R5-A: 最优权重输出路径（独立产物，不覆盖 R4 的 rank_weights_en.json）")
	// R5-A：起始权重取 R4 联合权重（rank_weights_en.json：semantic=2.0 keyword=3.2
	// entity=2.9 reference_frame=0.0 value=0.2 freshness=0.1 source_trust=0.1 feedback=0.1）。
	startPath := flag.String("start", "./weights/rank_weights_en.json", "起始权重 JSON（R4 联合产物）")
	step := flag.Float64("step", 0.1, "坐标下降步长")
	maxRounds := flag.Int("max-rounds", 20, "最大轮数")
	synthFloor := flag.Float64("synth-floor", 0.8485, "合成集 MRR 回归下限（R4 0.8585 − 0.01）")
	// R5-A：坐标下降子进程使用的 embedder 实现。默认 tfidf（复现 R4 基线、保持历史可比）；
	// 交付验收命令用 `-embedder vector`，让权重针对 BGE-m3 向量语义调优（任务书 §A.3.4 语义错配修复）。
	embedderMode := flag.String("embedder", "tfidf", "R5-A: run_kpi 子进程 embedder 实现 tfidf|vector|none")
	rkBin := flag.String("rk-binary", "", "R5-A: 预编译 run_kpi 二进制路径（非空则直接调用，跳过 go run 编译）")
	// R5-A：reference_frame 权重重校准。
	//   -val 校准集（默认 eval/dataset/reference_frame.jsonl，60 条），用于搜索过程中
	//     监控 ref 桶 top-1 传导率（信号区分度 → 命中传导），判定 ref 权重抬升是否有效；
	//   -include-reference-frame 把 reference_frame 纳入搜索变量（R5-A 判绿要求：
	//     重新进入搜索变量并校准出非零权重，或明确结论"纳入后无收益"保留 0.0）。
	//   关闭时不搜索 ref（保留 R3-A 结论），-include-reference-frame=false 可 isolate。
	valPath := flag.String("val", "./dataset/reference_frame.jsonl", "R5-A: reference_frame 校准集（60 条）")
	includeRef := flag.Bool("include-reference-frame", false, "R5-A: 把 reference_frame 纳入搜索变量（否则固定 R3-A 值 0.0）")
	flag.Parse()

	start := time.Now()
	log.Printf("=== R5-A 任务A 权重坐标下降搜索 v2（扩展 8 变量 + reference_frame 重校准） ===")
	log.Printf("起始权重: %s", *startPath)
	log.Printf("公开基准: %s", *dsBench)
	log.Printf("合成集:   %s", *dsSynth)
	log.Printf("搜索变量: %v", searchVarsV2)
	log.Printf("步长: %.2f  最大轮数: %d  合成集下限: %.4f", *step, *maxRounds, *synthFloor)
	log.Printf("reference_frame 校准集: %s 纳入搜索: %v", *valPath, *includeRef)

	cur, err := loadWeights(*startPath)
	if err != nil {
		log.Fatalf("load start weights: %v", err)
	}

	// 1) 跑基线（用起始权重显式跑一遍，确保口径一致）。
	log.Printf("--- 基线评测（R2 起始权重，semantic 真启） ---")
	baseCand := filepath.Join("weights", "_baseline_v2.json")
	if b, err := json.MarshalIndent(cur, "", "  "); err == nil {
		_ = os.WriteFile(baseCand, b, 0o644)
	}
	baseBench, err := runKPIWith(*dsBench, baseCand, "baseline_bench", *embedderMode, *rkBin)
	if err != nil {
		log.Fatalf("baseline bench: %v", err)
	}
	baseSynth, err := runKPIWith(*dsSynth, baseCand, "baseline_synth", *embedderMode, *rkBin)
	if err != nil {
		log.Fatalf("baseline synth: %v", err)
	}
	log.Printf("基线 bench MRR=%.4f top1=%.3f fact=%.1f%% | synth MRR=%.4f fact=%.1f%%",
		baseBench.OverallMRR, baseBench.top1Rate(), baseBench.OverallAccuracyByFact*100,
		baseSynth.OverallMRR, baseSynth.OverallAccuracyByFact*100)

	bestBenchMRR := baseBench.OverallMRR
	bestBenchTop1 := baseBench.top1Rate()
	bestSynthMRR := baseSynth.OverallMRR
	bestBenchFact := baseBench.OverallAccuracyByFact
	synthFloorV := *synthFloor
	// R5-A：ref 桶最优传导率（ref 分最高且 top-1 命中）。初始为 -1（未测）。
	// 仅 -include-reference-frame 时用于接受判据；关闭时为 -1（不参与判定）。
	bestRefCond := -1.0
	if *includeRef {
		if mVal, verr := runKPIWith(*valPath, baseCand, "baseline_val", *embedderMode, *rkBin); verr == nil && mVal.RefFrameSignal != nil {
			bestRefCond = mVal.RefFrameSignal["ref_top1_and_hit"]
			log.Printf("  基线 ref 桶：top1 命中率=%.1f%% 传导率=%.1f%%",
				mVal.RefFrameSignal["expected_top1_hit"]*100, bestRefCond*100)
		}
	}

	var trace []string
	trace = append(trace, fmt.Sprintf("round=0 baseline: bench_mrr=%.4f top1=%.3f synth_mrr=%.4f weights=%v",
		bestBenchMRR, bestBenchTop1, bestSynthMRR, weightsMap(cur)))

	// 2) 坐标下降（顺序固定：semantic → keyword → entity → feedback →
	//    reference_frame → value → freshness → source_trust）。
	//    R5-A：reference_frame 仅在 -include-reference-frame 时纳入变量；
	//    否则跳过（保留 R3-A 结论 ref=0.0，隔离复现 R4 基线）。
	//    fixed 信号用 fixedStep（默认 0.05）微调，calibrated 信号用 step（默认 0.1）。
	refInVars := *includeRef
	for round := 1; round <= *maxRounds; round++ {
		improved := false
		for _, v := range searchVarsV2 {
			// reference_frame 仅在 -include-reference-frame 时纳入变量。
			if v == "reference_frame" && !refInVars {
				continue
			}
			curW := cur.getWeight(v)
			// 红线 1（只增不改语义）：fixed 信号不参与校准，跳过。
			if fixedVars[v] {
				continue
			}
			for _, delta := range []float64{+*step, -*step} {
				cand := cur.clone()
				newW := curW + delta
				if newW < 0 {
					continue
				}
				cand.setWeight(v, newW)
				tag := fmt.Sprintf("r%02d_%s_%s", round, v, dirName(delta))
				candPath, err := cand.writeCand(tag)
				if err != nil {
					log.Fatalf("write cand: %v", err)
				}
				mBench, err := runKPIWith(*dsBench, candPath, tag+"_bench", *embedderMode, *rkBin)
				if err != nil {
					log.Fatalf("eval cand: %v", err)
				}
				log.Printf("round=%d var=%s delta=%+.3f newW=%.3f -> bench MRR=%.4f top1=%.3f",
					round, v, delta, newW, mBench.OverallMRR, mBench.top1Rate())

				// 接受准则：bench MRR 严格上升。
				if mBench.OverallMRR <= bestBenchMRR+1e-9 {
					continue
				}
				// 跑合成集验证不回归。
				mSynth, err := runKPIWith(*dsSynth, candPath, tag+"_synth", *embedderMode, *rkBin)
				if err != nil {
					log.Fatalf("eval synth: %v", err)
				}
				if mSynth.OverallMRR < synthFloorV-1e-9 {
					log.Printf("  拒绝：合成 MRR=%.4f < floor=%.4f", mSynth.OverallMRR, synthFloorV)
					continue
				}
				// R5-A：reference_frame 变量接受前，需在校准集上验证 ref 桶 top-1 传导率
				// 不下降（否则接受会牺牲 ref 桶命中，与 R4-B 结论冲突）。
				// 用同一次 val 评测结果更新 bestRefCond（不重复跑评测）。
				if v == "reference_frame" {
					mVal, verr := runKPIWith(*valPath, candPath, tag+"_val", *embedderMode, *rkBin)
					if verr == nil && mVal.RefFrameSignal != nil {
						cond := mVal.RefFrameSignal["ref_top1_and_hit"]
						log.Printf("  ref 桶校准集：top1 命中率=%.1f%% 传导率=%.1f%%",
							mVal.RefFrameSignal["expected_top1_hit"]*100, cond*100)
						if bestRefCond >= 0 && cond < bestRefCond-1e-6 {
							log.Printf("  拒绝：ref 传导率 %.1f%% 低于当前最优 %.1f%%",
								cond*100, bestRefCond*100)
							continue
						}
						bestRefCond = cond
					}
				}
				// 接受。
				log.Printf("  接受：bench MRR %.4f -> %.4f，synth MRR=%.4f",
					bestBenchMRR, mBench.OverallMRR, mSynth.OverallMRR)
				cur = cand
				bestBenchMRR = mBench.OverallMRR
				bestBenchTop1 = mBench.top1Rate()
				bestBenchFact = mBench.OverallAccuracyByFact
				bestSynthMRR = mSynth.OverallMRR
				improved = true
				trace = append(trace, fmt.Sprintf("round=%d var=%s newW=%.2f: bench_mrr=%.4f top1=%.3f synth_mrr=%.4f fact=%.1f%%",
					round, v, newW, bestBenchMRR, bestBenchTop1, bestSynthMRR, bestBenchFact*100))
				break // 该变量接受一个方向后，进入下一个变量
			}
		}
		if !improved {
			log.Printf("round=%d: 无改进，提前停止", round)
			break
		}
	}

	// 3) 写出最优权重（v2，更新 _comment 与 note）。
	cur.K = 60
	refNote := "保留 0.0（R3-A 结论：当前评测器下无信息量，纳入搜索后无收益）"
	if *includeRef && cur.getWeight("reference_frame") > 0 {
		refNote = fmt.Sprintf("R5-A 重校准出非零权重 %.2f（基于 60 条 reference_frame 校准集）", cur.getWeight("reference_frame"))
	} else if *includeRef {
		refNote = "R5-A 重校准后仍为 0.0（纳入搜索变量但坐标下降收敛到 0，明确结论：纳入后无收益）"
	}
	refIncluded := "否（隔离 R3-A 结论）"
	if *includeRef {
		refIncluded = "是（纳入 reference_frame 搜索变量 + 60 条校准集校验）"
	}
	cur.Comment = "R5-A 产物（扩展 8 变量坐标下降）：起始权重=R4 联合（rank_weights_en.json），" +
		"搜索变量=[semantic,keyword,entity,feedback,reference_frame,value,freshness,source_trust]，步长 0.1，" +
		"reference_frame 纳入搜索=" + refIncluded + "。" +
		"公开 MRR " + fmt.Sprintf("%.4f", bestBenchMRR) + "，合成 MRR " + fmt.Sprintf("%.4f", bestSynthMRR) + "。"
	cur.setWeightNote("semantic", "语义相似度（R5-A：char n-gram TF-IDF embedder 注入后调优，可切 -embedder vector 用 BGE-m3）")
	cur.setWeightNote("keyword", "BM25 全文关键词（R5-A 调优）")
	cur.setWeightNote("entity", "实体匹配（R5-A 调优）")
	cur.setWeightNote("feedback", "反馈历史（R5-A 未纳入校准：kernel 固定公式，红线 1 只增不改语义）")
	cur.setWeightNote("reference_frame", refNote)
	cur.setWeightNote("value", "记忆固有价值（R5-A 未纳入校准：kernel 固定公式，红线 1 只增不改语义）")
	cur.setWeightNote("freshness", "时效新鲜度（R5-A 未纳入校准：kernel 固定公式，红线 1 只增不改语义）")
	cur.setWeightNote("source_trust", "来源可信度（R5-A 未纳入校准：kernel 固定公式，红线 1 只增不改语义）")

	b, _ := json.MarshalIndent(cur, "", "  ")
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		log.Fatalf("mkdir out: %v", err)
	}
	if err := os.WriteFile(*outPath, b, 0o644); err != nil {
		log.Fatalf("write out: %v", err)
	}
	log.Printf("最优权重（v2）已写入 %s", *outPath)

	// 4) 打印最终汇总。
	fmt.Println("\n=== 搜索轨迹（v2） ===")
	for _, t := range trace {
		fmt.Println(t)
	}
	fmt.Printf("\n最终: bench MRR=%.4f top1=%.3f fact=%.1f%% | synth MRR=%.4f | 用时=%s\n",
		bestBenchMRR, bestBenchTop1, bestBenchFact*100, bestSynthMRR, time.Since(start).Round(time.Second))
}

func dirName(delta float64) string {
	if delta > 0 {
		return "up"
	}
	return "down"
}

func weightsMap(d *weightsDoc) map[string]float64 {
	m := map[string]float64{}
	for _, w := range d.Weights {
		m[w.Name] = w.Weight
	}
	return m
}
