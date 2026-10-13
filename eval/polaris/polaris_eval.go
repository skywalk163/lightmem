// 北极星评测主程序（R5-D 任务书 §D.3.3）。
//
// 用法：
//
//	# 评测模式（M1/M2）：默认权重 R5-A 交付态、embedder auto（vector 优先）
//	go run ./eval/polaris/ -holdout eval/dataset/holdout.jsonl -report polaris_r5d
//
//	# 压缩对照模式（控制面压缩前后 token 对比）
//	go run ./eval/polaris/ -mode compress -input eval/dataset/holdout.jsonl \
//	    -report polaris_compress
//
//	# 覆盖率不降验证：压缩后 query 复跑 M1
//	go run ./eval/polaris/ -holdout eval/dataset/holdout.jsonl -compress \
//	    -report polaris_r5d_compressed
//
// holdout 语料格式（NDJSON，真实语料到位后直接接入；字段全部可选除 query）：
//
//		{"id":"h-001","query":"...","expected":"<正确事实文本>",
//		 "expected_memory_ids":["mem-..."],"bucket":"real",
//		 "tenant":"...","user":"...","as_of":"2026-10-01",
//		 "memory":{...kernel.Memory...},"hop1_memory":{...},"second_version":{...}}
//
//	  - memory / hop1_memory / second_version：逐条物化后查询（与 run_kpi.go 同链路）；
//	  - 也可用 -corpus <memories.jsonl> 批量预载语料，item 只带 query+expected；
//	  - expected 为正确事实文本（M1 判定用）；expected_memory_ids 可选（ID 口径用）。
//
// 确定性保证：-now-fixed 默认 2026-10-13（固定 rf.Now，杜绝 time.Now() 进入
// freshness 信号）；同语料 + 同权重 + 同 embedder 复跑逐位一致。
package main

import (
	"context"
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

// polarisRecallTopK 检索窗口（与 run_kpi.go kpiRecallTopK 一致：maxScan=8000
// 覆盖全库，避免「窗口抽签」）。M1 判定窗口另由 -topk 控制（默认 5）。
const polarisRecallTopK = 200

// defaultNowFixed 确定性基准时刻（R5-D 决策）：freshness 信号依赖 rf.Now，
// 用固定日期而非 time.Now()，保证复跑逐位一致（红线 5）。
const defaultNowFixed = "2026-10-13"

// HoldoutItem 北极星语料条目（{"query":...,"expected":...} 超集）。
type HoldoutItem struct {
	ID                string         `json:"id"`
	Query             string         `json:"query"`
	Expected          string         `json:"expected"`
	ExpectedFact      string         `json:"expected_fact"`
	ExpectedMemoryIDs []string       `json:"expected_memory_ids"`
	Bucket            string         `json:"bucket"`
	Tenant            string         `json:"tenant"`
	User              string         `json:"user"`
	AsOf              string         `json:"as_of,omitempty"`
	Memory            *kernel.Memory `json:"memory,omitempty"`
	Hop1Memory        *kernel.Memory `json:"hop1_memory,omitempty"`
	// Hop2Memory 干扰记忆（reference_frame 桶的第三条对照记忆）。
	// 必须物化，否则同 entity 不同 fact 的对照缺失、ref 桶覆盖率虚高（口径泄漏）。
	Hop2Memory    *kernel.Memory `json:"hop2_memory,omitempty"`
	SecondVersion *kernel.Memory `json:"second_version,omitempty"`
}

// effectiveExpected 取 expected 事实文本（兼容 expected/expected_fact 两种字段名）。
func (h HoldoutItem) effectiveExpected() string {
	if h.Expected != "" {
		return h.Expected
	}
	return h.ExpectedFact
}

// PolarisConfig 运行配置（写入报告，保证可追溯）。
type PolarisConfig struct {
	Mode        string `json:"mode"`
	Holdout     string `json:"holdout"`
	Weights     string `json:"weights"`
	Embedder    string `json:"embedder"`
	EmbedderNam string `json:"embedder_name,omitempty"`
	VocabPath   string `json:"vocab_path,omitempty"`
	TopK        int    `json:"topk"`
	Compressed  bool   `json:"compressed"`
	NowFixed    string `json:"now_fixed"`
	GeneratedAt string `json:"generated_at"`
}

// PolarisReport 北极星评测 JSON 报告。
type PolarisReport struct {
	Config PolarisConfig `json:"config"`
	// M1 零 token 覆盖率（北极星主指标，目标 ≥0.95）。
	M1 M1Report `json:"m1"`
	// M2 控制面 token 占比。
	M2 M2Report `json:"m2"`
	// Compression 压缩对照（-mode compress 时填充；eval 模式为空）。
	Compression *CompressionReport `json:"compression,omitempty"`
	// Details 逐条试验明细（定位缺口用）。
	Details []PolarisDetail `json:"details,omitempty"`
}

// PolarisDetail 单条 query 的试验明细。
type PolarisDetail struct {
	ID          string `json:"id"`
	Bucket      string `json:"bucket"`
	Query       string `json:"query"`
	Compressed  string `json:"compressed_query,omitempty"`
	Expected    string `json:"expected"`
	TopFact     string `json:"top1_fact"`
	Hit         bool   `json:"m1_hit"`
	HitAt1      bool   `json:"m1_hit_at_1"`
	FirstRank   int    `json:"first_rank,omitempty"`
	ControlToks int    `json:"control_tokens"`
	DataToks    int    `json:"data_tokens"`
}

// resolvePath 按候选顺序解析路径（兼容从仓库根 / eval/ / eval/polaris/ 运行）。
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

// loadHoldout 加载 NDJSON 语料（逐行 JSON；空行跳过）。
func loadHoldout(path string) ([]HoldoutItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []HoldoutItem
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it HoldoutItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("parse holdout line: %w", err)
		}
		items = append(items, it)
	}
	return items, nil
}

// loadMemories 加载记忆语料 JSONL（-corpus 用；每行一个 kernel.Memory）。
func loadMemories(path string) ([]kernel.Memory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mems []kernel.Memory
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m kernel.Memory
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("parse memory line: %w", err)
		}
		mems = append(mems, m)
	}
	return mems, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[polaris] ")

	mode := flag.String("mode", "eval", "运行模式 eval|compress")
	holdoutFlag := flag.String("holdout", "", "评测集 NDJSON 路径（默认依次尝试 ../dataset/holdout.jsonl、eval/dataset/holdout.jsonl、../dataset/holdout_proxy.jsonl）")
	inputFlag := flag.String("input", "", "压缩模式：查询输入 JSONL（默认同 -holdout）")
	corpusFlag := flag.String("corpus", "", "可选：预载记忆语料 JSONL（-corpus 批量落库后再逐条查询）")
	weightsFlag := flag.String("weights", "", "RankFusion 权重 JSON（默认 ../weights/rank_weights_en_r5.json，R5-A 交付态）")
	embedderFlag := flag.String("embedder", "auto", "embedder 实现 auto|vector|tfidf|none；auto=向量文件在位用 vector 否则 tfidf")
	vocabFlag := flag.String("vocab-path", "", "R3-A tfidf 词表缓存路径（如 ../weights/embedder_vocab_en.json）；空则由 holdout 语料现构建")
	reportFlag := flag.String("report", "polaris_r5d", "报告文件名前缀")
	topK := flag.Int("topk", 5, "M1 判定候选窗口（top-K fact 内命中即算零 token 拿到事实）")
	compressFlag := flag.Bool("compress", false, "控制面压缩口径：query 先过 CompressQuery 再检索（覆盖率不降验证用）")
	nowFixed := flag.String("now-fixed", defaultNowFixed, "固定 rf.Now 基准日期（YYYY-MM-DD；确定性保证，勿用 time.Now）")
	reportsDir := flag.String("reports-dir", "", "报告输出目录（默认自动探测 eval/reports）")

	// R6-D 评测有效性开关：-validity 跑全套；-no-anchor/-ablate/-multiview 单独跑。
	validityFlag := flag.Bool("validity", false, "R6-D：跑全套评测有效性（无锚变体 + 消融 + 多视图）")
	noAnchorFlag := flag.Bool("no-anchor", false, "R6-D：仅跑无锚变体（剥离括号锚，量化锚红利）")
	ablateFlag := flag.String("ablate", "", "R6-D：消融模式，关闭指定信号（semantic|keyword|entity|value|freshness|source_trust|reference_frame|feedback|all）")
	multiviewFlag := flag.Bool("multiview", false, "R6-D：仅跑多视图集成（确定性模板视图 + 候选得票集成）")
	flag.Parse()

	_ = os.MkdirAll(resolveReportsDir(*reportsDir), 0o755)

	// ---- 语料加载（所有模式共用） ----
	holdoutPath := *holdoutFlag
	if holdoutPath == "" {
		holdoutPath = resolvePath(
			"../dataset/holdout.jsonl",
			"eval/dataset/holdout.jsonl",
			"dataset/holdout.jsonl",
			"../dataset/holdout_proxy.jsonl",
			"eval/dataset/holdout_proxy.jsonl",
			"dataset/holdout_proxy.jsonl",
		)
	}
	if holdoutPath == "" {
		log.Fatalf("未找到 holdout 语料（-holdout 指定路径；真实语料未到位时先用 gen_holdout_proxy.go 生成代理语料）")
	}
	items, err := loadHoldout(holdoutPath)
	if err != nil {
		log.Fatalf("load holdout %s: %v", holdoutPath, err)
	}
	log.Printf("loaded %d holdout items from %s", len(items), holdoutPath)

	// R6-D 评测有效性模式（优先级高于 -mode eval）
	if *validityFlag || *noAnchorFlag || *ablateFlag != "" || *multiviewFlag {
		runValidityMode(items, holdoutPath, *corpusFlag, *weightsFlag, *embedderFlag,
			*reportFlag, resolveReportsDir(*reportsDir), *topK, *compressFlag, *nowFixed, *vocabFlag,
			*noAnchorFlag, *ablateFlag, *multiviewFlag)
		return
	}

	switch *mode {
	case "compress":
		compressInput := holdoutPath
		if *inputFlag != "" {
			compressInput = *inputFlag
			reloaded, rerr := loadHoldout(compressInput)
			if rerr != nil {
				log.Fatalf("load compress input %s: %v", compressInput, rerr)
			}
			items = reloaded
			log.Printf("loaded %d queries from %s", len(items), compressInput)
		}
		runCompressMode(items, *reportFlag, resolveReportsDir(*reportsDir))
		return
	case "eval":
		// 继续。
	default:
		log.Fatalf("-mode %q invalid: must be eval|compress", *mode)
	}

	runEvalMode(items, holdoutPath, *corpusFlag, *weightsFlag, *embedderFlag,
		*reportFlag, resolveReportsDir(*reportsDir), *topK, *compressFlag, *nowFixed, *vocabFlag)
}

// resolveReportsDir 解析报告输出目录。
func resolveReportsDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if d := resolvePath("../reports", "eval/reports", "reports"); d != "" {
		return d
	}
	return "reports"
}

// ------------------------------------------------------------------
// 压缩模式：控制面压缩前后 token 对比（不检索，纯文本口径）。
// ------------------------------------------------------------------

func runCompressMode(items []HoldoutItem, prefix, dir string) {
	queries := make([]string, 0, len(items))
	for _, it := range items {
		if it.Query != "" {
			queries = append(queries, it.Query)
		}
	}
	rep := CompressControlPlane(queries)
	out := map[string]any{
		"mode":             "compress",
		"queries":          rep.Queries,
		"control_before":   rep.ControlBefore,
		"control_after":    rep.ControlAfter,
		"saved":            rep.Saved,
		"compression_rate": rep.CompressionRate,
		"items":            rep.Items,
	}
	writeJSONReport(dir, prefix, out)
	log.Printf("[compress] queries=%d control_tokens %d -> %d (saved=%d, rate=%.2f%%)",
		rep.Queries, rep.ControlBefore, rep.ControlAfter, rep.Saved, rep.CompressionRate*100)
	log.Printf("[compress] 达标口径：压缩率 ≥20%%（任务书 §D.5）")
}

// ------------------------------------------------------------------
// 评测模式：装配 kernel → 逐条检索 → M1/M2 统计 → JSON 报告。
// ------------------------------------------------------------------

func runEvalMode(items []HoldoutItem, holdoutPath, corpusPath, weightsPath, embedderMode,
	prefix, dir string, topK int, compressed bool, nowFixed, vocabPath string) {
	ctx := context.Background()

	// ---- 权重解析（默认 R5-A 交付态 rank_weights_en_r5.json） ----
	if weightsPath == "" {
		weightsPath = resolvePath(
			"../weights/rank_weights_en_r5.json",
			"eval/weights/rank_weights_en_r5.json",
			"weights/rank_weights_en_r5.json",
		)
	}
	if weightsPath == "" {
		log.Fatalf("未找到权重文件（-weights 指定）")
	}

	// ---- 装配内核（SQLite 内存后端；与 run_kpi.go 同链路） ----
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	rf := kernel.DefaultRankFusion()
	baseNow := time.Now().UTC()
	if nowFixed != "" {
		t, perr := time.Parse("2006-01-02", nowFixed)
		if perr != nil {
			log.Fatalf("-now-fixed %q: %v", nowFixed, perr)
		}
		baseNow = t.UTC()
	}
	rf.Now = baseNow
	if err := rf.LoadWeightsFromFile(weightsPath); err != nil {
		log.Fatalf("load weights %s: %v", weightsPath, err)
	}
	log.Printf("weights=%s baseNow=%s (fixed, 确定性)", weightsPath, baseNow.Format(time.RFC3339))

	// ---- embedder 注入 ----
	embedderName := ""
	switch embedderMode {
	case "none":
		log.Printf("[polaris] embedder mode=none (semantic falls back to RecallScore)")
	case "vector":
		ve, verr := loadPolarisVectorEmbedder()
		if verr != nil {
			log.Fatalf("[polaris] load vector embedder: %v\n  -> 回退建议：-embedder tfidf", verr)
		}
		rf.SetEmbedderImpl(ve)
		embedderName = ve.Name()
		log.Printf("[polaris] embedder injected: name=%s dim=%d count=%d", ve.Name(), ve.Dim(), ve.Count())
	case "tfidf":
		v, verr := loadPolarisVocab(vocabPath, items)
		if verr != nil {
			log.Fatalf("[polaris] load/build tfidf vocab: %v", verr)
		}
		rf.SetEmbedder(v.EmbedFn())
		embedderName = v.Name()
		log.Printf("[polaris] embedder injected: ngram=%d dim=%d docs=%d", v.Ngram, v.Dim(), v.N)
	case "auto":
		if ve, verr := loadPolarisVectorEmbedder(); verr == nil {
			rf.SetEmbedderImpl(ve)
			embedderName = ve.Name()
			log.Printf("[polaris] embedder auto->vector: name=%s dim=%d count=%d", ve.Name(), ve.Dim(), ve.Count())
		} else {
			v, verr2 := loadPolarisVocab(vocabPath, items)
			if verr2 != nil {
				log.Fatalf("[polaris] auto 模式向量不可用且 tfidf 构建失败: vector=%v tfidf=%v", verr, verr2)
			}
			rf.SetEmbedder(v.EmbedFn())
			embedderName = v.Name()
			log.Printf("[polaris] embedder auto->tfidf (vector unavailable: %v)", verr)
		}
	default:
		log.Fatalf("-embedder %q invalid: must be auto|vector|tfidf|none", embedderMode)
	}
	srv.SetRankFusion(rf)

	// ---- multi-hop 重排（与 run_kpi.go R4-A 后默认一致：开启、深度 1） ----
	mhCfg := kernel.DefaultMultiHopConfig()
	mhCfg.Enabled = true
	mhCfg.Depth = 1
	srv.SetMultiHopConfig(mhCfg)
	log.Printf("[polaris] multi-hop rerank: enabled=%v depth=%d", mhCfg.Enabled, mhCfg.Depth)

	// ---- 可选：批量预载语料 ----
	if corpusPath != "" {
		mems, cerr := loadMemories(corpusPath)
		if cerr != nil {
			log.Fatalf("load corpus %s: %v", corpusPath, cerr)
		}
		actor := "polaris-corpus"
		saved := 0
		for i := range mems {
			if _, serr := srv.SaveMemory(ctx, &mems[i], actor); serr != nil {
				log.Printf("WARN: save corpus memory %s: %v", mems[i].MemoryID, serr)
				continue
			}
			saved++
		}
		log.Printf("[polaris] corpus preloaded: %d/%d from %s", saved, len(mems), corpusPath)
	}

	// ---- 逐条检索 + M1/M2 ----
	m1 := NewM1Accumulator(topK)
	m2 := NewM2Accumulator(compressed)
	var details []PolarisDetail
	skipped := 0
	for idx, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			skipped++
			continue
		}
		tenant, usr := it.Tenant, it.User
		if tenant == "" {
			tenant = "eval-tenant"
		}
		if usr == "" {
			usr = "eval-user"
		}
		// 物化记忆（与 run_kpi.go 同链路：item 自带 memory/hop1/second）。
		if it.Memory != nil {
			if _, serr := srv.SaveMemory(ctx, it.Memory, usr); serr != nil {
				log.Printf("WARN: save memory %s: %v", it.Memory.MemoryID, serr)
			}
		}
		if it.Hop1Memory != nil {
			if _, serr := srv.SaveMemory(ctx, it.Hop1Memory, usr); serr != nil {
				log.Printf("WARN: save hop1 %s: %v", it.Hop1Memory.MemoryID, serr)
			}
		}
		if it.Hop2Memory != nil {
			if _, serr := srv.SaveMemory(ctx, it.Hop2Memory, usr); serr != nil {
				log.Printf("WARN: save hop2 %s: %v", it.Hop2Memory.MemoryID, serr)
			}
		}
		if it.SecondVersion != nil {
			if _, serr := srv.SaveMemory(ctx, it.SecondVersion, usr); serr != nil {
				log.Printf("WARN: save second version %s: %v", it.SecondVersion.MemoryID, serr)
			}
		}
		// 基准时刻：固定值（-now-fixed）；本评测不做 as_of 覆盖（北极星口径
		// 与 run_kpi.go 默认 -now-asof=false 一致，时序行为由 temporal 桶
		// 的 second_version 物化 + as_of 字段在语料侧表达）。
		rf.Now = baseNow
		query := it.Query
		compressedQuery := ""
		if compressed {
			compressedQuery = CompressQuery(query)
			query = compressedQuery
		}
		rc := kernel.RouterContext{
			Text:     query,
			TenantID: tenant,
			UserID:   usr,
			TopK:     polarisRecallTopK,
		}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			log.Printf("WARN: recall %s (idx=%d): %v", it.ID, idx, rerr)
			continue
		}
		// top-K 候选（M1 判定窗口）。
		var topFacts, topIDs []string
		for i := range res.Fused {
			if i >= topK {
				break
			}
			topFacts = append(topFacts, res.Fused[i].Fact)
			topIDs = append(topIDs, res.Fused[i].MemoryID)
		}
		var anchor *time.Time
		if len(res.Fused) > 0 && res.Fused[0].Memory != nil {
			anchor = res.Fused[0].Memory.OccurredAt
		}
		expected := it.effectiveExpected()
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, topK, anchor)
		trial.Query = it.Query
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)

		// M2 账目：控制面（query+指令，压缩口径由 -compress 决定）；
		// 数据面 = top-K 候选记忆正文。
		acct := TokenAccount{
			Query:         it.Query,
			ControlTokens: ControlPlaneTokens(it.Query, compressed),
		}
		for _, f := range topFacts {
			acct.DataTokens += EstimateTokens(f)
		}
		acct.TotalTokens = acct.ControlTokens + acct.DataTokens
		m2.AddAccount(it.Bucket, acct)

		// 明细全量入报告（未命中的是定位重点；报告 JSON 偏大属预期）。
		d := PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query,
			Compressed: compressedQuery, Expected: expected,
			Hit: trial.Hit, HitAt1: trial.HitAt1, FirstRank: trial.FirstRank,
			ControlToks: acct.ControlTokens, DataToks: acct.DataTokens,
		}
		if len(res.Fused) > 0 {
			d.TopFact = res.Fused[0].Fact
		}
		details = append(details, d)
	}

	rep := PolarisReport{
		Config: PolarisConfig{
			Mode: "eval", Holdout: holdoutPath, Weights: weightsPath,
			Embedder: embedderMode, EmbedderNam: embedderName, VocabPath: vocabPath,
			TopK: topK, Compressed: compressed, NowFixed: nowFixed,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		},
		M1:      m1.Report(),
		M2:      m2.Report(),
		Details: details,
	}
	writeJSONReport(dir, prefix, rep)
	m1r, m2r := rep.M1, rep.M2
	log.Printf("[M1] n=%d hits=%d coverage=%.4f (hit@1=%.4f, id_hit=%d/%d, mean_rr=%.4f)",
		m1r.N, m1r.Hits, m1r.Coverage, m1r.HitAt1Rate, m1r.IDHit, m1r.IDTotal, m1r.MeanRR)
	log.Printf("[M2] queries=%d control=%d data=%d total=%d m2=%.4f (per-query mean=%.4f)",
		m2r.Queries, m2r.ControlTokens, m2r.DataTokens, m2r.TotalTokens, m2r.M2, m2r.MeanPerQueryM2)
	log.Printf("[polaris] skipped=%d (query/expected 为空)", skipped)
	log.Printf("[polaris] 北极星判定：M1 零 token 覆盖率 ≥0.95 %s", passFail(m1r.Coverage >= 0.95))
	for bucket, bb := range m1r.ByBucket {
		log.Printf("  bucket %-16s n=%-4d coverage=%.4f hit@1=%d", bucket, bb.N, bb.Coverage, bb.HitAt1)
	}
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// writeJSONReport 写 JSON 报告（<dir>/<prefix>_<yyyymmdd-hhmmss>.json）。
func writeJSONReport(dir, prefix string, v any) {
	ts := time.Now().UTC().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.json", prefix, ts))
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		log.Fatalf("write report %s: %v", path, err)
	}
	log.Printf("[polaris] report written: %s", path)
}

// ------------------------------------------------------------------
// embedder 加载（polaris 自有路径解析；不自动触发 BGE 生成——
// 27 分钟的向量生成由 R5-A 的 run_kpi.go 链路负责，polaris 只消费）。
// ------------------------------------------------------------------

// loadPolarisVectorEmbedder 加载 BGE-m3 静态向量（R5-A 产物 eval/vectors/）。
func loadPolarisVectorEmbedder() (*kernel.VectorEmbedder, error) {
	matrix := resolvePath("../vectors/vectors.npy", "eval/vectors/vectors.npy", "vectors/vectors.npy")
	meta := resolvePath("../vectors/vectors_meta.json", "eval/vectors/vectors_meta.json", "vectors/vectors_meta.json")
	index := resolvePath("../vectors/vectors_index.jsonl", "eval/vectors/vectors_index.jsonl", "vectors/vectors_index.jsonl")
	if matrix == "" || meta == "" {
		return nil, fmt.Errorf("向量文件不在位（matrix=%q meta=%q）；R5-A 产物应在 eval/vectors/，或回退 -embedder tfidf", matrix, meta)
	}
	cfg := kernel.VectorEmbedderConfig{MatrixPath: matrix, MetaPath: meta}
	if index != "" {
		cfg.IndexPath = index
	}
	return kernel.NewVectorEmbedder(cfg)
}

// loadPolarisVocab 加载 tfidf 词表：优先 -vocab-path 指定缓存（与 run_kpi.go
// R3-A 的 embedder_vocab_en.json 同源，保证 isolate 忠实）；否则由 holdout
// 语料现构建（确定性：固定 n=3、固定语料顺序）。
func loadPolarisVocab(vocabPath string, items []HoldoutItem) (*kernel.CharNgramVocab, error) {
	if vocabPath != "" {
		if v, err := kernel.LoadCharNgramVocabJSON(vocabPath); err == nil {
			log.Printf("[polaris] tfidf vocab loaded from cache: %s (dim=%d docs=%d)", vocabPath, v.Dim(), v.N)
			return v, nil
		} else {
			log.Printf("[polaris] tfidf vocab cache %s 加载失败，回退现构建: %v", vocabPath, err)
		}
	}
	return buildPolarisVocab(items)
}

// buildPolarisVocab 从 holdout 语料构建 char n-gram TF-IDF 词表（确定性：
// 固定 n=3、固定语料顺序——item 文件序 + query/fact 序）。
func buildPolarisVocab(items []HoldoutItem) (*kernel.CharNgramVocab, error) {
	var corpus []string
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
		if it.SecondVersion != nil && it.SecondVersion.Fact != "" {
			corpus = append(corpus, it.SecondVersion.Fact)
		}
	}
	if len(corpus) == 0 {
		return nil, fmt.Errorf("empty corpus")
	}
	v := kernel.NewCharNgramVocab(3)
	v.BuildIDF(corpus)
	return v, nil
}

// ------------------------------------------------------------------
// R6-D 评测有效性：无锚变体 + 消融 + 多视图集成。
// ------------------------------------------------------------------

// runValidityMode R6-D 评测有效性主入口。
// 优先级：-validity 全套；-no-anchor 仅无锚；-ablate=<sig> 仅消融；-multiview 仅多视图。
func runValidityMode(items []HoldoutItem, holdoutPath, corpusPath, weightsPath, embedderMode,
	prefix, dir string, topK int, compressed bool, nowFixed, vocabPath string,
	noAnchor bool, ablate string, multiview bool) {
	ctx := context.Background()

	// ---- 装配内核（与 runEvalMode 同链路） ----
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	rf := kernel.DefaultRankFusion()
	baseNow := time.Now().UTC()
	if nowFixed != "" {
		t, perr := time.Parse("2006-01-02", nowFixed)
		if perr != nil {
			log.Fatalf("-now-fixed %q: %v", nowFixed, perr)
		}
		baseNow = t.UTC()
	}
	rf.Now = baseNow
	if err := rf.LoadWeightsFromFile(weightsPath); err != nil {
		log.Fatalf("load weights %s: %v", weightsPath, err)
	}
	log.Printf("weights=%s baseNow=%s (fixed, 确定性)", weightsPath, baseNow.Format(time.RFC3339))

	// ---- embedder 注入 ----
	embedderName := ""
	switch embedderMode {
	case "none":
		log.Printf("[polaris] embedder mode=none (semantic falls back to RecallScore)")
	case "vector":
		ve, verr := loadPolarisVectorEmbedder()
		if verr != nil {
			log.Fatalf("[polaris] load vector embedder: %v\n  -> 回退建议：-embedder tfidf", verr)
		}
		rf.SetEmbedderImpl(ve)
		embedderName = ve.Name()
		log.Printf("[polaris] embedder injected: name=%s dim=%d count=%d", ve.Name(), ve.Dim(), ve.Count())
	case "tfidf":
		v, verr := loadPolarisVocab(vocabPath, items)
		if verr != nil {
			log.Fatalf("[polaris] load/build tfidf vocab: %v", verr)
		}
		rf.SetEmbedder(v.EmbedFn())
		embedderName = v.Name()
		log.Printf("[polaris] embedder injected: ngram=%d dim=%d docs=%d", v.Ngram, v.Dim(), v.N)
	case "auto":
		if ve, verr := loadPolarisVectorEmbedder(); verr == nil {
			rf.SetEmbedderImpl(ve)
			embedderName = ve.Name()
			log.Printf("[polaris] embedder auto->vector: name=%s dim=%d count=%d", ve.Name(), ve.Dim(), ve.Count())
		} else {
			v, verr2 := loadPolarisVocab(vocabPath, items)
			if verr2 != nil {
				log.Fatalf("[polaris] auto 模式向量不可用且 tfidf 构建失败: vector=%v tfidf=%v", verr, verr2)
			}
			rf.SetEmbedder(v.EmbedFn())
			embedderName = v.Name()
			log.Printf("[polaris] embedder auto->tfidf (vector unavailable: %v)", verr)
		}
	default:
		log.Fatalf("-embedder %q invalid: must be auto|vector|tfidf|none", embedderMode)
	}
	srv.SetRankFusion(rf)

	// ---- multi-hop 重排（与 run_kpi.go R4-A 后默认一致：开启、深度 1） ----
	mhCfg := kernel.DefaultMultiHopConfig()
	mhCfg.Enabled = true
	mhCfg.Depth = 1
	srv.SetMultiHopConfig(mhCfg)
	log.Printf("[polaris] multi-hop rerank: enabled=%v depth=%d", mhCfg.Enabled, mhCfg.Depth)

	// ---- 可选：批量预载语料 ----
	if corpusPath != "" {
		mems, cerr := loadMemories(corpusPath)
		if cerr != nil {
			log.Fatalf("load corpus %s: %v", corpusPath, cerr)
		}
		actor := "polaris-corpus"
		saved := 0
		for i := range mems {
			if _, serr := srv.SaveMemory(ctx, &mems[i], actor); serr != nil {
				log.Printf("WARN: save corpus memory %s: %v", mems[i].MemoryID, serr)
				continue
			}
			saved++
		}
		log.Printf("[polaris] corpus preloaded: %d/%d from %s", saved, len(mems), corpusPath)
	}

	// ---- 分发到各子模块 ----
	if noAnchor {
		runNoAnchor(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix)
		return
	}
	if ablate != "" {
		runAblation(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix, ablate)
		return
	}
	if multiview {
		runMultiview(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix)
		return
	}
	// -validity：全套
	runNoAnchor(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix)
	runAblation(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix, "all")
	runMultiview(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix)
	log.Printf("[validity] R6-D 评测有效性全套完成：无锚 + 消融 + 多视图")
}
