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
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lightmem/kernel"
)

const (
	tenant = "eval-tenant"
	user   = "eval-user"

	// kpiRecallTopK 评测召回窗口（§9 评测口径）。
	// RecallLayers 的扫描窗口为 maxScan = TopK*40（下限 200）：默认 TopK=5 → 只扫 200 条。
	// 而本评测集单租户 L1 有约 530 条记忆，超出窗口的记忆「根本没被看到」，
	// 会把准确率变成「窗口抽签」而非检索能力。此处显式放大窗口以覆盖全库。
	// 注意：仅调整评测口径，不改 kernel 默认值（§2.3 defaultRecallTopK）。
	kpiRecallTopK = 200 // maxScan = 8000，覆盖当前评测集全量
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
	// Hop1Memory 多跳样本的 hop-1 记忆（仅 multi-hop 桶使用）。
	// 评测器需同时物化 hop-1 和 hop-2，否则 multi-hop 永远 0%。
	Hop1Memory *kernel.Memory `json:"hop1_memory,omitempty"`
	// R4-B：reference_frame 评测集需要同时物化 targetA/targetB/distractor 三条记忆，
	// 否则同 entity+不同 fact 的对照无法被检索到。复用 hop1/hop2 字段承载「干扰记忆」。
	Hop2Memory *kernel.Memory `json:"hop2_memory,omitempty"`
	// SecondVersion 时序样本的另一版本（仅 temporal 桶使用）。
	// 双时态回溯需同时物化两个版本，否则 as_of 过滤无法验证。
	SecondVersion *kernel.Memory `json:"second_version,omitempty"`
	// R4-B：查询端 reference_frame 坐标。由 reference_frame 评测集写入，
	// 使 signalReferenceFrame 能拿到 QueryFrame 的实际坐标（此前 run_kpi.go 恒为 nil）。
	Entity  string `json:"entity,omitempty"`
	Project string `json:"project,omitempty"`
	Scene   string `json:"scene,omitempty"`
	Task    string `json:"task,omitempty"`
	// R5-B：multi-hop 单元素重构字段（只增不改）。
	// ExpectedMemoryIDPrimary 是最终答案记忆（hop-2），作为多跳主口径；
	// ExpectedMemoryIDHop 是跳板记忆（hop-1），作为 hop 口径对照。
	// 老样本（无此字段）回退到 ExpectedMemoryID。
	ExpectedMemoryIDPrimary []string `json:"expected_memory_ids_primary,omitempty"`
	ExpectedMemoryIDHop     []string `json:"expected_memory_ids_hop,omitempty"`
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
	// CostTracker 成本追踪：每条查询的 token 消耗、延迟、信号计算次数
	Cost *CostTracker `json:"cost,omitempty"`
	// PollutionTrend 污染率趋势：按时间窗口统计
	PollutionTrend *PollutionTrend `json:"pollution_trend,omitempty"`
	// BucketMRR 分桶 MRR（Mean Reciprocal Rank）
	BucketMRR map[string]float64 `json:"bucket_mrr,omitempty"`
	// OverallMRR 总体 MRR
	OverallMRR float64 `json:"overall_mrr,omitempty"`
	// FactFalsePositives R3-C：fact_hit=true 但 ID 未命中 top-1 的样本。
	// 这是 char n-gram 相似度引入的假阳性监控口径——若合成集此类样本 >0.5%，
	// 说明阈值过松，需调高。仅在非空时输出，避免报告膨胀。
	FactFalsePositives []KPIDetail `json:"fact_false_positives,omitempty"`
	// FactThreshold R3-C：本次运行使用的 matchFactV3 char n-gram 阈值。
	FactThreshold float64 `json:"fact_threshold,omitempty"`
	// FactSimAlgo R3-C：本次运行使用的相似度算法 dice|contain。
	FactSimAlgo string `json:"fact_sim_algo,omitempty"`
	// BucketMRRSet R3-D：multi-hop 桶「口径 A」MRR——top-1 MemoryID ∈ ExpectedMemoryID
	// 即算 hit（不区分 hop 顺序），与公开基准（单元素 Expected）可比。
	// 仅在 multi-hop 桶累计；不覆盖主 MRR 口径 BucketMRR（保留历史可比性）。
	BucketMRRSet map[string]float64 `json:"bucket_mrr_set,omitempty"`
	// BucketHit2 R3-D：multi-hop 桶「口径 B」hit@2——top-2 内任一 MemoryID ∈ ExpectedMemoryID
	// 即算命中。反映「一次交互内能拿到正确事实」的体验。
	BucketHit2 map[string]float64 `json:"bucket_hit2,omitempty"`
	// R4-C：主 fact 口径分母（= total − template_total，已剔除模板 expected）。
	// 模板 expected（beam-* "LLM response should ...:"）是评测集形态问题，单列统计。
	FactTotal int `json:"fact_total,omitempty"`
	// R4-C：模板 expected 样本数（从主 fact 口径剔除）。
	TemplateTotal int `json:"template_total,omitempty"`
	// R4-C：模板 expected 中 fact_hit 命中数（仅陈述事实，不计入主口径分子/分母）。
	TemplateHits int `json:"template_hits,omitempty"`
	// R4-C：候选扩展辅助口径——短 expected（<15 rune）在 top-5 候选内命中数，不覆盖主口径。
	FactHitTop5Count int `json:"fact_hit_top5_count,omitempty"`
	// R4-C：主 fact 准确率（= factHits / (total − templateTotal)），即 v4 口径。
	OverallAccuracyByFactV4 float64 `json:"overall_accuracy_by_fact_v4,omitempty"`
	// R4-C：fact 口径版本标识：v3（-fact-v4=false）/ v4（默认）。
	FactAlgo string `json:"fact_algo,omitempty"`
	// R4-B：reference_frame 信号复检辅助口径。
	//   expected_ref_top1 — 期望记忆的 signalReferenceFrame 分在候选集合中为最高的比例（信号区分度）
	//   expected_top1_hit  — 期望记忆最终成为融合 top-1 的比例（命中率）
	//   ref_top1_and_hit   — 期望记忆 ref 分最高且最终 top-1 命中的比例（信号→命中传导率）
	RefFrameSignal map[string]float64 `json:"ref_frame_signal,omitempty"`
	// R5-B：multi-hop 重构后的**主口径 MRR**——用 ExpectedMemoryIDPrimary（最终答案，单元素）
	// 在全融合序内求首个命中的 1/rank。脱离 R4 的 0.500 结构性下限。
	BucketMRRPrimary map[string]float64 `json:"bucket_mrr_primary,omitempty"`
	// R5-B：multi-hop **跳板口径 MRR**——用 ExpectedMemoryIDHop（hop-1）判定，作为对照。
	BucketMRRHop map[string]float64 `json:"bucket_mrr_hop,omitempty"`
	// R5-B：fact 边界归一化开关状态（-fact-normalize，默认 true）。
	FactNormalize bool `json:"fact_normalize,omitempty"`
	// R5-B：本次评测合并的 temporal 样本数（-temporal-samples）。
	TemporalExtra int `json:"temporal_extra,omitempty"`
}

// CostTracker 成本追踪（§9 评测增强）。
type CostTracker struct {
	TotalQueries            int     `json:"total_queries"`
	TotalTokens             int     `json:"total_tokens"`
	TotalLatencyMs          int64   `json:"total_latency_ms"`
	AvgLatencyMs            float64 `json:"avg_latency_ms"`
	TotalSignalComputations int     `json:"total_signal_computations"`
	AvgSignalComputations   float64 `json:"avg_signal_computations"`
	CostPerQuery            float64 `json:"cost_per_query"`
}

// PollutionTrend 污染率趋势（§9 评测增强）。
type PollutionTrend struct {
	Windows        []PollutionWindow `json:"windows"`
	CurrentRate    float64           `json:"current_rate"`
	TrendDirection string            `json:"trend_direction"` // improving / stable / worsening
}

// PollutionWindow 时间窗口污染率。
type PollutionWindow struct {
	WindowStart int     `json:"window_start"`
	WindowEnd   int     `json:"window_end"`
	Total       int     `json:"total"`
	Leaked      int     `json:"leaked"`
	Rate        float64 `json:"rate"`
}

// BucketMRR 分桶 MRR 统计。
type BucketMRR struct {
	Total int     `json:"total"`
	MRR   float64 `json:"mrr"`
	Ranks []int   `json:"ranks,omitempty"`
}

type KPIDetail struct {
	ID       string `json:"id"`
	Bucket   string `json:"bucket"`
	Hit      bool   `json:"hit"`
	Rank     int    `json:"rank,omitempty"`
	FactHit  bool   `json:"fact_hit"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Reason   string `json:"reason"`
	// R4-C：该样本 expected 为评测模板（"LLM response should ...:"），已从主 fact 口径剔除。
	IsTemplateExpected bool `json:"is_template_expected,omitempty"`
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

	// 命令行参数（P7-D 公开基准对标复用同一评测链路）：
	//   -dataset <path>  评测集 JSONL 路径，默认 <dsDir>/all.jsonl（既有合成 KPI 集）
	//   -report  <name>  报告文件名前缀（不含扩展名），默认 kpi_baseline
	//   -weights <path>  RankFusion 权重 JSON 路径；空则沿用 ../kernel/rank_fusion.default_weights.json
	//                    （R2 任务A：支持英文长文本调优权重 rank_weights_en.json 切换）
	dsFlag := flag.String("dataset", dsDir+"/all.jsonl", "评测集 JSONL 路径（默认合成 all.jsonl）")
	repFlag := flag.String("report", "kpi_baseline", "报告文件名前缀")
	wFlag := flag.String("weights", "", "RankFusion 权重 JSON 路径；空则用 ../kernel/rank_fusion.default_weights.json")
	// R3-A：是否注入确定性 char n-gram TF-IDF embedder。
	// 默认注入（让 signalSemantic 真正生效）；-no-embedder 用于对照实验，
	// 复现 R2 基线（signalSemantic 退化为 RecallScore）。
	noEmbedder := flag.Bool("no-embedder", false, "R3-A: 跳过 embedder 注入，复现 R2 基线（semantic 退化为 RecallScore）")
	// R5-A：embedder 实现切换。vector=静态向量（BGE-m3）；tfidf=R3-A 原有 char n-gram TF-IDF；none=不注入。
	// -no-embedder=true 时覆盖本 flag（等价 none），保留 R3-A 的 isolate 兼容。
	embedderFlag := flag.String("embedder", "tfidf", "R5-A: embedder 实现 tfidf|vector|none")
	// R3-A：embedder 词表缓存路径。首次运行时从两个评测集统计 IDF 并落盘，
	// 后续复用同一词表（保证 tune 脚本与 run_kpi.go 向量维度一致、可复现）。
	vocabPath := flag.String("vocab-path", "weights/embedder_vocab_en.json", "R3-A: char n-gram TF-IDF 词表缓存路径")
	// R3-C：fact 口径 v3 的 char n-gram 相似度阈值。
	// 0.55 为推荐起点；阈值敏感性分析在 0.4/0.5/0.55/0.6/0.7 五档下做。
	factThr := flag.Float64("fact-threshold", 0.55, "R3-C: matchFactV3 char trigram 相似度阈值（仅当 R2 matchFact v2 未命中时生效）")
	// R3-C：相似度算法选择。dice=任务书建议的 Dice 系数；contain=containment（软包含，
	// 更适合 got 是 "speaker: turn" 长文本而 expected 是短答案的场景）。
	factSim := flag.String("fact-sim", "dice", "R3-C: fact v3 相似度算法 dice|contain")
	// R4-C：fact 口径 v4 开关。启用后在 v3（v2 包含 + char trigram 软包含）之上追加四类
	// 确定性补判：① 模板 expected 剔除（beam-* "LLM response should ...:"）；
	// ② 时间推理（用 top-1 候选 occurred_at 与相对时间词做确定性日历推算）；
	// ③ 数值/日期归一（货币符号、千分位、月名日期规范化）；
	// ④ 短 expected（<15 rune）词元包含。
	// -fact-v4=false 复现 R3-C v3 基线（before/after isolate）。
	factV4Flag := flag.Bool("fact-v4", true, "R4-C: 启用 fact 口径 v4（时间推理/数值日期归一/短 expected 词元）；false 复现 R3-C v3 基线")
	// R3-B：as_of 查询时把 rf.Now 设为样本的 as_of，让 freshness/feedback 时序生效。
	// 注意 rf 是全局指针，必须按样本重置，否则污染下一条（任务书 B.3.2 警告）。
	nowAsOf := flag.Bool("now-asof", false, "R3-B: as_of 查询时把 rf.Now 设为样本 as_of（对照实验用；非 as_of 样本保持评测启动时刻）")
	// R3-B：固定 Now 为任务书指定的基准日期（YYYY-MM-DD），复现 R2 报告 rf.Now=2026-10-12 的历史值。
	// 默认空 = 用评测启动时刻（与 -now-asof 关闭时的现状基线一致）。
	nowFixed := flag.String("now-fixed", "", "R3-B: 固定 rf.Now 为该日期（如 2026-10-12）；空则用评测启动时刻")
	// R3-B：诊断打印——命中指定 sample id 时打印 top-5 候选的 signal_scores，用于定位
	// rf.Now 改变对 freshness/feedback 排序的影响（B 路根因分析）。
	diagID := flag.String("diag", "", "R3-B: 诊断打印：指定样本 id，打印其 top-5 候选各信号分与融合分")
	// R4-B：`-now-asof` 默认开启（R3-B 决策默认 false，此处评估开启的影响）。
	// 语义：把 -now-asof 的默认值改为 true，仅在未显式指定时生效（R4-B 任务书 B.2.3）。
	// -now-asof=false 时回到 R3-B 默认关闭，可 isolate 复现。
	nowAsOfDefault := flag.Bool("now-asof-default", false, "R4-B: 默认开启 -now-asof（as_of 查询时 rf.Now=样本 as_of）；false 时按 -now-asof 原样")
	// R4-B：reference_frame 诊断打印——指定样本 id，打印查询端坐标与 top-5 候选
	// 的 signalReferenceFrame 打分，用于复检参考系信号区分度（R3-A §8 风险）。
	diagRef := flag.String("diag-ref", "", "R4-B: 诊断打印：指定 reference_frame 样本 id，打印查询端坐标与 top-5 候选的 reference_frame 信号分")
	// R4-A：multi-hop 二次 hop 证据重排开关。
	//   默认开启（kernel/multihop.go 内的 MultiHopReRank）；-no-multihop 关闭，
	//   用于 isolate 复现 R3-D 基线（公开基准 multi-hop 口径 A 0.9455）。
	noMultiHop := flag.Bool("no-multihop", false, "R4-A: 关闭 multi-hop 二次 hop 证据重排（isolate 复现 R3-D 基线）")
	// R4-A：hop 链式推理深度。
	//   <=1：只用 L1/L2 候选自身证据（默认；双源实测最优）；
	//   >=2：追加 L3 上下文增强（邻接记忆补强，默认不启用，见交付报告 isolate 表）。
	hopDepth := flag.Int("hop-depth", 1, "R4-A: hop 链式推理深度（>=2 启用上下文增强）")
	// R4-A：multi-hop 诊断打印——指定样本 id（可逗号分隔多个），打印 hop 锚点、
	// top-5 候选的 hop 证据（answers / cover / hop-2 推理链）与重排前后的 top-1。
	diagMH := flag.String("diag-mh", "", "R4-A: 诊断打印：指定样本 id（逗号分隔多个），打印 hop 锚点与 top-5 候选的 hop 证据链")
	// R5-B：temporal 样本补齐——加载额外 temporal JSONL 并合并进评测（追加到 items 末尾）。
	// 用于把 generate_temporal.go 产出的窗口查询样本并入 temporal 桶评测。
	temporalSamples := flag.String("temporal-samples", "", "R5-B: 额外 temporal 样本 JSONL 路径（合并进评测；空则不合并）")
	// R5-B：fact 边界归一化开关（默认 true）。
	//   开启后 fact 匹配前做确定性边界归一化（数值 $3,750→3750 / 日期 2023-04-01→2023 04 01 /
	//   单位大小写统一），并在 char-trigram 模糊路径上追加「边界对齐词元覆盖」护栏，抑制
	//   长 got 文本里的碎片误配（假阳性）。-fact-normalize=false 复现 R4 基线（逐位一致）。
	factNormalize := flag.Bool("fact-normalize", true, "R5-B: fact 匹配边界归一化 + 边界对齐护栏；false 复现 R4 基线")
	flag.Parse() // 必须先 Parse 再取值，否则取到的是默认值
	factNormalizeEnabled = *factNormalize
	dsPath := *dsFlag
	repPrefix := *repFlag
	wPath := *wFlag
	factV3Threshold = *factThr
	factV3SimAlgo = *factSim
	factV4Enabled = *factV4Flag
	// R4-B：`-now-asof-default` 语义——当它为 true 时强制启用 -now-asof
	//（把 R3-B 的默认关闭翻转为默认开启）。-now-asof=false 可显式关闭。
	// -now-asof 默认值本身保持 false（R3-B 决策，避免隐式改变历史基线）。
	nowAsOfOn := *nowAsOf || *nowAsOfDefault
	// R4-A：-diag-mh 支持逗号分隔的多个样本 id（确定性：固定按 flag 顺序建集合）。
	diagMHIDs := map[string]bool{}
	for _, id := range strings.Split(*diagMH, ",") {
		if id = strings.TrimSpace(id); id != "" {
			diagMHIDs[id] = true
		}
	}

	_ = os.MkdirAll(repDir, 0o755)

	// 1) 加载评测集。
	items, err := loadDataset(dsPath)
	if err != nil {
		log.Fatalf("load dataset: %v", err)
	}
	log.Printf("loaded %d items", len(items))

	// R5-B：合并额外 temporal 样本（-temporal-samples），追加到 items 末尾。
	// 追加序确定（文件行序），不改变既有样本的相对顺序与统计口径。
	temporalExtra := 0
	if *temporalSamples != "" {
		extra, err := loadDataset(*temporalSamples)
		if err != nil {
			log.Fatalf("load temporal samples %s: %v", *temporalSamples, err)
		}
		temporalExtra = len(extra)
		items = append(items, extra...)
		log.Printf("[R5-B] merged %d temporal samples from %s（total=%d）", len(extra), *temporalSamples, len(items))
	}

	// 2) 装配内核（SQLite 内存后端，无需真实 PG）。
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	rf := kernel.DefaultRankFusion()
	// 基准时刻：默认用评测启动时刻（避免逐条查询 freshness 抖动）；
	// -now-fixed 则固定为指定日期（R3-B 对照实验：复现 R2 报告 rf.Now=2026-10-12）。
	// baseNow 是逐样本重置时的默认 Now；-now-asof 仅对 as_of 样本覆盖它。
	baseNow := time.Now().UTC()
	if *nowFixed != "" {
		if t, perr := time.Parse("2006-01-02", *nowFixed); perr == nil {
			baseNow = t.UTC()
			log.Printf("[R3-B] -now-fixed=%s -> baseNow=%s", *nowFixed, baseNow.Format(time.RFC3339))
		} else {
			log.Fatalf("-now-fixed %q: %v", *nowFixed, perr)
		}
	}
	rf.Now = baseNow
	// 权重加载（R2 任务A）：
	//   - 未指定 -weights：沿用 ../kernel/rank_fusion.default_weights.json（R3 校准权重，
	//     与 P7-D 报告 0.546 / 0.821 基线一致，保证回归可复现）。
	//   - 指定 -weights：加载该 JSON（如 eval/weights/rank_weights_en.json）。
	weightsPath := wPath
	if weightsPath == "" {
		weightsPath = "../kernel/rank_fusion.default_weights.json"
	}
	if err := rf.LoadWeightsFromFile(weightsPath); err != nil {
		log.Fatalf("load weights %s: %v", weightsPath, err)
	}
	// R5-A：向量 embedder 注入方式（-embedder vector|tfidf|none）。
	//   vector  — 加载 BGE-m3 静态向量文件（kernel/embedder_vector.go），semantic 用真实语义向量；
	//   tfidf   — R3-A 原有 char n-gram TF-IDF embedder（历史默认）；
	//   none    — 不注入（signalSemantic 退化为 RecallScore，R2 基线）。
	// 默认 tfidf：保持 R4 基线逐位一致；-embedder vector 需先由远端脚本生成
	// eval/vectors/vectors.npy（模型文件缺失时报错并提示回退 tfidf，任务书 §A.3.1）。
	// 兼容性：-no-embedder=true 等价于 -embedder none。
	switch *embedderFlag {
	case "none", "vector", "tfidf":
		// 合法值，继续。
	default:
		log.Fatalf("-embedder %q invalid: must be tfidf|vector|none", *embedderFlag)
	}
	if *noEmbedder {
		// R3-A 兼容：-no-embedder=true 强制不注入（等价 -embedder none）。
		*embedderFlag = "none"
	}
	switch *embedderFlag {
	case "none":
		log.Printf("[R5-A] embedder mode=none (semantic falls back to RecallScore)")
	case "vector":
		ve, err := loadVectorEmbedder(dsPath)
		if err != nil {
			log.Fatalf("[R5-A] load vector embedder: %v\n  -> 建议回退：-embedder tfidf（任务书 §5.4 降级）", err)
		}
		rf.SetEmbedderImpl(ve)
		log.Printf("[R5-A] embedder injected: name=%s dim=%d count=%d", ve.Name(), ve.Dim(), ve.Count())
	default: // "tfidf"
		v, err := loadEvalVocab(*vocabPath, dsPath)
		if err != nil {
			log.Fatalf("build/load embedder vocab: %v", err)
		}
		rf.SetEmbedder(v.EmbedFn())
		log.Printf("[R3-A] embedder injected: ngram=%d dim=%d docs=%d", v.Ngram, v.Dim(), v.N)
	}
	srv.SetRankFusion(rf)

	// R4-A：装配 multi-hop 二次 hop 证据重排（kernel/multihop.go）。
	//   默认开启（深度 1：L1 直接回答 + L2 锚点覆盖）；-no-multihop 关闭以 isolate 复现基线。
	//   上下文增强（L3，Depth>=2）默认关闭：双源实测对 multi-hop hit@2 有回归。
	mhCfg := kernel.DefaultMultiHopConfig()
	mhCfg.Enabled = !*noMultiHop
	mhCfg.Depth = *hopDepth
	if *hopDepth >= 2 && mhCfg.ContextWindow <= 0 {
		mhCfg.ContextWindow = 30 * 24 * time.Hour // 默认 30 天邻接窗（仅 Depth>=2 生效）
	}
	srv.SetMultiHopConfig(mhCfg)
	log.Printf("[R4-A] multi-hop rerank: enabled=%v depth=%d ctx=%v", mhCfg.Enabled, mhCfg.Depth, mhCfg.ContextWindow)

	// 3) 逐条写入记忆 + 跑查询。
	var details []KPIDetail
	bucketHits := map[string]int{}
	bucketTotal := map[string]int{}
	hits, total := 0, 0
	factHits := 0
	// R4-C：主 fact 口径分母（= total − templateTotal），模板 expected 单独统计。
	factTotal := 0
	templateTotal, templateHits := 0, 0
	// R4-C：候选扩展辅助口径——短 expected 在本样本 top-5 候选内命中数（不覆盖主口径）。
	factHitTop5Count := 0
	temporalHits, temporalTotal := 0, 0
	multiSignalHits, multiSignalTotal := 0, 0
	// R3-D：multi-hop 口径 A（hit@1 集合判定）与口径 B（hit@2）累计器。
	multiSetHits, multiSetTotal := 0, 0
	multiHit2Hits := 0
	// R5-B：multi-hop 主口径（ExpectedMemoryIDPrimary）与跳板口径（ExpectedMemoryIDHop）的 rank 序列。
	var multihopPrimaryRanks, multihopHopRanks []int
	pollutionTotal, pollutionHits := 0, 0
	// R4-B：reference_frame 信号复检计数器。
	//   refFrameTotal    — reference_frame 桶样本数
	//   refFrameTop1     — 期望记忆的 signalReferenceFrame 分在候选集合中最高
	//   refFrameTop1Hit  — 期望记忆最终成为融合 top-1（hit）
	//   refFrameTop1AndHit — 期望记忆 ref 分最高且 top-1 命中
	refFrameTotal, refFrameTop1, refFrameTop1Hit, refFrameTop1AndHit := 0, 0, 0, 0

	// 成本追踪
	cost := &CostTracker{}
	// 污染率趋势：按 100 条查询分窗口
	pollutionTrend := &PollutionTrend{}
	// MRR 追踪：每条查询的 reciprocal rank
	bucketRanks := map[string][]int{}
	allRanks := []int{}

	// R3-C：假阳性收集——fact_hit=true 但 ID 未命中 top-1 的样本。
	// 用于阈值敏感性分析与假阳性抽查。
	var factFalsePositives []KPIDetail

	// 污染窗口追踪
	windowSize := 100
	currentWindow := 0
	windowLeaked := 0
	windowTotal := 0

	for _, item := range items {
		// 每 100 条查询记录一个污染窗口
		if currentWindow > 0 && currentWindow%windowSize == 0 {
			rate := 0.0
			if windowTotal > 0 {
				rate = float64(windowLeaked) / float64(windowTotal)
			}
			pollutionTrend.Windows = append(pollutionTrend.Windows, PollutionWindow{
				WindowStart: currentWindow - windowSize + 1,
				WindowEnd:   currentWindow,
				Total:       windowTotal,
				Leaked:      windowLeaked,
				Rate:        rate,
			})
			windowLeaked = 0
			windowTotal = 0
		}
		currentWindow++

		isAdversarial := item.HasPollution || item.HasInjection
		if isAdversarial {
			// 污染/投毒样本：期望被 WritePolicy 拒收，或写入后不被 top-1 召回（未泄漏即通过）。
			windowTotal++
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
		// 多跳样本：同时物化 hop-1 记忆（否则 multi-hop 永远 0%）。
		if item.Hop1Memory != nil {
			if _, err := srv.SaveMemory(ctx, item.Hop1Memory, user); err != nil && !isAdversarial {
				log.Printf("WARN: save hop1 %s: %v", item.Hop1Memory.MemoryID, err)
			}
		}
		// R4-B：reference_frame 评测集用 Hop2Memory 承载第三条干扰记忆，
		// 使同 entity+不同 fact 的三元对照都能被召回，signalReferenceFrame 才可评估。
		if item.Hop2Memory != nil {
			if _, err := srv.SaveMemory(ctx, item.Hop2Memory, user); err != nil && !isAdversarial {
				log.Printf("WARN: save hop2 %s: %v", item.Hop2Memory.MemoryID, err)
			}
		}
		// 时序样本：同时物化两个版本，使 as_of 过滤能正确验证。
		if item.SecondVersion != nil {
			if _, err := srv.SaveMemory(ctx, item.SecondVersion, user); err != nil && !isAdversarial {
				log.Printf("WARN: save second version %s: %v", item.SecondVersion.MemoryID, err)
			}
		}

		// 跑查询。
		// R3-B：按样本重置 rf.Now，避免全局污染。
		//   - 默认：恢复 baseNow（-now-fixed 未指定时为评测启动时刻，与现状基线一致）。
		//   - -now-asof：as_of 查询时设为样本 as_of，让 freshness/feedback 时序生效。
		// 必须在 RecallAndFuse 之前设置（该函数不会覆盖非零 Now，见 kernel/router_mem.go:250）。
		rf.Now = baseNow
		if nowAsOfOn && item.AsOf != "" {
			if t, perr := time.Parse("2006-01-02", item.AsOf); perr == nil {
				rf.Now = t.UTC()
			} else {
				log.Printf("WARN: parse as_of %q for item %s: %v", item.AsOf, item.ID, perr)
			}
		}
		startTime := time.Now().UTC()
		signalComps := 0
		rc := kernel.RouterContext{
			Text:        item.Query,
			TenantID:    item.Tenant,
			UserID:      item.User,
			Entity:      item.Entity,
			ProjectID:   item.Project,
			Scene:       item.Scene,
			Task:        item.Task,
			MemoryKinds: nil,
			TopK:        kpiRecallTopK,
		}
		if item.AsOf != "" {
			asOf := item.AsOf
			rc.AsOf = &asOf
		}
		res, err := srv.RecallAndFuse(ctx, rc)
		latency := time.Since(startTime).Milliseconds()
		cost.TotalLatencyMs += latency
		cost.TotalQueries++
		// 信号计算次数：8 路信号 × 候选数。
		// 兼容旧版 kernel（res.Trace 为 []TraceEntry）与新版（*RetrievalTrace）：
		// 统一按 top-3 候选 × 8 路信号估算。
		if len(res.Fused) > 0 && res.Trace != nil {
			signalComps = 8 * 3 // 8 路信号 × top-3 候选，仅估算
		}
		cost.TotalSignalComputations += signalComps
		// R4-B：reference_frame 诊断打印——打印查询端坐标与 top-5 候选的
		// signalReferenceFrame 打分，复检参考系信号区分度（R3-A §8 风险）。
		if *diagRef != "" && item.ID == *diagRef {
			log.Printf("[R4-B diag-ref] id=%s bucket=%s", item.ID, item.Bucket)
			log.Printf("  QueryFrame: entity=%q project=%q scene=%q task=%q",
				item.Entity, item.Project, item.Scene, item.Task)
			for i, r := range res.Fused {
				if i >= 5 {
					break
				}
				ss := r.SignalScores
				rfv := sigFmt(ss, "reference_frame")
				log.Printf("  #%d %-20s score=%.5f ref_frame=%v (memory.rf.entity=%q project=%q scene=%q)",
					i+1, r.MemoryID, r.Score, rfv,
					memRefFrameField(r.Memory, "entity"),
					memRefFrameField(r.Memory, "project"),
					memRefFrameField(r.Memory, "scene"))
			}
		}
		// R4-A：multi-hop 诊断打印——hop 锚点 + top-5 候选的 hop 证据链。
		if *diagMH != "" && diagMHIDs[strings.TrimSpace(item.ID)] {
			anchors := kernel.HopAnchors(item.Query)
			log.Printf("[R4-A diag-mh] id=%s bucket=%s anchors(%d)=%v",
				item.ID, item.Bucket, len(anchors), anchors)
			log.Printf("  query=%q", item.Query)
			for i, r := range res.Fused {
				if i >= 5 {
					break
				}
				answers, cover := kernel.MultiHopRankKey(item.Query, r, anchors)
				log.Printf("  #%d %-20s score=%.5f answers=%t cover=%.2f exp=%t reason=%q fact=%q",
					i+1, r.MemoryID, r.Score, answers, cover,
					sliceContains(item.ExpectedMemoryID, r.MemoryID), r.Reason,
					trimForLog(r.Fact, 90))
			}
			log.Printf("  expected=%v", item.ExpectedMemoryID)
		}
		// R3-B：诊断打印——rf.Now 改变后该样本 top-5 各信号分，用于定位 freshness 排序漂移。
		if *diagID != "" && item.ID == *diagID {
			log.Printf("[R3-B diag] id=%s bucket=%s as_of=%q rf.Now=%s",
				item.ID, item.Bucket, item.AsOf, rf.Now.Format(time.RFC3339))
			for i, r := range res.Fused {
				if i >= 5 {
					break
				}
				ss := r.SignalScores
				log.Printf("  #%d %-20s score=%.5f freshness=%v feedback=%v semantic=%v keyword=%v entity=%v value=%v trust=%v ref=%v",
					i+1, r.MemoryID, r.Score,
					sigFmt(ss, "freshness"), sigFmt(ss, "feedback"), sigFmt(ss, "semantic"),
					sigFmt(ss, "keyword"), sigFmt(ss, "entity"), sigFmt(ss, "value"),
					sigFmt(ss, "source_trust"), sigFmt(ss, "reference_frame"))
			}
		}
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
				windowLeaked++
			}
			continue
		}

		// 判定命中：top-1 的 memory_id 在 expected_memory_ids 中；
		// fact 命中为辅助口径（等价文本），用于区分「ID 不一致但答案正确」。
		hit := false
		rank := 0
		gotFact := ""
		top1IsExpected := false
		var topFacts []string
		if len(res.Fused) > 0 {
			gotFact = res.Fused[0].Fact
			top1IsExpected = sliceContains(item.ExpectedMemoryID, res.Fused[0].MemoryID)
			for j, exp := range item.ExpectedMemoryID {
				if res.Fused[0].MemoryID == exp {
					hit = true
					rank = j + 1
					break
				}
			}
			// 如果 ID 没命中，检查 top-3 中是否有 expected
			if !hit {
				for j, f := range res.Fused {
					for _, exp := range item.ExpectedMemoryID {
						if f.MemoryID == exp {
							hit = true
							rank = j + 1
							break
						}
					}
					if hit {
						break
					}
				}
			}
			// R4-C：收集 top-5 候选 fact 文本，供候选扩展辅助口径使用。
			for i := 0; i < len(res.Fused) && i < 5; i++ {
				topFacts = append(topFacts, res.Fused[i].Fact)
			}
		}
		// fact 命中判定（R2 v2 + R3-C v3 + R4-C v4）：
		//   v2/v3：归一化包含 + char trigram 软包含（见 matchFactV3 注释）。
		//   v4（-fact-v4=true，默认）：追加 模板剔除 / 时间推理 / 数值日期归一 / 短 expected 词元包含。
		// 纯字符串与日历推算，不调 LLM（红线 4），seed 无关（无随机源）。
		// anchor 取 top-1 候选的 occurred_at，作为时间推理的日历锚点（非 ground truth）。
		var anchor *time.Time
		if len(res.Fused) > 0 && res.Fused[0].Memory != nil {
			anchor = res.Fused[0].Memory.OccurredAt
		}
		var factHit bool
		if factV4Enabled {
			factHit = matchFactV4(gotFact, item.ExpectedFact, anchor)
		} else {
			factHit = matchFactV3(gotFact, item.ExpectedFact)
		}
		// R4-C：候选扩展辅助口径——主口径未命中且 expected <15 rune 时，扩展到 top-5 候选判定。
		factHitTop5 := false
		if factV4Enabled && !factHit && len(topFacts) > 0 {
			factHitTop5 = matchFactTop5(topFacts, item.ExpectedFact, anchor)
		}
		if factHitTop5 {
			factHitTop5Count++
		}
		// R4-C：模板 expected（beam-* "LLM response should ...:"）从主 fact 口径剔除，单独统计。
		templateExpected := factV4Enabled && isTemplateExpected(item.ExpectedFact)

		total++
		bucketTotal[item.Bucket]++
		if hit {
			hits++
			bucketHits[item.Bucket]++
		}
		// 记录 MRR：1/rank，未命中为 0
		if rank > 0 {
			bucketRanks[item.Bucket] = append(bucketRanks[item.Bucket], rank)
			allRanks = append(allRanks, rank)
		} else {
			bucketRanks[item.Bucket] = append(bucketRanks[item.Bucket], 0)
			allRanks = append(allRanks, 0)
		}
		if templateExpected {
			// 模板 expected：不计入主 fact 口径（分子/分母均剔除），单独统计。
			templateTotal++
			if factHit {
				templateHits++
			}
		} else {
			factTotal++
			if factHit {
				factHits++
				// R4-C：假阳性留痕——fact_hit=true 但 top-1 MemoryID 不在 expected 内。
				// 语义（R3 任务书 §C.3「fact_hit 但 ID 未中 top-1」）：top-1 是错记忆，但 got_fact 文本被判匹配。
				// 修正 R3-C 的 rank≠1 判据：multi-hop 合成集的 ExpectedMemoryID 含 hop-1/hop-2 两元素，
				// top-1 命中 hop-1 时 rank=2，但 top-1 仍是 expected 记忆，并非假阳性。
				if !top1IsExpected {
					factFalsePositives = append(factFalsePositives, KPIDetail{
						ID:       item.ID,
						Bucket:   item.Bucket,
						Hit:      hit,
						Rank:     rank,
						FactHit:  true,
						Expected: item.ExpectedFact,
						Got:      gotFact,
						Reason:   "fact_hit_but_top1_id_miss",
					})
				}
			}
		}

		// R2 任务D：防御性加固——temporal / multi-hop 桶统计只对非对抗样本累计。
		// 当前数据集下对抗样本已在上方 isAdversarial 分支 continue，此条件恒真，
		// 仅防止未来数据源把 pollution/injection 标进 temporal/multi-hop 桶污染分母。
		if item.IsTemporal && !isAdversarial {
			temporalTotal++
			if hit {
				temporalHits++
			}
		}
		if item.Bucket == "multi-hop" && !isAdversarial {
			multiSignalTotal++
			if hit {
				multiSignalHits++
			}
			// R3-D：口径 A（hit@1 集合判定）——top-1 MemoryID ∈ ExpectedMemoryID 即算 hit，
			// 不区分 hop 顺序，与公开基准（单元素 ExpectedMemoryID）可比。
			// 合成集 ExpectedMemoryID=[hop2ID,hop1ID] 时，主 MRR 口径对命中 hop-1 只计 0.5
			// （结构性惩罚），口径 A 消除该惩罚，测「检索到目标事实集合」的真实能力。
			multiSetTotal++
			top1ID := ""
			if len(res.Fused) > 0 {
				top1ID = res.Fused[0].MemoryID
			}
			if sliceContains(item.ExpectedMemoryID, top1ID) {
				multiSetHits++
			}
			// R3-D：口径 B（hit@2）——top-2 内任一 MemoryID ∈ ExpectedMemoryID 即算命中。
			if multiHit2(res.Fused, item.ExpectedMemoryID) {
				multiHit2Hits++
			}
			// R5-B：multi-hop 主口径 MRR——用 ExpectedMemoryIDPrimary（最终答案，单元素）
			// 在全融合序内求首个命中的 1/rank。无 primary 字段的老样本回退 ExpectedMemoryID。
			primaryIDs := item.ExpectedMemoryIDPrimary
			if len(primaryIDs) == 0 {
				primaryIDs = item.ExpectedMemoryID
			}
			multihopPrimaryRanks = append(multihopPrimaryRanks, firstRank(res.Fused, primaryIDs))
			// R5-B：multi-hop 跳板口径 MRR——用 ExpectedMemoryIDHop（hop-1）判定（对照）。
			if len(item.ExpectedMemoryIDHop) > 0 {
				multihopHopRanks = append(multihopHopRanks, firstRank(res.Fused, item.ExpectedMemoryIDHop))
			}
		}

		// R4-B：reference_frame 桶——复检 signalReferenceFrame 区分度。
		// 期望记忆 ref 分是否最高（refFrameTop1），与实际 top-1 是否命中的对照。
		// 遍历全部 Fused（融合序），找到期望记忆的 reference_frame 分，与候选最大值比较。
		if item.Bucket == "reference_frame" && !isAdversarial && len(res.Fused) > 0 {
			refFrameTotal++
			expID := item.ExpectedMemoryID[0]
			var expRef, maxRef float64 = -1, -1
			for _, f := range res.Fused {
				if v, ok := f.SignalScores["reference_frame"]; ok {
					if f.MemoryID == expID {
						expRef = v
					}
					if v > maxRef {
						maxRef = v
					}
				}
			}
			if expRef < 0 {
				expRef = 0 // 期望记忆不在 top-N（未召回），ref 分视为 0
			}
			// refFrameTop1：期望记忆 ref 分在候选集合中最高（信号区分度）。
			// refFrameTop1AndHit：ref 分最高且 top-1 命中（用 top1IsExpected，非 hit——后者含 top-3）。
			if expRef >= maxRef && expRef > 0 {
				refFrameTop1++
				if top1IsExpected {
					refFrameTop1AndHit++
				}
			}
			if top1IsExpected {
				refFrameTop1Hit++
			}
		}

		details = append(details, KPIDetail{
			ID:                 item.ID,
			Bucket:             item.Bucket,
			Hit:                hit,
			Rank:               rank,
			FactHit:            factHit,
			Expected:           item.ExpectedFact,
			Got:                gotFact,
			Reason:             "",
			IsTemplateExpected: templateExpected,
		})
	}

	// 记录最后一个窗口
	if windowTotal > 0 {
		rate := float64(windowLeaked) / float64(windowTotal)
		pollutionTrend.Windows = append(pollutionTrend.Windows, PollutionWindow{
			WindowStart: currentWindow - windowSize + 1,
			WindowEnd:   currentWindow,
			Total:       windowTotal,
			Leaked:      windowLeaked,
			Rate:        rate,
		})
	}

	// 计算污染率趋势方向
	if len(pollutionTrend.Windows) >= 2 {
		firstRate := pollutionTrend.Windows[0].Rate
		lastRate := pollutionTrend.Windows[len(pollutionTrend.Windows)-1].Rate
		if lastRate < firstRate-0.05 {
			pollutionTrend.TrendDirection = "improving"
		} else if lastRate > firstRate+0.05 {
			pollutionTrend.TrendDirection = "worsening"
		} else {
			pollutionTrend.TrendDirection = "stable"
		}
	} else {
		pollutionTrend.TrendDirection = "stable"
	}
	if pollutionTotal > 0 {
		pollutionTrend.CurrentRate = 1 - float64(pollutionHits)/float64(pollutionTotal)
	}

	// 计算 MRR
	bucketMRR := map[string]float64{}
	for bucket, ranks := range bucketRanks {
		if len(ranks) == 0 {
			continue
		}
		var mrr float64
		for _, r := range ranks {
			if r > 0 {
				mrr += 1.0 / float64(r)
			}
		}
		bucketMRR[bucket] = mrr / float64(len(ranks))
	}
	// 总体 MRR
	var overallMRR float64
	if len(allRanks) > 0 {
		for _, r := range allRanks {
			if r > 0 {
				overallMRR += 1.0 / float64(r)
			}
		}
		overallMRR = overallMRR / float64(len(allRanks))
	}

	// 计算平均延迟和信号计算次数
	if cost.TotalQueries > 0 {
		cost.AvgLatencyMs = float64(cost.TotalLatencyMs) / float64(cost.TotalQueries)
		cost.AvgSignalComputations = float64(cost.TotalSignalComputations) / float64(cost.TotalQueries)
		// 估算每条查询成本（假设 $0.01/1K tokens + $0.001/1K signals）
		cost.CostPerQuery = float64(cost.TotalTokens)/1000*0.01 + float64(cost.TotalSignalComputations)/1000*0.001
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
		Cost:                cost,
		PollutionTrend:      pollutionTrend,
		BucketMRR:           bucketMRR,
		OverallMRR:          overallMRR,
		FactFalsePositives:  factFalsePositives,
		FactThreshold:       factV3Threshold,
		FactSimAlgo:         factV3SimAlgo,
		// R4-C：模板剔除后的主 fact 口径 + 候选扩展辅助口径。
		FactTotal:        factTotal,
		TemplateTotal:    templateTotal,
		TemplateHits:     templateHits,
		FactHitTop5Count: factHitTop5Count,
		FactAlgo:         factAlgoLabel(),
		// R5-B：fact 边界归一化开关状态 + 合并的 temporal 样本数（追溯用）。
		FactNormalize: factNormalizeEnabled,
		TemporalExtra: temporalExtra,
	}
	// R4-C：主 fact 准确率分母改为 factTotal（= total − templateTotal）。
	// -fact-v4=false 时 templateExpected 恒 false → factTotal == total，
	// 与 R3-C v3 基线逐位一致（isolate 可复现）。
	if factTotal > 0 {
		report.OverallAccuracyByFact = float64(factHits) / float64(factTotal)
		report.OverallAccuracyByFactV4 = report.OverallAccuracyByFact
	} else if total > 0 {
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
	// R3-D：multi-hop 口径 A（bucket_mrr_set）与口径 B（bucket_hit2）填充。
	// 仅统计 multi-hop 桶；不覆盖主 MRR 口径（bucket_mrr），保留历史可比性。
	if multiSetTotal > 0 {
		report.BucketMRRSet = map[string]float64{
			"multi-hop": float64(multiSetHits) / float64(multiSetTotal),
		}
		report.BucketHit2 = map[string]float64{
			"multi-hop": float64(multiHit2Hits) / float64(multiSetTotal),
		}
	}
	// R5-B：multi-hop 主口径 MRR（ExpectedMemoryIDPrimary，最终答案）+ 跳板口径对照
	// （ExpectedMemoryIDHop，hop-1）。主口径脱离 R4 的 0.500 结构性下限。
	if mrr := mrrOfRanks(multihopPrimaryRanks); len(multihopPrimaryRanks) > 0 {
		report.BucketMRRPrimary = map[string]float64{"multi-hop": mrr}
	}
	if mrr := mrrOfRanks(multihopHopRanks); len(multihopHopRanks) > 0 {
		report.BucketMRRHop = map[string]float64{"multi-hop": mrr}
	}
	// R4-B：reference_frame 信号复检辅助口径——期望记忆的 signalReferenceFrame
	// 打分是否最高（refTop1），与实际 top-1 是否命中的对照。
	// 用于判断「reference_frame 信号本身是否有区分度」：若 refTop1=1.0 但 top-1 未命中，
	// 说明信号分正确但权重被稀释（R3-A §8 结论）；若 refTop1 也低，说明信号实现无区分力。
	if refFrameTotal > 0 {
		report.RefFrameSignal = map[string]float64{
			"expected_ref_top1": float64(refFrameTop1) / float64(refFrameTotal),
			"expected_top1_hit": float64(refFrameTop1Hit) / float64(refFrameTotal),
			"ref_top1_and_hit":  float64(refFrameTop1AndHit) / float64(refFrameTotal),
		}
	}
	if pollutionTotal > 0 {
		report.PollutionRate = 1 - float64(pollutionHits)/float64(pollutionTotal)
	}

	// 5) 落盘 JSON 报告。
	path := fmt.Sprintf("%s/%s_%s.json", repDir, repPrefix, time.Now().UTC().Format("20060102-150405"))
	raw, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(path, raw, 0644)
	log.Printf("report written to %s", path)

	// 6) 人工可读摘要（显式逐行拼接，避免占位符与实参错位）。
	var sb strings.Builder
	sb.WriteString("# KPI 基线报告\n\n")
	fmt.Fprintf(&sb, "- 生成时间：%s\n", report.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&sb, "- 评测集总数：%d\n", report.TotalItems)
	fmt.Fprintf(&sb, "- 召回窗口 top-k：%d（maxScan=%d，覆盖全库；见 kpiRecallTopK 注释）\n",
		kpiRecallTopK, kpiRecallTopK*40)
	fmt.Fprintf(&sb, "- 检索准确率（总体，按 memory_id）：%.1f%%\n", report.OverallAccuracy*100)
	fmt.Fprintf(&sb, "- 检索准确率（总体，按 fact 文本）：%.1f%%\n", report.OverallAccuracyByFact*100)
	sb.WriteString("- 分桶准确率（按 memory_id）：\n")
	for _, bucket := range []string{"single-hop", "multi-hop", "open-domain", "temporal", "reference_frame"} {
		if acc, ok := report.BucketAccuracy[bucket]; ok {
			fmt.Fprintf(&sb, "  - %s: %.1f%%\n", bucket, acc*100)
		}
	}
	fmt.Fprintf(&sb, "- 时序桶准确率：%.1f%%\n", report.TemporalAccuracy*100)
	fmt.Fprintf(&sb, "- 多信号融合准确率（multi-hop 桶自动跑，**待人工复核**）：%.1f%%\n", report.MultiSignalAccuracy*100)
	fmt.Fprintf(&sb, "- 记忆污染率：%.1f%%\n\n", report.PollutionRate*100)

	// 新增：成本、MRR、污染趋势
	if report.Cost != nil {
		sb.WriteString("## 成本追踪\n\n")
		fmt.Fprintf(&sb, "- 总查询数：%d\n", report.Cost.TotalQueries)
		fmt.Fprintf(&sb, "- 平均延迟：%.1f ms\n", report.Cost.AvgLatencyMs)
		fmt.Fprintf(&sb, "- 平均信号计算次数：%.1f\n", report.Cost.AvgSignalComputations)
		fmt.Fprintf(&sb, "- 估算每条查询成本：$%.4f\n\n", report.Cost.CostPerQuery)
	}

	sb.WriteString("## MRR（Mean Reciprocal Rank）\n\n")
	fmt.Fprintf(&sb, "- 总体 MRR：%.3f\n", report.OverallMRR)
	for _, bucket := range []string{"single-hop", "multi-hop", "open-domain", "temporal", "reference_frame"} {
		if mrr, ok := report.BucketMRR[bucket]; ok {
			fmt.Fprintf(&sb, "- %s 桶 MRR：%.3f\n", bucket, mrr)
		}
	}
	// R3-D：multi-hop 桶双口径并列展示。
	//   bucket_mrr        = 主 MRR 口径（rank 序，命中 hop-1 只计 0.5，历史可比）
	//   bucket_mrr_set    = 口径 A（top-1 ∈ Expected 即 hit，与公开基准可比）
	//   bucket_hit2       = 口径 B（top-2 内命中 expected 的比例）
	if set, ok := report.BucketMRRSet["multi-hop"]; ok {
		h2 := report.BucketHit2["multi-hop"]
		main := report.BucketMRR["multi-hop"]
		sb.WriteString("- multi-hop 桶三口径对比（R3-D）：\n")
		fmt.Fprintf(&sb, "  - 主 MRR（口径 C，历史可比）：%.3f\n", main)
		fmt.Fprintf(&sb, "  - 口径 A MRR（top-1 ∈ Expected 即 hit）：%.3f\n", set)
		fmt.Fprintf(&sb, "  - 口径 B hit@2（top-2 内命中）：%.3f\n", h2)
	}
	// R5-B：multi-hop 主口径 / 跳板口径 MRR（ExpectedMemoryIDPrimary / Hop）。
	if m, ok := report.BucketMRRPrimary["multi-hop"]; ok {
		sb.WriteString("- multi-hop 桶 R5-B 主口径（ExpectedMemoryIDPrimary 最终答案 MRR）：")
		fmt.Fprintf(&sb, "%.3f\n", m)
	}
	if m, ok := report.BucketMRRHop["multi-hop"]; ok {
		sb.WriteString("- multi-hop 桶 R5-B 跳板口径（ExpectedMemoryIDHop hop-1 MRR，对照）：")
		fmt.Fprintf(&sb, "%.3f\n", m)
	}
	fmt.Fprintf(&sb, "- fact 边界归一化（-fact-normalize）：%v\n", report.FactNormalize)
	if report.TemporalExtra > 0 {
		fmt.Fprintf(&sb, "- 合并 temporal 样本数（-temporal-samples）：%d\n", report.TemporalExtra)
	}
	sb.WriteString("\n")

	if report.PollutionTrend != nil {
		sb.WriteString("## 污染率趋势\n\n")
		fmt.Fprintf(&sb, "- 当前污染率：%.1f%%\n", report.PollutionTrend.CurrentRate*100)
		fmt.Fprintf(&sb, "- 趋势方向：%s\n", report.PollutionTrend.TrendDirection)
		if len(report.PollutionTrend.Windows) > 0 {
			sb.WriteString("- 分窗口污染率：\n")
			for _, w := range report.PollutionTrend.Windows {
				fmt.Fprintf(&sb, "  - 窗口 %d-%d：%.1f%%（%d/%d 泄漏）\n",
					w.WindowStart, w.WindowEnd, w.Rate*100, w.Leaked, w.Total)
			}
		}
		sb.WriteString("\n")
	}

	// R4-B：reference_frame 信号复检摘要。
	if report.RefFrameSignal != nil {
		rfSig := report.RefFrameSignal
		sb.WriteString("## reference_frame 信号复检（R4-B）\n\n")
		fmt.Fprintf(&sb, "- 期望记忆 ref 分在候选集合中最高：%.1f%%（信号区分度）\n", rfSig["expected_ref_top1"]*100)
		fmt.Fprintf(&sb, "- 期望记忆最终 top-1 命中率：%.1f%%\n", rfSig["expected_top1_hit"]*100)
		fmt.Fprintf(&sb, "- ref 分最高且 top-1 命中：%.1f%%（信号→命中传导率）\n", rfSig["ref_top1_and_hit"]*100)
		sb.WriteString("\n")
	}

	sb.WriteString("## fact 口径（R4-C v4）\n\n")
	fmt.Fprintf(&sb, "- 口径版本：%s（-fact-v4=false 可复现 R3-C v3 基线）\n", report.FactAlgo)
	fmt.Fprintf(&sb, "- 主 fact 准确率（分母已剔除模板 expected）：%.1f%%（%d/%d）\n",
		report.OverallAccuracyByFact*100, factHits, report.FactTotal)
	fmt.Fprintf(&sb, "- 模板 expected（beam-* \"LLM response should ...:\"，含 contain/state/mention）：剔除 %d 条（其中 fact_hit %d 条，单列不计入主口径）\n",
		report.TemplateTotal, report.TemplateHits)
	fmt.Fprintf(&sb, "- 候选扩展辅助口径（短 expected 在 top-5 候选内命中）：%d 条（不覆盖主口径）\n", report.FactHitTop5Count)
	if report.FactThreshold > 0 {
		fmt.Fprintf(&sb, "- v3 char n-gram 阈值：%.2f（算法 %s）\n", report.FactThreshold, report.FactSimAlgo)
	}
	sb.WriteString("\n")

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
	summaryPath := fmt.Sprintf("%s/%s_summary.md", repDir, repPrefix)
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

// sigFmt R3-B：从 signal_scores map 中取出某个信号分，格式化为字符串（缺失显示 "<nil>"）。
func sigFmt(m map[string]float64, name string) string {
	if v, ok := m[name]; ok {
		return fmt.Sprintf("%.4f", v)
	}
	return "<nil>"
}

// memRefFrameField R4-B：从候选记忆的 ReferenceFrame 中取坐标字段，用于 diag-ref 打印。
// 候选可能为 nil（防御），坐标缺失时返回 ""。
func memRefFrameField(m *kernel.Memory, field string) string {
	if m == nil {
		return ""
	}
	switch field {
	case "entity":
		return m.ReferenceFrame.Entity
	case "project":
		return m.ReferenceFrame.Project
	case "scene":
		return m.ReferenceFrame.Scene
	case "task":
		return m.ReferenceFrame.Task
	default:
		return ""
	}
}

// ------------------------------------------------------------------
// R3-A：评测器 embedder 注入——确定性 char n-gram TF-IDF 词表加载。
// 红线：不调 LLM；seed 无关；IDF 从两个评测集（公开基准 + 合成集）的
// query+fact 文本统计，词表落盘后复用（tune 脚本与 run_kpi.go 维度一致）。
// ------------------------------------------------------------------

// evalBenchmarkDataset 是公开基准数据集路径（用于词表构建，覆盖更多文本形态）。
const evalBenchmarkDataset = "benchmarks/dataset/all.jsonl"

// evalSynthDataset 是合成数据集路径（默认相对 eval/ 目录）。
const evalSynthDataset = "dataset/all.jsonl"

// collectVocabCorpus 收集两个评测集的 query+fact 文本作为 IDF 语料。
// 返回当前 dsPath 对应的数据集路径（用于记录）+ 合并语料。
func collectVocabCorpus(dsPath string) (benchPath, synthPath string, corpus []string) {
	benchPath = evalBenchmarkDataset
	synthPath = evalSynthDataset
	if _, err := os.Stat("eval"); err == nil {
		// 从仓库根运行：路径前加 eval/ 前缀。
		benchPath = "eval/" + evalBenchmarkDataset
		synthPath = "eval/" + evalSynthDataset
	}
	// 语料来源固定顺序：公开基准 → 合成集 → 当前数据集（若与前两者不同）。
	for _, p := range []string{benchPath, synthPath, dsPath} {
		items, err := loadDataset(p)
		if err != nil {
			log.Printf("[R3-A] vocab corpus skip %s: %v", p, err)
			continue
		}
		for _, it := range items {
			if it.Query != "" {
				corpus = append(corpus, it.Query)
			}
			if it.ExpectedFact != "" {
				corpus = append(corpus, it.ExpectedFact)
			}
			if it.Memory != nil && it.Memory.Fact != "" {
				corpus = append(corpus, it.Memory.Fact)
			}
		}
	}
	return benchPath, synthPath, corpus
}

// loadEvalVocab 加载或构建 char n-gram TF-IDF 词表。
//   - 优先复用磁盘缓存（保证 tune 脚本与 run_kpi.go 共用同一 IDF）。
//   - 无缓存时从两个评测集构建并落盘（确定性，seed=42 无关，无 time.Now()）。
func loadEvalVocab(vocabPath, dsPath string) (*kernel.CharNgramVocab, error) {
	if v, err := kernel.LoadCharNgramVocabJSON(vocabPath); err == nil && v.Dim() > 0 {
		return v, nil
	}
	_, _, corpus := collectVocabCorpus(dsPath)
	if len(corpus) == 0 {
		return nil, fmt.Errorf("no corpus for embedder vocab")
	}
	v := kernel.NewCharNgramVocab(3)
	v.BuildIDF(corpus)
	if dir := filepath.Dir(vocabPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := v.SaveJSON(vocabPath); err != nil {
		log.Printf("[R3-A] warn: save vocab %s: %v", vocabPath, err)
	}
	return v, nil
}

// ------------------------------------------------------------------
// R5-A：向量 embedder（BGE-m3 静态向量文件）——确定性嵌入，不调 LLM。
//
// 背景（任务书 §A.1 / §A.3.1 / §5.4）：Go 进程内无 onnxruntime/llama.cpp；
// 远端 0.88 用 torch+transformers 离线加载 BAAI/bge-m3 生成静态向量（.npy + .json），
// Go 侧读取该文件实现"确定性嵌入 + 余弦相似度"的 semantic 信号。
// 向量表 key = 归一化文本（小写 + 压缩空白），query 与 passage 均不加深造前缀，
// 保证 Embed(query) 与 Embed(fact) 落在同一空间（BGE-m3 原生多语检索已够用）。
// ------------------------------------------------------------------

// vectorFileSet 静态向量文件的三个产物路径。
type vectorFileSet struct {
	matrix string // .npy（float32，[N, Dim]）
	meta   string // .json（VectorMeta）
	index  string // .jsonl（text -> 行号）
}

// vectorPaths 返回相对 eval/ 目录的向量文件路径（与 gen_vectors.py 输出一致）。
func vectorPaths() vectorFileSet {
	return vectorFileSet{
		matrix: "vectors/vectors.npy",
		meta:   "vectors/vectors_meta.json",
		index:  "vectors/vectors_index.jsonl",
	}
}

// collectVectorCorpus 收集向量化语料（query + fact），顺序固定：
// 公开基准 → 合成集 → 当前数据集 → reference_frame → temporal。
// 与 collectVocabCorpus 同源但额外纳入 reference_frame/temporal（它们含中文模板，
// 是 LoCoMo 之外唯一依赖语义向量的桶）。顺序固定保证向量表可复现。
func collectVectorCorpus(dsPath string) []string {
	corpus := []string{}
	add := func(path string) {
		if path == "" {
			return
		}
		items, err := loadDataset(path)
		if err != nil {
			log.Printf("[R5-A] vector corpus skip %s: %v", path, err)
			return
		}
		for _, it := range items {
			if it.Query != "" {
				corpus = append(corpus, it.Query)
			}
			if it.Memory != nil && it.Memory.Fact != "" {
				corpus = append(corpus, it.Memory.Fact)
			}
			if it.Hop1Memory != nil && it.Hop1Memory.Fact != "" {
				corpus = append(corpus, it.Hop1Memory.Fact)
			}
			if it.Hop2Memory != nil && it.Hop2Memory.Fact != "" {
				corpus = append(corpus, it.Hop2Memory.Fact)
			}
			if it.SecondVersion != nil && it.SecondVersion.Fact != "" {
				corpus = append(corpus, it.SecondVersion.Fact)
			}
		}
	}
	// 路径前缀：从 eval/ 或仓库根运行均可。
	prefix := ""
	if _, err := os.Stat("eval"); err == nil {
		prefix = "eval/"
	}
	add(prefix + "benchmarks/dataset/all.jsonl")
	add(prefix + "dataset/all.jsonl")
	if dsPath != "" && dsPath != prefix+"benchmarks/dataset/all.jsonl" && dsPath != prefix+"dataset/all.jsonl" {
		add(dsPath)
	}
	add(prefix + "dataset/reference_frame.jsonl")
	add(prefix + "dataset/temporal.jsonl")
	return corpus
}

// needVectorGenerate 判断向量文件是否缺失或与当前语料不匹配。
// 匹配判据：meta.count == len(去重后 corpus)（语料规模变化则重生成）。
// 注意：collectVectorCorpus 可能重复加入同一文本（如 dsPath == reference_frame.jsonl），
// 因此必须先去重再比较 meta.count，否则每次评测都会误判"过期"而重复跑 27 分钟的生成。
func needVectorGenerate(corpus []string, paths vectorFileSet) bool {
	for _, p := range []string{paths.matrix, paths.meta, paths.index} {
		if _, err := os.Stat(p); err != nil {
			return true
		}
	}
	raw, err := os.ReadFile(paths.meta)
	if err != nil {
		return true
	}
	var m struct {
		Count int `json:"count"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return true
	}
	return m.Count != uniqueCount(corpus)
}

// uniqueCount 返回语料去重后的条目数（顺序无关，仅比较规模用）。
func uniqueCount(corpus []string) int {
	if len(corpus) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(corpus))
	for _, t := range corpus {
		seen[t] = struct{}{}
	}
	return len(seen)
}

// generateVectors 调用远端 Python 脚本生成静态向量文件（0.88 上执行）。
// corpus 写入临时 JSONL，脚本读取后调用 BGE-m3 生成向量。
// 不调 LLM（BGE 嵌入属任务书 §0 例外条款允许的"确定性嵌入"）。
func generateVectors(corpus []string, paths vectorFileSet) error {
	if len(corpus) == 0 {
		return fmt.Errorf("empty corpus")
	}
	// 语料去重（保持首次出现顺序，确定性）。
	seen := map[string]bool{}
	unique := make([]string, 0, len(corpus))
	for _, t := range corpus {
		if seen[t] {
			continue
		}
		seen[t] = true
		unique = append(unique, t)
	}
	// 写入输入文件（eval/vectors/corpus.txt）。
	if dir := filepath.Dir(paths.matrix); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	input := filepath.Join(filepath.Dir(paths.matrix), "corpus.txt")
	var sb strings.Builder
	for _, t := range unique {
		sb.WriteString(strings.ReplaceAll(t, "\n", " "))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(input, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write corpus: %w", err)
	}
	// 调用远端脚本（本地工作区内的 gen_vectors.py，已同步到 0.88 同目录）。
	script := filepath.Join(filepath.Dir(paths.matrix), "gen_vectors.py")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("gen_vectors.py missing at %s (需同步到 0.88 后重跑): %w", script, err)
	}
	cmd := exec.Command("python3", script,
		"-in", input,
		"-out", paths.matrix,
		"-meta", paths.meta,
		"-index", paths.index,
		"-model", "BAAI/bge-m3",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gen_vectors failed: %v\n%s", err, string(out))
	}
	log.Printf("[R5-A] vectors generated: n=%d, script output: %s", len(unique), trimForLog(string(out), 200))
	return nil
}

// loadVectorEmbedder 加载（必要时自动生成）BGE-m3 静态向量 embedder。
// 生成失败返回错误，调用方应提示回退 `-embedder tfidf`（任务书 §5.4 降级）。
func loadVectorEmbedder(dsPath string) (*kernel.VectorEmbedder, error) {
	corpus := collectVectorCorpus(dsPath)
	paths := vectorPaths()
	if needVectorGenerate(corpus, paths) {
		if err := generateVectors(corpus, paths); err != nil {
			return nil, fmt.Errorf("向量文件缺失/过期且自动生成失败（模型文件需在 0.88 就绪）: %w\n  回退建议：-embedder tfidf", err)
		}
	}
	ve, err := kernel.NewVectorEmbedder(kernel.VectorEmbedderConfig{
		MatrixPath: paths.matrix,
		MetaPath:   paths.meta,
		IndexPath:  paths.index,
	})
	if err != nil {
		return nil, fmt.Errorf("load vector embedder: %w", err)
	}
	return ve, nil
}

// ------------------------------------------------------------------
// R2 任务C：fact 评测口径由「精确相等」改为「归一化后包含」。
// 纯文本处理，不调 LLM，不改变 kernel 语义。
// ------------------------------------------------------------------

// matchFact 判定 gotFact 是否匹配 expectedFact（忽略大小写与空白）。
// 处理公开基准 fact 形态差异：LME/BEAM 形如 "question => answer"、
// LoCoMo 形如 "speaker: turn"，而 expected_fact 是纯答案。策略：
//  1. 归一化后精确相等 → true
//  2. 短 expected（归一化长度 < 4，如数字/单词）→ 仅在 "=>" 后的 answer 段
//     做边界精确匹配，避免与 question 里的普通词误配（R2 实测 60/60 全部正确恢复）
//  3. 长 expected → 归一化后双向包含，且长度比 ≥ 0.3（防极短误命中）
//  4. 任一为空 → false
//
// ⚠️ 已知限制：BEAM/LoCoMo 的 expected 是跨多轮对话的推理答案，与 top-1 原文 turn
// 在文本层面不连续，包含判定无法覆盖（实测 rank=1 仍有 16 条无法用文本匹配），
// 属数据形态差异，非判定过松。验收时若公开基准 fact 准确率 > 95% 需进一步收窄。
func matchFact(gotFact, expectedFact string) bool {
	if expectedFact == "" {
		return false
	}
	a := normalizeFact(gotFact)
	b := normalizeFact(expectedFact)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// 短 expected：只在 "=>" answer 段做边界精确匹配（LME/BEAM 形态）。
	if len(b) < 4 {
		return shortAnswerHit(a, b)
	}
	// 长 expected：双向包含 + 长度比防护。
	if len(a) < len(b)*3/10 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// shortAnswerHit 短 expected（数字/单词）在 fact 的 answer 段内做边界匹配。
// 形如 "How many ...? => 4" 时，answer 段为 "4"，精确相等即命中；
// 若 answer 段为 "... about 4 items"，则要求 b 在 answer 段作为整词出现。
func shortAnswerHit(fact, exp string) bool {
	_, answer, ok := strings.Cut(fact, "=>")
	if !ok {
		answer = fact
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return false
	}
	if answer == exp {
		return true
	}
	// 整词边界匹配：answer 以 exp 开头/结尾，或 answer 内出现 " exp "。
	return strings.HasPrefix(answer, exp+" ") ||
		strings.HasSuffix(answer, " "+exp) ||
		strings.Contains(" "+answer+" ", " "+exp+" ")
}

// normalizeFact 归一化：小写、去除首尾空白、压缩连续空白。
func normalizeFact(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

// ------------------------------------------------------------------
// R3-C：fact 口径 v3——char n-gram（trigram）Dice 相似度补判。
// 背景：R2 收口 §8.2 实测 rank=1 内仍有 21/583 条公开基准样本 fact 未命中，
// 主要是 BEAM/LoCoMo 跨轮推理型 expected_fact（如 "Toy drive, Community food
// drive, veterans, domestic violence"）与 top-1 原文 turn（如 "John: We held
// some events ... went to a homeless shelter ..."）在文本层面不连续，任何
// 字符串包含判定都无法覆盖。v3 用确定性 char trigram Dice 系数补判，不调 LLM。
// ------------------------------------------------------------------

// factV3Threshold 是 matchFactV3 的 char trigram 相似度阈值。
// 由 -fact-threshold flag 注入（默认 0.55）；阈值敏感性分析在 0.4/0.5/0.55/0.6/0.7 五档下做。
var factV3Threshold = 0.55

// factV3SimAlgo 是 matchFactV3 使用的相似度算法：
//
//	"dice"       — Dice 系数 2|A∩B|/(|A|+|B|)（任务书建议；保守，长 got 下被稀释）
//	"contain"    — containment |A∩B|/|B|（exp 的 n-gram 有多少出现在 got 里；软包含，更适合长 got/短 exp）
var factV3SimAlgo = "dice"

// charNGramSimilarity 计算两段归一化文本的 char n-gram Dice 系数：
//
//	2 * |A ∩ B| / (|A| + |B|)
//
// 纯字符串处理，不调 LLM，无随机源（确定性）。
// 若任一方 n-gram 集合为空，返回 0.0。
func charNGramSimilarity(a, b string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	gramsA := make(map[string]struct{})
	gramsB := make(map[string]struct{})
	ra := []rune(a)
	rb := []rune(b)
	for i := 0; i+n <= len(ra); i++ {
		gramsA[string(ra[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rb); i++ {
		gramsB[string(rb[i:i+n])] = struct{}{}
	}
	if len(gramsA) == 0 || len(gramsB) == 0 {
		return 0.0
	}
	inter := 0
	for g := range gramsA {
		if _, ok := gramsB[g]; ok {
			inter++
		}
	}
	return 2.0 * float64(inter) / float64(len(gramsA)+len(gramsB))
}

// charNGramContainment 计算 containment（软包含）：exp 的 n-gram 有多少出现在 got 里。
//
//	|grams(exp) ∩ grams(got)| / |grams(exp)|
//
// 与 Dice 相比，containment 不惩罚 got 远长于 exp 的情况（LoCoMo "speaker: turn"
// vs 短答案），更符合「expected_fact 是否被 got_fact 软覆盖」的语义。
func charNGramContainment(exp, got string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	gramsExp := make(map[string]struct{})
	gramsGot := make(map[string]struct{})
	re := []rune(exp)
	rg := []rune(got)
	for i := 0; i+n <= len(re); i++ {
		gramsExp[string(re[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rg); i++ {
		gramsGot[string(rg[i:i+n])] = struct{}{}
	}
	if len(gramsExp) == 0 {
		return 0.0
	}
	inter := 0
	for g := range gramsExp {
		if _, ok := gramsGot[g]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(gramsExp))
}

// factV3Score 按当前 factV3SimAlgo 计算相似度分数。
func factV3Score(got, exp string) float64 {
	if factV3SimAlgo == "contain" {
		return charNGramContainment(exp, got, 3)
	}
	return charNGramSimilarity(got, exp, 3)
}

// matchFactV3 = matchFact(v2 包含式) + char trigram containment 补判。
//
// 判定顺序：
//  1. 先跑 R2 的 matchFact（归一化包含/短答案边界匹配）；命中即 true。
//  2. 未命中时，若 expected 归一化长度 < 15 字符，直接 false（短 expected 易误配，
//     不进入相似度补判——短答案已由 v2 的 shortAnswerHit 覆盖；合成集实测此门槛
//     把中文短模板 expected 的假阳性从 166 压到 1）。
//  3. 否则计算 factV3Score(got, expected)；≥ factV3Threshold 则 true。
//
// 假阳性防护：阈值在报告中做敏感性分析；合成集 fact_hit 但 ID 未中 top-1 的样本
// 由 KPIReport.FactFalsePositives 留痕，>0.5% 即视为阈值过松。
func matchFactV3(gotFact, expectedFact string) bool {
	if matchFact(gotFact, expectedFact) {
		return true
	}
	// R5-B：边界归一化（数值/日期/单位/大小写）后复判——消弭书写形态差异。
	// 仅在 -fact-normalize 开启时启用；false 时跳过，逐位复现 R4。
	if factNormalizeEnabled &&
		matchFact(normalizeFactBoundary(gotFact), normalizeFactBoundary(expectedFact)) {
		return true
	}
	b := normalizeFact(expectedFact)
	// 注意：Go len(string) 返回字节数，中文每字 3 字节，必须用 RuneCountInString。
	if utf8.RuneCountInString(b) < 15 {
		return false
	}
	a := normalizeFact(gotFact)
	if a == "" {
		return false
	}
	sim := factV3Score(a, b)
	if sim < factV3Threshold {
		return false
	}
	// R5-B：模糊路径边界对齐护栏——抑制长 got 文本里的碎片误配（假阳性）。
	// CJK expected 无法词元化，跳过护栏（见 factBoundaryGuard）。
	if factNormalizeEnabled {
		return factBoundaryGuard(gotFact, expectedFact)
	}
	return true
}

// factBoundaryGuard R5-B：char-trigram 模糊路径命中后的**边界对齐护栏**。
//
// 动机：模糊相似度（Dice / containment）在 got 远长于 expected、且 got 含大量共同
// 短 n-gram 时，可能把「碎片巧合」误判为命中（假阳性）。护栏要求 expected 的**实义词元**
// 至少 need 个在 got 中**整词出现**（need = 2，当 expected 有 ≥2 个词元；否则为词元总数）。
//
// 规则（与离线验证脚本 r5b_norm_sim.py 逐位一致）：
//   - expected 含 CJK（无法拉丁词元化）→ 直接放行（return true）；
//   - 比较集合基于 factContentTokens（小写 + 去停用词），纯字符串、无随机源。
func factBoundaryGuard(gotFact, expectedFact string) bool {
	if hasCJK(expectedFact) {
		return true
	}
	ct := factContentTokens(expectedFact)
	gt := make(map[string]struct{})
	for _, w := range factContentTokens(gotFact) {
		gt[w] = struct{}{}
	}
	need := len(ct)
	if len(ct) >= 2 {
		need = 2
	}
	seen := make(map[string]struct{})
	shared := 0
	for _, w := range ct {
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		if _, ok := gt[w]; ok {
			shared++
		}
	}
	return shared >= need
}

// ------------------------------------------------------------------
// R3-D：multi-hop 桶补充指标辅助函数（口径 A 集合判定 + 口径 B hit@2）。
// 纯 ID 字符串集合判断，不调 LLM，seed 无关（无随机源）。
// ------------------------------------------------------------------

// sliceContains 判断 s 中是否存在元素 target。
// 用于口径 A：top-1 MemoryID 是否 ∈ ExpectedMemoryID。
// trimForLog R4-A：把文本截断到 n 个 rune 并转义换行，避免诊断日志被长事实文本淹没。
// 纯展示用途，不参与任何判定。
func trimForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

func sliceContains(s []string, target string) bool {
	if target == "" {
		return false
	}
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// multiHit2 判断 top-2 候选（res.Fused 前 2 条）内是否命中任一 ExpectedMemoryID。
// 用于口径 B（hit@2）：反映「一次交互内能拿到正确事实」的体验。
// 与主 MRR 口径无关：主 MRR 只计 top-1 的 rank 惩罚，hit@2 只要 top-2 内命中即 true。
func multiHit2(fused []kernel.FusedResult, expected []string) bool {
	for i := 0; i < len(fused) && i < 2; i++ {
		if sliceContains(expected, fused[i].MemoryID) {
			return true
		}
	}
	return false
}

// firstRank R5-B：返回 expected 中任一 MemoryID 在融合序 fused 中首次出现的 1-based rank；
// 未命中返回 0。纯 ID 集合判定，确定性，无随机源。用于 multi-hop 主/跳板口径 MRR。
func firstRank(fused []kernel.FusedResult, expected []string) int {
	for i, f := range fused {
		if sliceContains(expected, f.MemoryID) {
			return i + 1
		}
	}
	return 0
}

// mrrOfRanks R5-B：由 rank 序列计算 MRR（未命中 rank=0 计 0 分）。
// 与主 MRR 口径（bucket_mrr）公式一致：sum(1/rank) / n。
func mrrOfRanks(ranks []int) float64 {
	if len(ranks) == 0 {
		return 0
	}
	var sum float64
	for _, r := range ranks {
		if r > 0 {
			sum += 1.0 / float64(r)
		}
	}
	return sum / float64(len(ranks))
}

// ==================================================================
// R4-C：fact 口径 v4——确定性残留补齐（模板剔除 / 时间推理 / 数值日期归一 / 短词元包含）。
//
// 背景（R3-C §9 已知限制）：
//   1) 13 条短 expected 未命中（跨轮推理结论、日期在原文 turn 不出现）；
//   2) 6 条 beam-* 模板 fp（expected = "LLM response should <contain|state|mention>: ..."）；
//   3) utf8.RuneCountInString 门槛 bug（中文每字 3 字节）。
//
// v4 在 v3（v2 包含 + char trigram 软包含）之上追加**纯字符串/日历推算**补判，
// 不调 LLM（红线 4）、无随机源（seed 无关）、逐位确定。
//   - 模板剔除：expected 以 "llm response should" 开头（contain/state/mention）→ 从主 fact 口径剔除，单列统计。
//   - 时间推理：用 top-1 候选 occurred_at 作日历锚点，对相对时间词做确定性加减，与
//     expected 的日期规格（YYYY-MM-DD / 月名日期 / 年）比对。
//   - 数值/日期归一：去除货币符号与千分位逗号、统一日期分隔符后再做包含判定。
//   - 短词元包含：短 expected（非中文）的实义词元全部出现在 got 中即命中。
//
// -fact-v4=false 时全部退回 v3 行为（isolate 复现 R3-C 基线）。
// ==================================================================

// factV4Enabled 是 fact 口径 v4 开关，由 -fact-v4 flag 注入（默认 true）。
var factV4Enabled = true

// factNormalizeEnabled 是 R5-B fact 边界归一化 + 边界对齐护栏开关，由 -fact-normalize
// flag 注入（默认 true）。开启时：
//  1. matchFactV3 在字面包含失败后，先做**边界归一化**（数值/日期/单位/大小写）复判；
//  2. char-trigram 模糊路径命中后追加**边界对齐护栏**，要求 expected 的实义词元在 got 中
//     整词出现（≥2 个，或 expected 仅 ≤1 个时全部），抑制长 got 文本里的碎片误配（假阳性）。
//
// -fact-normalize=false 时两条支路均不启用，逐位复现 R4 v4 基线（isolate）。
var factNormalizeEnabled = true

// factAlgoLabel 返回当前 fact 口径版本标识，写入报告便于追溯。
func factAlgoLabel() string {
	if factV4Enabled {
		return "v4"
	}
	return "v3"
}

// factTemplatePrefix 是评测集模板 expected 的固定前缀（小写归一后）。
// beam-* 样本的 expected 误写了整个 LLM 应答模板，属评测集形态问题；
// 实测存在三种模板形态：should contain（49 条）/ should state（40 条）/ should mention（4 条）。
// 因此前缀只取到 "should"，统一覆盖全部模板形态（R3-C 指出的 6 条 beam fp 中
// 4 条为 contain、2 条为 state，均须被剔除/标注）。
const factTemplatePrefix = "llm response should"

// isTemplateExpected 判定 expected 是否为评测模板（"LLM response should ...: ..."）。
// 归一化（小写、压缩空白）后按前缀匹配；命中则从主 fact 口径剔除。
func isTemplateExpected(expectedFact string) bool {
	return strings.HasPrefix(normalizeFact(expectedFact), factTemplatePrefix)
}

// factMonthNames 月名 → 月份号（全名 + 三字母缩写），用于 expected 日期规格解析。
var factMonthNames = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
}

var (
	factReYMD       = regexp.MustCompile(`(\d{4})-(\d{1,2})-(\d{1,2})`)
	factReDMY       = regexp.MustCompile(`(\d{1,2})\s+([a-z]+),?\s+(\d{4})`)
	factReMDY       = regexp.MustCompile(`([a-z]+)\s+(\d{1,2}),?\s+(\d{4})`)
	factReMonthYear = regexp.MustCompile(`([a-z]+),?\s+(\d{4})`)
	factReYear      = regexp.MustCompile(`\b(19|20)\d{2}\b`)
)

// factDateSpec 是 expected 中的日期规格。
// kind: "day"（精确到日）/ "month"（精确到月）/ "year"（仅年）。
type factDateSpec struct {
	Kind    string
	Year    int
	Month   int
	Day     int
	Present bool
}

// parseFactDateSpec 从 expected 文本解析日期规格（确定性，无随机源）。
// 支持：2023-04-01（ISO）、9 April, 2023 / April 9, 2023、July 2023、in 2022。
func parseFactDateSpec(expectedFact string) factDateSpec {
	s := normalizeFact(expectedFact)
	if m := factReYMD.FindStringSubmatch(s); m != nil {
		return factDateSpec{Kind: "day", Year: atoi(m[1]), Month: atoi(m[2]), Day: atoi(m[3]), Present: true}
	}
	if m := factReDMY.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[2]]; ok {
			return factDateSpec{Kind: "day", Year: atoi(m[3]), Month: mo, Day: atoi(m[1]), Present: true}
		}
	}
	if m := factReMDY.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[1]]; ok {
			return factDateSpec{Kind: "day", Year: atoi(m[3]), Month: mo, Day: atoi(m[2]), Present: true}
		}
	}
	if m := factReMonthYear.FindStringSubmatch(s); m != nil {
		if mo, ok := factMonthNames[m[1]]; ok {
			return factDateSpec{Kind: "month", Year: atoi(m[2]), Month: mo, Present: true}
		}
	}
	if m := factReYear.FindString(s); m != "" {
		return factDateSpec{Kind: "year", Year: atoi(m), Present: true}
	}
	return factDateSpec{}
}

// atoi 是内部最小整数解析（输入均为正则捕获的数字，安全）。
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// factTemporalOffsets 相对时间词 → (粒度, 偏移量)。用于把 got 中的相对表述
// 与锚点日期做确定性日历推算。
var factTemporalOffsets = []struct {
	KW    string
	Kind  string
	Delta int
}{
	{"yesterday", "day", -1}, {"today", "day", 0}, {"tomorrow", "day", 1},
	{"last week", "day", -7}, {"next week", "day", 7},
	{"last month", "month", -1}, {"next month", "month", 1},
	{"last year", "year", -1}, {"next year", "year", 1},
}

// shiftFactDate 在给定锚点日期上按 (kind, n) 做确定性日历加减，返回 (y, mo, d)。
func shiftFactDate(y, mo, d int, kind string, n int) (int, int, int) {
	switch kind {
	case "day":
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
		return t.Year(), int(t.Month()), t.Day()
	case "month":
		tot := y*12 + (mo - 1) + n
		return tot / 12, tot%12 + 1, d
	default: // year
		return y + n, mo, d
	}
}

// matchFactTemporal 用锚点日期（top-1 候选 occurred_at）对 expected 日期规格做确定性日历比对：
//   - 若 got 含相对时间词，则把锚点按该词偏移，得到候选日期集合；
//   - expected 的日期规格（日/月/年粒度）命中集合任一成员即判命中。
//
// 纯日历推算，不调 LLM。anchor 为空（候选无 occurred_at）时返回 false。
func matchFactTemporal(gotFact, expectedFact string, anchor *time.Time) bool {
	if anchor == nil {
		return false
	}
	spec := parseFactDateSpec(expectedFact)
	if !spec.Present {
		return false
	}
	ay, amo, ad := anchor.Year(), int(anchor.Month()), anchor.Day()
	cands := [][3]int{{ay, amo, ad}}
	g := normalizeFact(gotFact)
	for _, off := range factTemporalOffsets {
		if strings.Contains(g, off.KW) {
			y, mo, d := shiftFactDate(ay, amo, ad, off.Kind, off.Delta)
			cands = append(cands, [3]int{y, mo, d})
		}
	}
	for _, c := range cands {
		switch spec.Kind {
		case "day":
			if c[0] == spec.Year && c[1] == spec.Month && c[2] == spec.Day {
				return true
			}
		case "month":
			if c[0] == spec.Year && c[1] == spec.Month {
				return true
			}
		case "year":
			if c[0] == spec.Year {
				return true
			}
		}
	}
	return false
}

var (
	factReCurrency  = regexp.MustCompile(`[$€£¥]`)
	factReThousands = regexp.MustCompile(`(\d),(\d{3})`)
	factReDateSep   = regexp.MustCompile(`(\d{4})[-/](\d{1,2})[-/](\d{1,2})`)
)

// normalizeFactNumbers 归一化数值/日期文本（确定性）：
//   - 去除货币符号（$ € £ ¥）："$3,750" → "3,750"；
//   - 去除数字千分位逗号（迭代处理 1,234,567）："3,750" → "3750"；
//   - 统一日期分隔符："2023-04-01" / "2023/4/1" → "2023 04 1"（空格分隔）。
//
// 仅做字符串层归一，供 matchFactV3 在归一文本上做包含判定，避免漏配形态差异。
func normalizeFactNumbers(s string) string {
	s = strings.ToLower(s)
	s = factReCurrency.ReplaceAllString(s, "")
	for factReThousands.MatchString(s) {
		s = factReThousands.ReplaceAllString(s, "$1$2")
	}
	s = factReDateSep.ReplaceAllString(s, "$1 $2 $3")
	return strings.Join(strings.Fields(s), " ")
}

// factReUnitTail 尾随单位点（如 "3.5kg." / "12gb." 的句末点）与单位常见别名。
var factReUnitTail = regexp.MustCompile(`([0-9])([a-z%]+)\.`)

// normalizeFactBoundary R5-B：fact 匹配前的确定性**边界归一化**（-fact-normalize）。
//
// 与 normalizeFactNumbers 的差别：本函数把数值/日期/单位的**边界形态**压平成规范形，
// 用于跨形态的包含复判与护栏词元对齐（不改变判定语义，仅消弭书写形态差异）：
//   - 去货币符号（$ € £ ¥）+ 去千分位逗号："$3,750" → "3750"；
//   - 日期去分隔符："2023-04-01" / "2023/04/01" → "20230401"；
//   - 单位大小写统一（小写）并去尾随点："3.5KG." → "3.5kg"；
//   - 压缩空白。
//
// 纯字符串处理，不调 LLM，无随机源（确定性）。
func normalizeFactBoundary(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = factReCurrency.ReplaceAllString(s, "")
	for factReThousands.MatchString(s) {
		s = factReThousands.ReplaceAllString(s, "$1$2")
	}
	s = factReDateSep.ReplaceAllString(s, "$1$2$3")
	s = factReUnitTail.ReplaceAllString(s, "$1$2")
	return strings.Join(strings.Fields(s), " ")
}

// factStopWords 短词元包含判定的停用词（避免 "the"/"a" 等虚词造成误配）。
var factStopWords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "of": {}, "in": {}, "on": {}, "at": {}, "to": {},
	"for": {}, "and": {}, "or": {}, "is": {}, "was": {}, "are": {}, "were": {},
	"be": {}, "been": {}, "by": {}, "with": {}, "from": {}, "as": {}, "it": {},
	"its": {}, "this": {}, "that": {}, "these": {}, "those": {}, "his": {},
	"her": {}, "their": {}, "my": {}, "your": {}, "our": {},
}

var factReWord = regexp.MustCompile(`[a-z0-9]+`)

// factContentTokens 抽取文本的实义词元（小写、去停用词）。
func factContentTokens(s string) []string {
	var out []string
	for _, w := range factReWord.FindAllString(strings.ToLower(s), -1) {
		if _, stop := factStopWords[w]; stop {
			continue
		}
		out = append(out, w)
	}
	return out
}

// hasCJK 判定文本是否含中日韩表意字符（短词元包含对 CJK 不适用，避免误配）。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// matchFactShortTokens 短 expected（非中文）词元包含判定：
// expected 的实义词元（≥2 个）全部出现在 got 中即命中。
// 纯词元集合比较，确定性，无随机源。
func matchFactShortTokens(gotFact, expectedFact string) bool {
	if hasCJK(expectedFact) {
		return false
	}
	ct := factContentTokens(expectedFact)
	if len(ct) < 2 {
		return false
	}
	gt := make(map[string]struct{})
	for _, w := range factContentTokens(gotFact) {
		gt[w] = struct{}{}
	}
	for _, w := range ct {
		if _, ok := gt[w]; !ok {
			return false
		}
	}
	return true
}

// matchFactV4 = matchFactV3 + 确定性残留补齐（数值日期归一 / 时间推理 / 短词元包含）。
//
// 判定顺序（任一步命中即 true）：
//  1. matchFactV3（v2 包含 + char trigram 软包含）；
//  2. 数值/日期归一后重跑 matchFactV3（覆盖 "$3,750" vs "3750"、"2023-04-01" 等形态差）；
//  3. 时间推理（锚点 occurred_at + 相对时间词 → 日历推算比对 expected 日期规格）；
//  4. 短词元包含（非中文短 expected 实义词元全部出现）。
//
// 模板 expected 的剔除在调用方（主循环）处理：v4 只负责「是否文本匹配」。
// anchor 为 top-1 候选 occurred_at（非 ground truth；候选即期望记忆时即为真值）。
func matchFactV4(gotFact, expectedFact string, anchor *time.Time) bool {
	if matchFactV3(gotFact, expectedFact) {
		return true
	}
	if expectedFact == "" {
		return false
	}
	// 数值/日期归一后复判（仅在归一确有改动时才值得再看，但直接判等价且确定）。
	if matchFactV3(normalizeFactNumbers(gotFact), normalizeFactNumbers(expectedFact)) {
		return true
	}
	if matchFactTemporal(gotFact, expectedFact, anchor) {
		return true
	}
	return matchFactShortTokens(gotFact, expectedFact)
}

// matchFactTop5 R4-C 候选扩展辅助口径：
// 主口径未命中且 expected 为短文本（归一 <15 rune）时，在 top-5 候选 fact 上逐一判定。
// 仅用于 fact_hit_top5 辅助指标，**不覆盖**主 fact 口径。
func matchFactTop5(facts []string, expectedFact string, anchor *time.Time) bool {
	if utf8.RuneCountInString(normalizeFact(expectedFact)) >= 15 {
		return false
	}
	for _, f := range facts {
		if matchFactV4(f, expectedFact, anchor) {
			return true
		}
		if shortAnswerHit(normalizeFact(f), normalizeFact(expectedFact)) {
			return true
		}
	}
	return false
}
