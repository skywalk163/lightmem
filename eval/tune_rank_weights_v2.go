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
// 搜索规则（与 R2 tune_rank_weights.go 一致，保证可比）：
//   - 起始权重 = eval/weights/rank_weights_en.json（R2 A 路产物，semantic=1.5 keyword=2.7
//     entity=2.4 reference_frame=0.0 value=0.2 freshness=0.1 source_trust=0.1 feedback=0.1）。
//   - 每轮按固定顺序轮流试探 ±step（默认 ±0.1）。
//   - 接受准则：公开基准 MRR 严格上升，且合成集 MRR 不低于 R2 基线（0.8412）− 0.01。
//   - 最多 maxRounds 轮（默认 20）；一轮内四个变量均无改进则提前停止。
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
var searchVarsV2 = []string{"semantic", "keyword", "entity", "reference_frame"}

// reportMetrics 镜像 run_kpi.go KPIReport 中本脚本需要的字段。
type reportMetrics struct {
	OverallMRR float64 `json:"overall_mrr"`
	Details    []struct {
		Rank int `json:"rank"`
	} `json:"details"`
	// fact 准确率（按 fact 文本）：用于搜索轨迹记录。
	OverallAccuracyByFact float64 `json:"overall_accuracy_by_fact"`
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
func runKPI(dataset, weightsPath, tag string) (reportMetrics, error) {
	repPrefix := "tune_v2_" + tag
	cmd := exec.Command("go", "run", "./run_kpi.go",
		"-dataset", dataset,
		"-weights", weightsPath,
		"-report", repPrefix,
	)
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
	outPath := flag.String("out", "./weights/rank_weights_en.json", "最优权重输出路径")
	// R2 A 路产物作为起点：semantic=1.5 keyword=2.7 entity=2.4 reference_frame=0.0 等。
	startPath := flag.String("start", "./weights/rank_weights_en.json", "起始权重 JSON（R2 A 路产物）")
	step := flag.Float64("step", 0.1, "坐标下降步长")
	maxRounds := flag.Int("max-rounds", 20, "最大轮数")
	synthFloor := flag.Float64("synth-floor", 0.8312, "合成集 MRR 回归下限（R2 0.8412 − 0.01）")
	flag.Parse()

	start := time.Now()
	log.Printf("=== R3-A 任务A 权重坐标下降搜索 v2（方案 β：semantic 真启） ===")
	log.Printf("起始权重: %s", *startPath)
	log.Printf("公开基准: %s", *dsBench)
	log.Printf("合成集:   %s", *dsSynth)
	log.Printf("搜索变量: %v", searchVarsV2)
	log.Printf("步长: %.2f  最大轮数: %d  合成集下限: %.4f", *step, *maxRounds, *synthFloor)

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
	baseBench, err := runKPI(*dsBench, baseCand, "baseline_bench")
	if err != nil {
		log.Fatalf("baseline bench: %v", err)
	}
	baseSynth, err := runKPI(*dsSynth, baseCand, "baseline_synth")
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

	var trace []string
	trace = append(trace, fmt.Sprintf("round=0 baseline: bench_mrr=%.4f top1=%.3f synth_mrr=%.4f weights=%v",
		bestBenchMRR, bestBenchTop1, bestSynthMRR, weightsMap(cur)))

	// 2) 坐标下降（顺序固定：semantic → keyword → entity → reference_frame）。
	for round := 1; round <= *maxRounds; round++ {
		improved := false
		for _, v := range searchVarsV2 {
			curW := cur.getWeight(v)
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
				mBench, err := runKPI(*dsBench, candPath, tag+"_bench")
				if err != nil {
					log.Fatalf("eval cand: %v", err)
				}
				log.Printf("round=%d var=%s delta=%+.1f newW=%.2f -> bench MRR=%.4f top1=%.3f",
					round, v, delta, newW, mBench.OverallMRR, mBench.top1Rate())

				// 接受准则：bench MRR 严格上升。
				if mBench.OverallMRR <= bestBenchMRR+1e-9 {
					continue
				}
				// 跑合成集验证不回归。
				mSynth, err := runKPI(*dsSynth, candPath, tag+"_synth")
				if err != nil {
					log.Fatalf("eval synth: %v", err)
				}
				if mSynth.OverallMRR < synthFloorV-1e-9 {
					log.Printf("  拒绝：合成 MRR=%.4f < floor=%.4f", mSynth.OverallMRR, synthFloorV)
					continue
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
	cur.Comment = "R3-A 产物（方案 β）：semantic 已真实生效（char n-gram TF-IDF embedder，" +
		"词表来自公开基准+合成集 query/fact，IDF 缓存 weights/embedder_vocab_en.json），" +
		"reference_frame 修复后重新评估（QueryFrame 空→0.5 中性分）。" +
		"搜索变量=[semantic,keyword,entity,reference_frame]，坐标下降步长 0.1。" +
		"公开 MRR " + fmt.Sprintf("%.4f", bestBenchMRR) + "，合成 MRR " + fmt.Sprintf("%.4f", bestSynthMRR) + "。"
	cur.setWeightNote("semantic", "语义相似度（R3-A 真启：char trigram TF-IDF embedder 注入后调优）")
	cur.setWeightNote("keyword", "BM25 全文关键词（R3-A 调优）")
	cur.setWeightNote("entity", "实体匹配（R3-A 调优）")
	cur.setWeightNote("reference_frame", "参考系匹配度（R3-A 修复后重新评估）")
	cur.setWeightNote("value", "记忆固有价值（固定）")
	cur.setWeightNote("freshness", "时效新鲜度（固定）")
	cur.setWeightNote("source_trust", "来源可信度（固定）")
	cur.setWeightNote("feedback", "反馈历史（固定）")

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
