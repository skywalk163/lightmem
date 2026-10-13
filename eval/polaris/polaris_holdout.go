//go:build ignore

// polaris_holdout.go — R6-C 北极星真实语料正式判定 runner（独立主程序）。
//
// 为什么独立：polaris_eval.go 归 D 路独占（R6 任务书 §5.2），C 路用本文件
// 绕开争用。本文件带 //go:build ignore，不进入 go build ./... / go test ./...
// 编译；运行时需与库文件一起列出：
//
//	go run eval/polaris/polaris_holdout.go \
//	  eval/polaris/polaris_fact.go eval/polaris/polaris_m1.go \
//	  eval/polaris/polaris_m2.go eval/polaris/polaris_token.go \
//	  eval/polaris/compress_light.go eval/polaris/polaris_fp.go
//
// 三项职责（C.2）：
//  1. 真实语料 holdout.jsonl 的 M1/M2 正式判定；语料未到位时代理语料预跑，
//     报告显著标注「非正式判定」（评测诚实性优先）；
//  2. FP 审计层（polaris_fp.go）：对 v4 已判 hit 做否定一致性 + 碎片化复核，
//     产出 C 独立口径 FP 清单；
//  3. 数据面压缩对照（compress_light.go）：候选 fact 展示/计费面副本压缩前后
//     token 对照；判定输入保持原文（双口径），M1 不降。
//
// 确定性：权重 R5 交付态、embedder auto（vector 优先）、now-fixed 固定基准日、
// 固定种子语义——同输入逐位一致。
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

const holdoutRecallTopK = 200

// HoldoutItem 与 polaris_eval.go 同构（独立 runner 自带，避免引入 D 路文件）。
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
	Hop2Memory        *kernel.Memory `json:"hop2_memory,omitempty"`
	SecondVersion     *kernel.Memory `json:"second_version,omitempty"`
}

func (h HoldoutItem) effectiveExpected() string {
	if h.Expected != "" {
		return h.Expected
	}
	return h.ExpectedFact
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[holdout] ")

	holdoutFlag := flag.String("holdout", "", "评测集 NDJSON（默认自动探测 eval/dataset/holdout.jsonl，缺席则 holdout_proxy.jsonl）")
	corpusFlag := flag.String("corpus", "", "可选：预载记忆语料 JSONL")
	weightsFlag := flag.String("weights", "", "RankFusion 权重 JSON（默认 eval/weights/rank_weights_en_r5.json）")
	embedderFlag := flag.String("embedder", "auto", "auto|vector|tfidf|none")
	vocabFlag := flag.String("vocab-path", "", "tfidf 词表缓存")
	reportFlag := flag.String("report", "r6c_holdout", "报告文件名前缀")
	topK := flag.Int("topk", 5, "M1 判定候选窗口")
	nowFixed := flag.String("now-fixed", "2026-10-13", "固定 rf.Now 基准日期")
	reportsDir := flag.String("reports-dir", "", "报告输出目录（默认自动探测 eval/reports）")
	flag.Parse()

	_ = os.MkdirAll(resolveReportsDir(*reportsDir), 0o755)

	// ---- 语料定位：真实 holdout 优先，缺席回落代理（显著标注非正式） ----
	holdoutPath := *holdoutFlag
	official := true
	note := "真实语料 holdout.jsonl（正式判定）"
	if holdoutPath == "" {
		holdoutPath = resolvePath(
			"../dataset/holdout.jsonl", "eval/dataset/holdout.jsonl", "dataset/holdout.jsonl",
		)
		if holdoutPath == "" {
			holdoutPath = resolvePath(
				"../dataset/holdout_proxy.jsonl", "eval/dataset/holdout_proxy.jsonl", "dataset/holdout_proxy.jsonl",
			)
			official = false
			note = "⚠️ 非正式判定：真实语料未到位，使用代理语料 holdout_proxy.jsonl 预跑（数字仅作走势参考，不作为北极星达标依据）"
		}
	}
	if holdoutPath == "" {
		log.Fatalf("未找到评测语料（-holdout 指定路径）")
	}
	items, err := loadHoldout(holdoutPath)
	if err != nil {
		log.Fatalf("load %s: %v", holdoutPath, err)
	}
	log.Printf("loaded %d items from %s", len(items), holdoutPath)
	if !official {
		log.Printf("==============================================")
		log.Printf("⚠️  非正式判定：代理语料预跑，不作为北极星达标依据")
		log.Printf("==============================================")
	}

	// ---- 装配内核（与 run_kpi.go / polaris_eval.go 同链路） ----
	ctx := context.Background()
	if *weightsFlag == "" {
		*weightsFlag = resolvePath(
			"../weights/rank_weights_en_r5.json", "eval/weights/rank_weights_en_r5.json", "weights/rank_weights_en_r5.json",
		)
	}
	if *weightsFlag == "" {
		log.Fatalf("未找到权重文件（-weights 指定）")
	}
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	rf := kernel.DefaultRankFusion()
	baseNow := time.Now().UTC()
	if *nowFixed != "" {
		t, perr := time.Parse("2006-01-02", *nowFixed)
		if perr != nil {
			log.Fatalf("-now-fixed %q: %v", *nowFixed, perr)
		}
		baseNow = t.UTC()
	}
	rf.Now = baseNow
	if err := rf.LoadWeightsFromFile(*weightsFlag); err != nil {
		log.Fatalf("load weights %s: %v", *weightsFlag, err)
	}
	log.Printf("weights=%s baseNow=%s", *weightsFlag, baseNow.Format(time.RFC3339))

	embedderName := ""
	switch *embedderFlag {
	case "vector":
		ve, verr := loadHoldoutVectorEmbedder()
		if verr != nil {
			log.Fatalf("load vector embedder: %v", verr)
		}
		rf.SetEmbedderImpl(ve)
		embedderName = ve.Name()
	case "tfidf":
		v, verr := loadHoldoutVocab(*vocabFlag, items)
		if verr != nil {
			log.Fatalf("build tfidf vocab: %v", verr)
		}
		rf.SetEmbedder(v.EmbedFn())
		embedderName = v.Name()
	case "auto":
		if ve, verr := loadHoldoutVectorEmbedder(); verr == nil {
			rf.SetEmbedderImpl(ve)
			embedderName = ve.Name()
			log.Printf("embedder auto->vector: %s", embedderName)
		} else {
			v, verr2 := loadHoldoutVocab(*vocabFlag, items)
			if verr2 != nil {
				log.Fatalf("auto: vector=%v tfidf=%v", verr, verr2)
			}
			rf.SetEmbedder(v.EmbedFn())
			embedderName = v.Name()
			log.Printf("embedder auto->tfidf: %s", embedderName)
		}
	case "none":
	default:
		log.Fatalf("-embedder %q invalid", *embedderFlag)
	}
	srv.SetRankFusion(rf)

	mhCfg := kernel.DefaultMultiHopConfig()
	mhCfg.Enabled = true
	mhCfg.Depth = 1
	srv.SetMultiHopConfig(mhCfg)

	if *corpusFlag != "" {
		mems, cerr := loadMemories(*corpusFlag)
		if cerr != nil {
			log.Fatalf("load corpus: %v", cerr)
		}
		saved := 0
		for i := range mems {
			if _, serr := srv.SaveMemory(ctx, &mems[i], "holdout-corpus"); serr != nil {
				continue
			}
			saved++
		}
		log.Printf("corpus preloaded: %d/%d", saved, len(mems))
	}

	// ---- 逐条检索 + M1/M2 + FP 审计 + 数据面对照 ----
	m1 := NewM1Accumulator(*topK)
	m2 := NewM2Accumulator(false)
	var fpRows []FPAuditRow
	var dataFactsAll []string
	var details []HoldoutDetail
	for idx := range items {
		it := items[idx]
		if it.Query == "" || it.effectiveExpected() == "" {
			continue
		}
		tenant, usr := it.Tenant, it.User
		if tenant == "" {
			tenant = "eval-tenant"
		}
		if usr == "" {
			usr = "eval-user"
		}
		for _, m := range []*kernel.Memory{it.Memory, it.Hop1Memory, it.Hop2Memory, it.SecondVersion} {
			if m != nil {
				if _, serr := srv.SaveMemory(ctx, m, usr); serr != nil {
					log.Printf("WARN save %s: %v", m.MemoryID, serr)
				}
			}
		}
		rf.Now = baseNow
		rc := kernel.RouterContext{Text: it.Query, TenantID: tenant, UserID: usr, TopK: holdoutRecallTopK}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			log.Printf("WARN recall %s: %v", it.ID, rerr)
			continue
		}
		var topFacts, topIDs []string
		for i := range res.Fused {
			if i >= *topK {
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
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, *topK, anchor)
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)

		// FP 审计层：仅对 v4 已判 hit 的条目做语义一致性复核。
		// got 必须取**真正通过 v4 的那条候选 fact**（而非 ID 命中 rank 的 fact——
		// 二者可能不是同一条；ID 命中但 fact 未匹配时审计对象为空，不参与复核）。
		if trial.Hit {
			for i, f := range topFacts {
				if i >= *topK {
					break
				}
				if !matchFactV4(f, expected, anchor) {
					continue
				}
				vetoed, rule := FPVeto(expected, f)
				fpRows = append(fpRows, FPAuditRow{ID: it.ID, Expected: expected, Got: f, Vetoed: vetoed, Rule: rule})
				break
			}
		}

		// 数据面：判定用原文 topFacts；token 账目同时统计压缩副本。
		dataFactsAll = append(dataFactsAll, topFacts...)
		acct := TokenAccount{Query: it.Query, ControlTokens: ControlPlaneTokens(it.Query, false)}
		for _, f := range topFacts {
			acct.DataTokens += EstimateTokens(CompressDataFact(f))
		}
		acct.TotalTokens = acct.ControlTokens + acct.DataTokens
		m2.AddAccount(it.Bucket, acct)

		details = append(details, HoldoutDetail{
			ID: it.ID, Bucket: it.Bucket, Expected: expected, Hit: trial.Hit,
			FirstRank: trial.FirstRank,
		})
	}

	// ---- 汇总 ----
	fpRep := FPAuditReport{V4Hits: len(fpRows), Rows: fpRows}
	for _, r := range fpRows {
		if r.Vetoed {
			fpRep.Vetoed++
		}
	}
	fpRep.ResidualFP = fpRep.V4Hits - fpRep.Vetoed // 审计口径剩余「可疑未否决」命中数
	// 真正的残留 FP = 被规则否决的语义矛盾条数（已剔出审计口径）。
	dataAcct := CompressDataPlaneAccount(dataFactsAll)

	rep := map[string]any{
		"mode":                   "holdout-official",
		"official_corpus":        official,
		"corpus_note":            note,
		"config":                 map[string]any{"holdout": holdoutPath, "weights": *weightsFlag, "embedder": *embedderFlag, "embedder_name": embedderName, "topk": *topK, "now_fixed": *nowFixed},
		"m1":                     m1.Report(),
		"m2":                     m2.Report(),
		"data_plane_compression": dataAcct,
		"fp_audit":               fpRep,
		"details":                details,
	}
	writeHoldoutJSON(*reportsDir, *reportFlag, rep)
	m1r, m2r := m1.Report(), m2.Report()
	log.Printf("[M1] n=%d hits=%d coverage=%.4f hit@1=%.4f", m1r.N, m1r.Hits, m1r.Coverage, m1r.HitAt1Rate)
	log.Printf("[M2] control=%d data=%d m2=%.4f", m2r.ControlTokens, m2r.DataTokens, m2r.M2)
	log.Printf("[data-plane] tokens %d -> %d (saved=%d, reduction=%.2f%%)", dataAcct.TokensBefore, dataAcct.TokensAfter, dataAcct.Saved, dataAcct.Reduction*100)
	log.Printf("[fp-audit] v4_hits=%d vetoed=%d", fpRep.V4Hits, fpRep.Vetoed)
	if !official {
		log.Printf("⚠️  非正式判定（代理语料）：以上数字不作为北极星达标依据")
	}
}

// HoldoutDetail 单条试验明细。
type HoldoutDetail struct {
	ID        string `json:"id"`
	Bucket    string `json:"bucket"`
	Expected  string `json:"expected"`
	Hit       bool   `json:"m1_hit"`
	FirstRank int    `json:"first_rank,omitempty"`
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
			return nil, fmt.Errorf("parse line: %w", err)
		}
		items = append(items, it)
	}
	return items, nil
}

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

func resolveReportsDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if d := resolvePath("../reports", "eval/reports", "reports"); d != "" {
		return d
	}
	return "reports"
}

func writeHoldoutJSON(dir, prefix string, v any) {
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

// loadHoldoutVectorEmbedder 加载 BGE-m3 静态向量（与 polaris_eval.go 同路径）。
func loadHoldoutVectorEmbedder() (*kernel.VectorEmbedder, error) {
	matrix := resolvePath("../vectors/vectors.npy", "eval/vectors/vectors.npy", "vectors/vectors.npy")
	meta := resolvePath("../vectors/vectors_meta.json", "eval/vectors/vectors_meta.json", "vectors/vectors_meta.json")
	index := resolvePath("../vectors/vectors_index.jsonl", "eval/vectors/vectors_index.jsonl", "vectors/vectors_index.jsonl")
	if matrix == "" || meta == "" {
		return nil, fmt.Errorf("向量文件不在位")
	}
	cfg := kernel.VectorEmbedderConfig{MatrixPath: matrix, MetaPath: meta}
	if index != "" {
		cfg.IndexPath = index
	}
	return kernel.NewVectorEmbedder(cfg)
}

// loadHoldoutVocab 加载/构建 tfidf 词表。
func loadHoldoutVocab(vocabPath string, items []HoldoutItem) (*kernel.CharNgramVocab, error) {
	if vocabPath != "" {
		if v, err := kernel.LoadCharNgramVocabJSON(vocabPath); err == nil {
			return v, nil
		}
	}
	var corpus []string
	for _, it := range items {
		if it.Query != "" {
			corpus = append(corpus, it.Query)
		}
		if it.Memory != nil && it.Memory.Fact != "" {
			corpus = append(corpus, it.Memory.Fact)
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
