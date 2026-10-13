//go:build ignore

// eval/polaris/polaris_voting.go — R6-A 投票共识 + 参考系激活 runner（独立主程序）。
//
// 为什么独立：polaris_eval.go 归 D 路独占（R6 任务书 §5.2），A 路用本文件
// 绕开争用。本文件带 //go:build ignore，不进入 go build ./... / go test ./...
// 编译；运行时需与库文件一起列出：
//
//	go run eval/polaris/polaris_voting.go \
//	  eval/polaris/polaris_fact.go eval/polaris/polaris_m1.go \
//	  eval/polaris/polaris_m2.go eval/polaris/polaris_token.go
//
// 三项职责（A.2.3）：
//  1. ref 桶 + 全桶 M1 覆盖率的投票口径评测（-fusion vote|rrf，默认 rrf）；
//  2. 参考系激活：从 query 确定性提取参考系坐标（正则，不调 LLM），赋给
//     rf.QueryFrame，使 signalReferenceFrame 从「恒 0.5 中性」转为真实匹配分
//     （P3 深层根因：北极星链路 rc 只设 tenant/user，frame() 坐标全空 → ref
//     信号是死的）；
//  3. 分歧度-错误相关性：逐 query 记录 VoteDivergence 与 top-1 是否命中的
//     成对样本，分箱/相关（Spearman）量化「分歧大→易错」。
//
// 确定性：权重 R5 交付态、embedder auto（vector 优先）、now-fixed 固定基准日、
// query 参考系提取为固定正则、投票平局按 signal 名字典序——同输入逐位一致。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

const votingRecallTopK = 200

// VotingItem 与 polaris_eval.go 同构（独立 runner 自带）。
type VotingItem struct {
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

func (h VotingItem) effectiveExpected() string {
	if h.Expected != "" {
		return h.Expected
	}
	return h.ExpectedFact
}

// VotingConfig 运行配置（写入报告，保证可追溯）。
type VotingConfig struct {
	Mode         string `json:"mode"`
	Fusion       string `json:"fusion"`
	RefActivate  bool   `json:"ref_activate"`
	Holdout      string `json:"holdout"`
	Weights      string `json:"weights"`
	Embedder     string `json:"embedder"`
	EmbedderName string `json:"embedder_name,omitempty"`
	TopK         int    `json:"topk"`
	NowFixed     string `json:"now_fixed"`
	GeneratedAt  string `json:"generated_at"`
}

// VotingDetail 单条 query 的试验明细（含分歧度，用于相关性分析）。
type VotingDetail struct {
	ID          string  `json:"id"`
	Bucket      string  `json:"bucket"`
	Query       string  `json:"query"`
	Expected    string  `json:"expected"`
	Top1Fact    string  `json:"top1_fact"`
	Top1ID      string  `json:"top1_id"`
	M1Hit       bool    `json:"m1_hit"`
	M1HitAt1    bool    `json:"m1_hit_at_1"`
	Consensus   float64 `json:"vote_consensus"`
	Divergence  float64 `json:"vote_divergence"`
	VoteSources int     `json:"vote_sources"`
	VoteScore   float64 `json:"top1_vote_score"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[voting] ")

	fusionFlag := flag.String("fusion", "rrf", "融合模式 rrf|vote（默认 rrf，isolate 复现）")
	refActivate := flag.Bool("ref-activate", true, "从 query 确定性提取参考系赋 QueryFrame（默认开；关闭时复现基线 ref 信号恒 0.5）")
	// R7-A：semantic 多路径冗余（默认关闭，不回归）。
	semRedundantFlag := flag.Bool("semantic-redundant", false, "R7-A：semantic 启用多路径冗余（默认关闭，保证不回归）")
	semAblatePathFlag := flag.String("semantic-ablate-path", "", "R7-A：多路径模式下消融的语义路径（semantic-path0|semantic-path1|semantic-path2；空=不消融）")
	holdoutFlag := flag.String("holdout", "", "评测集 NDJSON")
	weightsFlag := flag.String("weights", "", "权重 JSON（默认 eval/weights/rank_weights_r6_ref.json；缺失回退 rank_weights_en_r5.json）")
	embedderFlag := flag.String("embedder", "auto", "auto|vector|tfidf|none")
	vocabFlag := flag.String("vocab-path", "", "tfidf 词表缓存")
	reportFlag := flag.String("report", "r6a_vote", "报告文件名前缀")
	debugRef := flag.Bool("debug-ref", false, "R6-A 诊断：ref 桶 miss 的召回层诊断（正确记忆是否进候选集）")
	debugVote := flag.Bool("debug-vote", false, "R6-A 诊断：多跳桶 vote 模式 miss 的诊断")
	topK := flag.Int("topk", 5, "M1 判定候选窗口")
	voteTopKFlag := flag.Int("vote-topk", 1, "投票模式每柱解释深度（千脑柱间投票：每柱首选即投）")
	nowFixed := flag.String("now-fixed", "2026-10-13", "固定 rf.Now 基准日期")
	reportsDir := flag.String("reports-dir", "", "报告输出目录")
	flag.Parse()

	if *fusionFlag != "rrf" && *fusionFlag != "vote" {
		log.Fatalf("-fusion %q invalid: must be rrf|vote", *fusionFlag)
	}
	dir := resolveVotingReportsDir(*reportsDir)
	_ = os.MkdirAll(dir, 0o755)

	// ---- 语料定位 ----
	holdoutPath := *holdoutFlag
	if holdoutPath == "" {
		holdoutPath = resolveVotingPath(
			"../dataset/holdout.jsonl", "eval/dataset/holdout.jsonl", "dataset/holdout.jsonl",
			"../dataset/holdout_proxy.jsonl", "eval/dataset/holdout_proxy.jsonl", "dataset/holdout_proxy.jsonl",
		)
	}
	if holdoutPath == "" {
		log.Fatalf("未找到 holdout 语料（-holdout 指定）")
	}
	items, err := loadVotingHoldout(holdoutPath)
	if err != nil {
		log.Fatalf("load %s: %v", holdoutPath, err)
	}
	log.Printf("loaded %d items from %s", len(items), holdoutPath)

	// ---- 装配内核 ----
	ctx := context.Background()
	if *weightsFlag == "" {
		// R6-A 交付权重优先（ref 激活后的重校准态）；缺失时回退 R5 交付态。
		*weightsFlag = resolveVotingPath(
			"../weights/rank_weights_r6_ref.json", "eval/weights/rank_weights_r6_ref.json", "weights/rank_weights_r6_ref.json",
		)
		if *weightsFlag == "" {
			*weightsFlag = resolveVotingPath(
				"../weights/rank_weights_en_r5.json", "eval/weights/rank_weights_en_r5.json", "weights/rank_weights_en_r5.json",
			)
		}
	}
	if *weightsFlag == "" {
		log.Fatalf("未找到权重文件")
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
	rf.FusionMode = *fusionFlag
	if rf.FusionMode == kernel.FusionModeVote {
		// 投票模式下每路信号只把 top-K 作为「解释」参与计票；不设则退化为全候选投票，
		// 票数被 200 个候选稀释到无区分度（R6-A 实证：vote 整体 0.90 / ref 0.55）。
		rf.VoteTopK = *voteTopKFlag
	}
	// R7-A：semantic 多路径冗余（默认关闭不回归；开启后三路语义投票）。
	if *semRedundantFlag {
		rf.SemanticRedundant = true
		rf.SemanticAblatePath = *semAblatePathFlag
	}
	log.Printf("fusion=%s ref_activate=%v weights=%s baseNow=%s semantic_redundant=%v ablate_path=%q",
		*fusionFlag, *refActivate, *weightsFlag, baseNow.Format(time.RFC3339),
		*semRedundantFlag, *semAblatePathFlag)

	embedderName := ""
	switch *embedderFlag {
	case "vector":
		ve, verr := loadVotingVectorEmbedder()
		if verr != nil {
			log.Fatalf("load vector embedder: %v", verr)
		}
		rf.SetEmbedderImpl(ve)
		embedderName = ve.Name()
	case "tfidf":
		v, verr := loadVotingVocab(*vocabFlag, items)
		if verr != nil {
			log.Fatalf("build tfidf vocab: %v", verr)
		}
		rf.SetEmbedder(v.EmbedFn())
		embedderName = v.Name()
	case "auto":
		if ve, verr := loadVotingVectorEmbedder(); verr == nil {
			rf.SetEmbedderImpl(ve)
			embedderName = ve.Name()
			log.Printf("embedder auto->vector: %s", embedderName)
		} else {
			v, verr2 := loadVotingVocab(*vocabFlag, items)
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

	// ---- 逐条检索 ----
	m1 := NewM1Accumulator(*topK)
	var details []VotingDetail
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
		if it.Memory != nil {
			srv.SaveMemory(ctx, it.Memory, usr)
		}
		if it.Hop1Memory != nil {
			srv.SaveMemory(ctx, it.Hop1Memory, usr)
		}
		if it.Hop2Memory != nil {
			srv.SaveMemory(ctx, it.Hop2Memory, usr)
		}
		if it.SecondVersion != nil {
			srv.SaveMemory(ctx, it.SecondVersion, usr)
		}
		rf.Now = baseNow
		// 参考系激活（R6-A）：从 query 确定性提取参考系坐标（正则，不调 LLM）。
		// ⚠️ 关键：不能只设 rf.QueryFrame——RecallAndFuse 内部会用 rc.frame()
		// 覆盖 rf.QueryFrame（router_mem.go），因此必须把参考系设到 rc 上，
		// 使 rc.frame() 带出 Entity/Project/Scene，ref 信号才有区分度。
		var ent, proj, scene string
		if *refActivate {
			qf := extractQueryFrame(it.Query)
			ent, proj, scene = qf.Entity, qf.Project, qf.Scene
		}
		rc := kernel.RouterContext{
			Text:      it.Query,
			TenantID:  tenant,
			UserID:    usr,
			Entity:    ent,
			ProjectID: proj,
			Scene:     scene,
			TopK:      votingRecallTopK,
		}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		// R6-A 诊断（-debug-ref）：ref 桶 miss 的召回层诊断——正确记忆是否进候选。
		if *debugRef && it.Bucket == "reference_frame" && len(it.ExpectedMemoryIDs) > 0 {
			logRecallDebug(ctx, srv, rc, it)
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			log.Printf("WARN: recall %s: %v", it.ID, rerr)
			continue
		}
		// R6-A 诊断（-debug-vote）：多跳桶 vote 模式 miss 时打印融合 top-5 与召回层。
		if *debugVote && it.Bucket == "multi-hop" && len(it.ExpectedMemoryIDs) > 0 {
			logVoteDebug(rc, res, it)
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
		m1.Add(it.Bucket, trial)

		d := VotingDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query, Expected: expected,
			M1Hit: trial.Hit, M1HitAt1: trial.HitAt1,
		}
		if len(res.Fused) > 0 {
			d.Top1Fact = res.Fused[0].Fact
			d.Top1ID = res.Fused[0].MemoryID
			d.Consensus = res.Fused[0].VoteConsensus
			d.Divergence = res.Fused[0].VoteDivergence
			d.VoteScore = res.Fused[0].ConsensusVoteScore
			d.VoteSources = len(res.Fused[0].VoteSources)
		}
		details = append(details, d)
	}

	m1r := m1.Report()
	rep := map[string]any{
		"config": VotingConfig{
			Mode: "eval", Fusion: *fusionFlag, RefActivate: *refActivate,
			Holdout: holdoutPath, Weights: *weightsFlag,
			Embedder: *embedderFlag, EmbedderName: embedderName,
			TopK: *topK, NowFixed: *nowFixed,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		},
		"m1":          m1r,
		"details":     details,
		"correlation": buildDivergenceCorrelation(details),
	}
	writeVotingJSON(dir, *reportFlag, rep)

	log.Printf("[M1] n=%d hits=%d coverage=%.4f hit@1=%.4f mean_rr=%.4f",
		m1r.N, m1r.Hits, m1r.Coverage, m1r.HitAt1Rate, m1r.MeanRR)
	for bucket, bb := range m1r.ByBucket {
		log.Printf("  bucket %-16s n=%-4d coverage=%.4f hit@1=%d", bucket, bb.N, bb.Coverage, bb.HitAt1)
	}
	if *fusionFlag == "vote" {
		log.Printf("[vote] 分歧度-错误相关性: %+v", rep["correlation"])
	}
}

// ------------------------------------------------------------------
// query 参考系确定性提取（正则，不调 LLM；红线 4）
// ------------------------------------------------------------------

// reEntity 实体提取：中文/ASCII 混合的「客户X」「项目X」「场景X」等实体名。
// 口径：从 query 中提取形如「客户A」「客户甲」「项目0」「场景乙」的实体 token。
// 仅覆盖评测语料既有模式（确定性正则），不做语义推断。
var reVotingEntity = regexp.MustCompile(`(客户[A-Z]|项目[0-9A-Z]|场景[一-龥A-Z]|决策人[0-9A-Z]+)`)

// extractQueryFrame 从 query 文本确定性提取参考系坐标。
// 输出 kernel.ReferenceFrame 的子集：Entity / Project / Scene。
// 无匹配时返回空帧（与基线 rc.frame() 的 tenant/user 口径不同：本函数只赋
// 有证据的坐标，不给正偏向）。
func extractQueryFrame(query string) *kernel.ReferenceFrame {
	f := &kernel.ReferenceFrame{}
	seen := map[string]bool{}
	for _, m := range reVotingEntity.FindAllString(query, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		switch {
		case strings.HasPrefix(m, "客户"):
			if f.Entity == "" {
				f.Entity = m
			}
		case strings.HasPrefix(m, "项目"):
			if f.Project == "" {
				f.Project = m
			}
		case strings.HasPrefix(m, "场景"):
			if f.Scene == "" {
				f.Scene = m
			}
		case strings.HasPrefix(m, "决策人"):
			// 决策人是事实值而非参考系，跳过。
		}
	}
	return f
}

// ------------------------------------------------------------------
// 分歧度-错误相关性（Spearman + 分箱，确定性）
// ------------------------------------------------------------------

// CorrelationReport 分歧度-错误相关性报告。
type CorrelationReport struct {
	// Pairs 成对样本数（有分歧度数据且参与 M1 判定的 query 数）。
	Pairs int `json:"pairs"`
	// SpearmanRho 分歧度 vs 是否命中（hit=0, miss=1）的 Spearman 秩相关。
	SpearmanRho float64 `json:"spearman_rho"`
	// MeanDivByBucket miss 桶 vs hit 桶的平均分歧度。
	MeanDivMiss float64 `json:"mean_divergence_miss"`
	MeanDivHit  float64 `json:"mean_divergence_hit"`
	// ByDivBucket 分箱：分歧度区间 → {n, miss_rate}。
	ByDivBucket []DivBucket `json:"by_divergence_bucket"`
}

// DivBucket 单分箱。
type DivBucket struct {
	Lo       string  `json:"lo"`
	Hi       string  `json:"hi"`
	N        int     `json:"n"`
	Miss     int     `json:"miss"`
	MissRate float64 `json:"miss_rate"`
}

// buildDivergenceCorrelation 计算分歧度与 top-1 错误率的 Spearman 相关 + 分箱。
// 确定性：样本按 detail 出现顺序收集，秩计算用标准公式（无随机源）。
func buildDivergenceCorrelation(details []VotingDetail) CorrelationReport {
	rep := CorrelationReport{}
	type sample struct {
		div    float64
		miss   float64
		bucket string
	}
	var xs, ys []float64
	var missDivs, hitDivs []float64
	for _, d := range details {
		// 只用投票模式产出的样本（分歧度 > 0 或 sources>0）。
		if d.VoteSources == 0 {
			continue
		}
		miss := 0.0
		if !d.M1HitAt1 {
			miss = 1.0
		}
		xs = append(xs, d.Divergence)
		ys = append(ys, miss)
		if miss > 0 {
			missDivs = append(missDivs, d.Divergence)
		} else {
			hitDivs = append(hitDivs, d.Divergence)
		}
	}
	rep.Pairs = len(xs)
	if len(xs) < 2 {
		return rep
	}
	rep.SpearmanRho = spearman(xs, ys)
	rep.MeanDivMiss = mean(missDivs)
	rep.MeanDivHit = mean(hitDivs)

	// 分箱：0-0.2 / 0.2-0.4 / 0.4-0.6 / 0.6-0.8 / 0.8-1.0。
	bounds := []float64{0, 0.2, 0.4, 0.6, 0.8, 1.0}
	type agg struct{ n, miss int }
	var aggs []agg
	for i := 0; i < len(bounds)-1; i++ {
		aggs = append(aggs, agg{})
	}
	for _, s := range xs {
		_ = s
	}
	// 重新配对迭代（避免上面 sample 结构未用导致 lint）。
	type pair struct {
		div  float64
		miss int
	}
	var pairs []pair
	for i := range xs {
		pairs = append(pairs, pair{div: xs[i], miss: int(ys[i])})
	}
	for _, p := range pairs {
		idx := -1
		for i := 0; i < len(bounds)-1; i++ {
			if p.div >= bounds[i] && p.div < bounds[i+1] {
				idx = i
				break
			}
		}
		if p.div == 1.0 {
			idx = len(bounds) - 2
		}
		if idx < 0 {
			continue
		}
		aggs[idx].n++
		aggs[idx].miss += p.miss
	}
	for i := 0; i < len(bounds)-1; i++ {
		b := DivBucket{Lo: fmt.Sprintf("%.1f", bounds[i]), Hi: fmt.Sprintf("%.1f", bounds[i+1]),
			N: aggs[i].n, Miss: aggs[i].miss}
		if aggs[i].n > 0 {
			b.MissRate = float64(aggs[i].miss) / float64(aggs[i].n)
		}
		rep.ByDivBucket = append(rep.ByDivBucket, b)
	}
	return rep
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// spearman Spearman 秩相关（Pearson on ranks；平局取平均秩）。
func spearman(x, y []float64) float64 {
	n := len(x)
	if n == 0 || len(y) != n {
		return 0
	}
	rx := rankVector(x)
	ry := rankVector(y)
	return pearson(rx, ry)
}

func rankVector(v []float64) []float64 {
	n := len(v)
	type idxv struct {
		v float64
		i int
	}
	arr := make([]idxv, n)
	for i, x := range v {
		arr[i] = idxv{x, i}
	}
	sort.SliceStable(arr, func(a, b int) bool {
		if arr[a].v != arr[b].v {
			return arr[a].v < arr[b].v
		}
		return arr[a].i < arr[b].i
	})
	ranks := make([]float64, n)
	i := 0
	for i < n {
		j := i
		for j < n && arr[j].v == arr[i].v {
			j++
		}
		avgRank := float64(i+1+j) / 2.0 // 1-based 平均秩
		for k := i; k < j; k++ {
			ranks[arr[k].i] = avgRank
		}
		i = j
	}
	return ranks
}

func pearson(x, y []float64) float64 {
	n := len(x)
	if n < 2 {
		return 0
	}
	mx, my := mean(x), mean(y)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dx := x[i] - mx
		dy := y[i] - my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}

// ------------------------------------------------------------------
// 工具函数（语料加载 / embedder / 报告写出）
// ------------------------------------------------------------------

// logRecallDebug R6-A 诊断：打印 ref 桶 miss 时正确记忆是否进候选集，
// 用于确认「召回层丢失 vs 融合层被压」。
func logRecallDebug(ctx context.Context, srv *kernel.APIServer, rc kernel.RouterContext, it VotingItem) {
	res, err := srv.RecallAndFuse(ctx, rc)
	if err != nil {
		log.Printf("[debug %s] recall err: %v", it.ID, err)
		return
	}
	expIDs := map[string]bool{}
	for _, id := range it.ExpectedMemoryIDs {
		expIDs[id] = true
	}
	// 召回层：是否含正确记忆。
	var recalledIDs []string
	for _, l := range res.Recalled {
		for _, c := range l.Candidates {
			if c != nil && c.Memory != nil {
				recalledIDs = append(recalledIDs, c.Memory.MemoryID)
			}
		}
	}
	inRecalled := false
	for _, id := range recalledIDs {
		if expIDs[id] {
			inRecalled = true
			break
		}
	}
	// 融合 top-5 排名。
	rank := 0
	for i, f := range res.Fused {
		if i >= 5 {
			break
		}
		if expIDs[f.MemoryID] {
			rank = i + 1
			break
		}
	}
	// R6-A 深度诊断：ref 信号对正确记忆 vs top-1 的具体分（验证信号是否区分）。
	expRefScore := -1.0
	if res.Trace != nil {
		for _, sig := range res.Trace.Signals {
			if sig.Name == "reference_frame" {
				type kv struct {
					id string
					sc float64
				}
				var arr []kv
				for id, sc := range sig.Scores {
					arr = append(arr, kv{id, sc})
				}
				sort.SliceStable(arr, func(i, j int) bool { return arr[i].sc > arr[j].sc })
				for i, e := range arr {
					if i >= 5 {
						break
					}
					mark := ""
					if expIDs[e.id] {
						mark = " <== expected"
						expRefScore = e.sc
					}
					log.Printf("[debug %s] refSig[%d] %s=%.3f%s", it.ID, i, e.id, e.sc, mark)
				}
				log.Printf("[debug %s] refSig total=%d", it.ID, len(arr))
			}
		}
	}
	log.Printf("[debug %s] query=%q expected_ids=%v in_recalled=%v top5_rank=%d refScore(exp)=%.3f top1=%s",
		it.ID, it.Query, it.ExpectedMemoryIDs, inRecalled, rank, expRefScore, safeTop1Fact(res))
	// R6-A 深度诊断（二）：query 帧坐标 vs 正确记忆候选帧坐标，定位 ref 信号 0 的层级。
	log.Printf("[debug %s] qframe tenant=%q user=%q entity=%q project=%q scene=%q",
		it.ID, rc.TenantID, rc.UserID, rc.Entity, rc.ProjectID, rc.Scene)
	for _, l := range res.Recalled {
		for _, c := range l.Candidates {
			if c == nil || c.Memory == nil {
				continue
			}
			if !expIDs[c.Memory.MemoryID] {
				continue
			}
			rf := c.Memory.ReferenceFrame
			log.Printf("[debug %s] exp cand %s layer=%s frame tenant=%q user=%q entity=%q project=%q scene=%q fact=%q",
				it.ID, c.Memory.MemoryID, c.Layer, rf.Tenant, rf.User, rf.Entity, rf.Project, rf.Scene, c.Memory.Fact)
		}
	}
}

func safeTop1Fact(res kernel.RouterResult) string {
	if len(res.Fused) > 0 {
		return res.Fused[0].Fact
	}
	return ""
}

// logVoteDebug R6-A 诊断：vote 模式 multi-hop 桶 miss 时打印融合 top-5
// 与各信号分，定位「召回层丢失 vs 投票排名被压」。
func logVoteDebug(rc kernel.RouterContext, res kernel.RouterResult, it VotingItem) {
	expIDs := map[string]bool{}
	for _, id := range it.ExpectedMemoryIDs {
		expIDs[id] = true
	}
	// 召回层
	var recalledIDs []string
	for _, l := range res.Recalled {
		for _, c := range l.Candidates {
			if c != nil && c.Memory != nil {
				recalledIDs = append(recalledIDs, c.Memory.MemoryID)
			}
		}
	}
	inRecalled := false
	for _, id := range recalledIDs {
		if expIDs[id] {
			inRecalled = true
			break
		}
	}
	// 融合 top-5
	rank := 0
	for i, f := range res.Fused {
		if i >= 5 {
			break
		}
		if expIDs[f.MemoryID] {
			rank = i + 1
			break
		}
	}
	log.Printf("[vote %s] query=%q exp=%v in_recalled=%v top5_rank=%d n_fused=%d",
		it.ID, it.Query, it.ExpectedMemoryIDs, inRecalled, rank, len(res.Fused))
	for i, f := range res.Fused {
		if i >= 5 {
			break
		}
		log.Printf("  top%d %s score=%.4f consensus=%.4f div=%.4f fact=%q",
			i+1, f.MemoryID, f.Score, f.VoteConsensus, f.VoteDivergence, f.Fact)
	}
	// 正确记忆的各信号分
	for _, l := range res.Recalled {
		for _, c := range l.Candidates {
			if c == nil || c.Memory == nil || !expIDs[c.Memory.MemoryID] {
				continue
			}
			log.Printf("  expected %s layer=%s fact=%q", c.Memory.MemoryID, c.Layer, c.Memory.Fact)
		}
	}
}

func resolveVotingPath(candidates ...string) string {
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

func resolveVotingReportsDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if d := resolveVotingPath("../reports", "eval/reports", "reports"); d != "" {
		return d
	}
	return "reports"
}

func loadVotingHoldout(path string) ([]VotingItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []VotingItem
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it VotingItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("parse holdout line: %w", err)
		}
		items = append(items, it)
	}
	return items, nil
}

func loadVotingVectorEmbedder() (*kernel.VectorEmbedder, error) {
	matrix := resolveVotingPath("../vectors/vectors.npy", "eval/vectors/vectors.npy", "vectors/vectors.npy")
	meta := resolveVotingPath("../vectors/vectors_meta.json", "eval/vectors/vectors_meta.json", "vectors/vectors_meta.json")
	index := resolveVotingPath("../vectors/vectors_index.jsonl", "eval/vectors/vectors_index.jsonl", "vectors/vectors_index.jsonl")
	if matrix == "" || meta == "" {
		return nil, fmt.Errorf("向量文件不在位（matrix=%q meta=%q）", matrix, meta)
	}
	cfg := kernel.VectorEmbedderConfig{MatrixPath: matrix, MetaPath: meta}
	if index != "" {
		cfg.IndexPath = index
	}
	return kernel.NewVectorEmbedder(cfg)
}

func loadVotingVocab(vocabPath string, items []VotingItem) (*kernel.CharNgramVocab, error) {
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

func writeVotingJSON(dir, prefix string, v any) {
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
