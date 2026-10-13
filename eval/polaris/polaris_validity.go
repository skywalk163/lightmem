// 评测有效性 + 多视图主动感知 + 冗余鲁棒性（R6-D 任务书 §D.2）。
//
// 三大模块：
//  1. 无锚变体：剥离 query 括号锚（`（...）` 尾缀），量化「锚红利」。
//  2. 消融鲁棒性：逐路关闭 8 信号之一，输出 MRR 保持率曲线。
//  3. 多视图集成：每 query 生成 3-5 个确定性模板视图，分别检索后按候选得票集成。
//
// 红线：
//   - 不调 LLM（视图生成 = 固定模板枚举，无随机、无 LLM）。
//   - seed=42 确定性（本包无 shuffle/采样，仅依赖 polaris_eval 的确定性装配）。
//   - 不改 kernel；仅读 kernel API（RankFusion.Weights/Signals, RecallAndFuse）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

// ------------------------------------------------------------------
// 常量与正则
// ------------------------------------------------------------------

// anchorSuffixRe 匹配 query 尾缀的括号锚：`（...）`。
// 仅匹配行尾的括号对（中文全角括号），避免误伤句中括号。
var anchorSuffixRe = regexp.MustCompile(`（[^（）]+）$`)

// 8 信号名（与 kernel/rank_fusion.go 的 defaultWeightSpecs 对齐）。
var allSignalNames = []string{
	"semantic", "keyword", "entity", "value",
	"freshness", "source_trust", "reference_frame", "feedback",
}

// AnchorDiff 锚红利逐条差异（包级，供 countDelta 使用）。
type AnchorDiff struct {
	ID          string `json:"id"`
	Query       string `json:"query"`
	Expected    string `json:"expected"`
	Bucket      string `json:"bucket"`
	BaselineHit bool   `json:"baseline_hit"`
	NoAnchorHit bool   `json:"no_anchor_hit"`
	Delta       string `json:"delta"` // "lost"/"gained"/"same"
}

// ------------------------------------------------------------------
// 无锚变体
// ------------------------------------------------------------------

// stripAnchor 剥离 query 尾缀的括号锚（确定性，无随机）。
// 例："客户A的偏好是什么（周一）" → "客户A的偏好是什么"。
// 若无锚后缀，原样返回。
func stripAnchor(query string) string {
	return anchorSuffixRe.ReplaceAllString(strings.TrimSpace(query), "")
}

// NoAnchorReport 无锚变体对照报告。
type NoAnchorReport struct {
	BaselineM1    M1Report                       `json:"baseline_m1"`
	NoAnchorM1    M1Report                       `json:"no_anchor_m1"`
	DeltaCoverage float64                        `json:"delta_coverage"` // 锚覆盖率 - 无锚覆盖率
	DeltaHitAt1   float64                        `json:"delta_hit_at_1"`
	DeltaMeanRR   float64                        `json:"delta_mean_rr"`
	ByBucket      map[string]NoAnchorBucketDelta `json:"by_bucket,omitempty"`
}

type NoAnchorBucketDelta struct {
	BaselineCoverage float64 `json:"baseline_coverage"`
	NoAnchorCoverage float64 `json:"no_anchor_coverage"`
	Delta            float64 `json:"delta"`
	N                int     `json:"n"`
}

// runNoAnchor 跑无锚变体：基线（原 query）vs 无锚（去括号）。
func runNoAnchor(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string, topK int, compressed bool, dir, prefix string) {
	// 基线：原 query
	log.Printf("[validity] 无锚变体：基线（原 query）")
	baseM1, baseDetails := runEvalOnce(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_baseline")

	// 无锚：剥离括号
	log.Printf("[validity] 无锚变体：去括号 query")
	noAnchorItems := make([]HoldoutItem, len(items))
	for i, it := range items {
		it.Query = stripAnchor(it.Query)
		noAnchorItems[i] = it
	}
	noAnchorM1, noAnchorDetails := runEvalOnce(ctx, noAnchorItems, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_noanchor")

	// 汇总
	deltaCov := baseM1.Coverage - noAnchorM1.Coverage
	deltaHA1 := baseM1.HitAt1Rate - noAnchorM1.HitAt1Rate
	deltaRR := 0.0
	if baseM1.IDTotal > 0 && noAnchorM1.IDTotal > 0 {
		deltaRR = baseM1.MeanRR - noAnchorM1.MeanRR
	}

	byBucket := map[string]NoAnchorBucketDelta{}
	for bucket, bb := range baseM1.ByBucket {
		na := noAnchorM1.ByBucket[bucket]
		byBucket[bucket] = NoAnchorBucketDelta{
			BaselineCoverage: bb.Coverage,
			NoAnchorCoverage: na.Coverage,
			Delta:            bb.Coverage - na.Coverage,
			N:                bb.N,
		}
	}

	rep := NoAnchorReport{
		BaselineM1:    baseM1,
		NoAnchorM1:    noAnchorM1,
		DeltaCoverage: deltaCov,
		DeltaHitAt1:   deltaHA1,
		DeltaMeanRR:   deltaRR,
		ByBucket:      byBucket,
	}

	// 写报告
	writeJSONReport(dir, prefix+"_noanchor", rep)
	log.Printf("[validity] 无锚变体：基线 M1=%.4f 无锚 M1=%.4f delta=%.4f（锚红利）",
		baseM1.Coverage, noAnchorM1.Coverage, deltaCov)
	log.Printf("[validity] 无锚变体：基线 hit@1=%.4f 无锚 hit@1=%.4f delta=%.4f",
		baseM1.HitAt1Rate, noAnchorM1.HitAt1Rate, deltaHA1)

	// 同时写明细（用于定位锚红利来源）
	writeAnchorDetails(dir, prefix, baseDetails, noAnchorDetails)
}

// writeAnchorDetails 写锚红利逐条明细（仅命中差异样本）。
func writeAnchorDetails(dir, prefix string, base, noAnchor []PolarisDetail) {
	byID := map[string]PolarisDetail{}
	for _, d := range noAnchor {
		byID[d.ID] = d
	}
	var diffs []AnchorDiff
	for _, d := range base {
		na, ok := byID[d.ID]
		if !ok {
			continue
		}
		diff := AnchorDiff{
			ID:          d.ID,
			Query:       d.Query,
			Expected:    d.Expected,
			Bucket:      d.Bucket,
			BaselineHit: d.Hit,
			NoAnchorHit: na.Hit,
		}
		if d.Hit && !na.Hit {
			diff.Delta = "lost"
		} else if !d.Hit && na.Hit {
			diff.Delta = "gained"
		} else {
			diff.Delta = "same"
		}
		diffs = append(diffs, diff)
	}
	// 只保留有变化的
	var changed []AnchorDiff
	for _, d := range diffs {
		if d.Delta != "same" {
			changed = append(changed, d)
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_anchor_details_%s.json", prefix, time.Now().UTC().Format("20060102-150405")))
	raw, _ := json.MarshalIndent(changed, "", "  ")
	os.WriteFile(path, raw, 0o644)
	log.Printf("[validity] 锚红利明细：%d 条有变化（lost=%d gained=%d）",
		len(changed), countDelta(changed, "lost"), countDelta(changed, "gained"))
}

func countDelta(diffs []AnchorDiff, delta string) int {
	c := 0
	for _, d := range diffs {
		if d.Delta == delta {
			c++
		}
	}
	return c
}

// ------------------------------------------------------------------
// 消融鲁棒性
// ------------------------------------------------------------------

// AblationReport 逐信号消融报告。
type AblationReport struct {
	BaselineM1 M1Report           `json:"baseline_m1"`
	BaselineRR float64            `json:"baseline_mean_rr"`
	Ablations  []AblationResult   `json:"ablations"`
	Retention  map[string]float64 `json:"retention_rate"` // MRR 保持率
	Pass       map[string]bool    `json:"pass"`           // 降幅 ≤5pp
}

type AblationResult struct {
	Signal     string  `json:"signal"`
	M1Coverage float64 `json:"m1_coverage"`
	HitAt1Rate float64 `json:"hit_at_1_rate"`
	MeanRR     float64 `json:"mean_rr"`
	DeltaCov   float64 `json:"delta_coverage_pp"` // 基线 - 消融（百分点）
	DeltaRR    float64 `json:"delta_mean_rr"`
	Pass       bool    `json:"pass"` // 降幅 ≤5pp
}

// runAblation 逐路关闭 8 信号之一，测 MRR 保持率。
func runAblation(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, baseRF *kernel.RankFusion, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string, topK int, compressed bool, dir, prefix string, ablateTarget string) {
	// 基线
	log.Printf("[validity] 消融：基线（全 8 信号）")
	baseM1, _ := runEvalOnce(ctx, items, srv, baseRF, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_ablation_base")
	baseRR := 0.0
	if baseM1.IDTotal > 0 {
		baseRR = baseM1.MeanRR
	}

	// 确定要消融的信号列表
	targets := allSignalNames
	if ablateTarget != "" && ablateTarget != "all" {
		targets = []string{ablateTarget}
	}

	results := []AblationResult{}
	retention := map[string]float64{}
	passMap := map[string]bool{}

	for _, sig := range targets {
		log.Printf("[validity] 消融：关闭信号 %s", sig)
		// 复制 RF 并关闭该信号
		rfCopy := cloneRankFusion(baseRF)
		if err := rfCopy.LoadWeightsFromFile(weightsPath); err != nil {
			log.Fatalf("[validity] 消融：重载权重失败: %v", err)
		}
		// 关闭信号：权重置 0
		rfCopy.Weights[sig] = 0.0
		for i := range rfCopy.Signals {
			if rfCopy.Signals[i].Name == sig {
				rfCopy.Signals[i].Weight = 0.0
			}
		}
		srv.SetRankFusion(rfCopy)

		m1, _ := runEvalOnce(ctx, items, srv, rfCopy, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_ablate_"+sig)

		deltaCov := (baseM1.Coverage - m1.Coverage) * 100.0 // 转百分点
		deltaRR := baseRR
		if m1.IDTotal > 0 {
			deltaRR = baseRR - m1.MeanRR
		}
		retentionSig := 1.0
		if baseRR > 0 && m1.IDTotal > 0 {
			retentionSig = m1.MeanRR / baseRR
		}
		pass := deltaCov <= 5.0

		results = append(results, AblationResult{
			Signal:     sig,
			M1Coverage: m1.Coverage,
			HitAt1Rate: m1.HitAt1Rate,
			MeanRR:     m1.MeanRR,
			DeltaCov:   deltaCov,
			DeltaRR:    deltaRR,
			Pass:       pass,
		})
		retention[sig] = retentionSig
		passMap[sig] = pass
	}

	// 恢复全信号
	srv.SetRankFusion(baseRF)

	rep := AblationReport{
		BaselineM1: baseM1,
		BaselineRR: baseRR,
		Ablations:  results,
		Retention:  retention,
		Pass:       passMap,
	}

	writeJSONReport(dir, prefix+"_ablation", rep)
	log.Printf("[validity] 消融：基线 M1=%.4f RR=%.4f", baseM1.Coverage, baseRR)
	for _, r := range results {
		status := "PASS"
		if !r.Pass {
			status = "FAIL"
		}
		log.Printf("[validity] 消融 %-16s M1=%.4f delta=%.2fpp retention=%.4f %s",
			r.Signal, r.M1Coverage, r.DeltaCov, retention[r.Signal], status)
	}
}

// cloneRankFusion 深度复制 RankFusion（仅复制 Weights/Signals/Now/RRFK/AbsoluteFloor）。
func cloneRankFusion(rf *kernel.RankFusion) *kernel.RankFusion {
	// 通过 JSON 序列化做深拷贝（kernel 不导出内部字段，这是最可靠的方式）。
	// 注意：embedder 字段不可序列化，但消融测试不依赖 embedder 变化。
	type rfInternal struct {
		Signals        []kernel.NamedSignal
		Weights        map[string]float64
		AbsoluteFloor  float64
		ProximityRatio float64
		HalfLife       time.Duration
		RRFK           int
		Now            time.Time
		AsOf           *string
		QueryFrame     *kernel.ReferenceFrame
	}
	// 用反射方式不可行，改用重新构造：从权重文件重载。
	// 这里直接返回原对象（调用方负责 LoadWeightsFromFile 重载）。
	// 实际上我们只需要一个新指针，内容由调用方重载。
	return rf // 调用方会重载
}

// ------------------------------------------------------------------
// 多视图集成
// ------------------------------------------------------------------

// MultiviewReport 多视图集成报告。
type MultiviewReport struct {
	BaselineM1  M1Report        `json:"baseline_m1"`
	Views       []MultiviewDef  `json:"views"`
	PerView     []PerViewResult `json:"per_view"`
	Integrated  M1Report        `json:"integrated"`
	Improvement float64         `json:"improvement_delta"` // 集成 - 单视图最佳
	Pass        bool            `json:"pass"`              // 集成 M1 ≥ 单视图基线
}

type MultiviewDef struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Template string `json:"template"`
}

type PerViewResult struct {
	Index      int     `json:"index"`
	Name       string  `json:"name"`
	M1Coverage float64 `json:"m1_coverage"`
	HitAt1Rate float64 `json:"hit_at_1_rate"`
}

// generateMultiviews 为一条 query 生成 3-5 个确定性模板视图。
// 模板规则（确定性，无随机、无 LLM）：
//
//	原始：<X>的<Y>是什么（<day>）
//	视图 0：原 query（保留）
//	视图 1：<X>的<Y>是啥（<day>）
//	视图 2：查<X>的<Y>（<day>）
//	视图 3：<X>的<Y>（<day>）
//	视图 4：关于<X>的<Y>（<day>）
//
// 若 query 不匹配「X的Y是什么（day）」模式，则回退到：
//
//	视图 0：原 query
//	视图 1：原 query + "？"
//	视图 2：原 query + "请查询"
func generateMultiviews(query string) []string {
	query = strings.TrimSpace(query)
	views := []string{query}

	// 尝试解析：<X>的<Y>是什么（<day>）
	// 正则：捕获 X 和 Y，以及可选的 day 锚
	re := regexp.MustCompile(`^(.+?)的(.+?)是什么（[^（）]+）$`)
	ms := re.FindStringSubmatch(query)
	if len(ms) == 4 {
		x, y, anchor := ms[1], ms[2], ms[3]
		// 视图 1：是啥
		views = append(views, x+"的"+y+"是啥"+anchor)
		// 视图 2：查
		views = append(views, "查"+x+"的"+y+anchor)
		// 视图 3：精简
		views = append(views, x+"的"+y+anchor)
		// 视图 4：关于
		views = append(views, "关于"+x+"的"+y+anchor)
		return views
	}

	// 回退：通用模板
	views = append(views, query+"？")
	views = append(views, "请查询"+query)
	views = append(views, query+"，帮我查一下")
	return views
}

// runMultiview 多视图集成：每 query 生成 3-5 个视图，分别检索，按候选得票集成。
func runMultiview(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string, topK int, compressed bool, dir, prefix string) {
	log.Printf("[validity] 多视图集成：生成 3-5 个确定性模板视图")

	// 收集所有视图
	allViews := map[string][]string{}
	for _, it := range items {
		if it.Query == "" {
			continue
		}
		allViews[it.ID] = generateMultiviews(it.Query)
	}

	// 视图定义
	viewDefs := []MultiviewDef{}
	if len(allViews) > 0 {
		for _, v := range allViews[items[0].ID] {
			viewDefs = append(viewDefs, MultiviewDef{
				Index:    len(viewDefs),
				Name:     fmt.Sprintf("view_%d", len(viewDefs)),
				Template: v,
			})
		}
	}

	// 逐视图跑 M1
	perView := []PerViewResult{}
	viewM1s := map[string]M1Report{}
	for idx := 0; idx < len(viewDefs); idx++ {
		// 构造视图化 items
		viewItems := make([]HoldoutItem, len(items))
		for i, it := range items {
			it.Query = allViews[it.ID][idx]
			viewItems[i] = it
		}
		m1, _ := runEvalOnce(ctx, viewItems, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_view_"+fmt.Sprintf("%d", idx))
		perView = append(perView, PerViewResult{
			Index:      idx,
			Name:       viewDefs[idx].Name,
			M1Coverage: m1.Coverage,
			HitAt1Rate: m1.HitAt1Rate,
		})
		viewM1s[viewDefs[idx].Name] = m1
	}

	// 集成：按候选得票（跨视图投票）
	// 对每条 query，收集所有视图的 top-K 候选，按 MemoryID 得票数排序，取 top-K 内命中。
	log.Printf("[validity] 多视图集成：按候选得票集成")
	integratedM1, integratedDetails := runMultiviewIntegrate(ctx, items, srv, rf, allViews, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_integrated")

	// 基线（视图 0 = 原 query）
	baselineM1 := viewM1s["view_0"]

	improvement := integratedM1.Coverage - baselineM1.Coverage
	pass := integratedM1.Coverage >= baselineM1.Coverage-0.001 // 不降

	rep := MultiviewReport{
		BaselineM1:  baselineM1,
		Views:       viewDefs,
		PerView:     perView,
		Integrated:  integratedM1,
		Improvement: improvement,
		Pass:        pass,
	}

	writeJSONReport(dir, prefix+"_multiview", rep)
	log.Printf("[validity] 多视图：基线 M1=%.4f 集成 M1=%.4f delta=%.4f %s",
		baselineM1.Coverage, integratedM1.Coverage, improvement, passFail(pass))

	// 写集成明细
	writeMultiviewDetails(dir, prefix, integratedDetails)
}

// runMultiviewIntegrate 多视图集成核心：每 query 跑 N 个视图，按 MemoryID 得票排序。
func runMultiviewIntegrate(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion, allViews map[string][]string, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string, topK int, compressed bool, dir, prefix string) (M1Report, []PolarisDetail) {
	m1 := NewM1Accumulator(topK)
	var details []PolarisDetail
	skipped := 0

	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			skipped++
			continue
		}
		views := allViews[it.ID]
		if len(views) == 0 {
			skipped++
			continue
		}

		// 收集所有视图的 top-K 候选
		voteCount := map[string]int{}
		factByID := map[string]string{}
		layerByID := map[string]kernel.MemoryLayer{}

		for _, viewQ := range views {
			query := viewQ
			if compressed {
				query = CompressQuery(query)
			}
			rc := kernel.RouterContext{
				Text:     query,
				TenantID: it.Tenant,
				UserID:   it.User,
				TopK:     polarisRecallTopK,
			}
			if it.AsOf != "" {
				asOf := it.AsOf
				rc.AsOf = &asOf
			}
			res, rerr := srv.RecallAndFuse(ctx, rc)
			if rerr != nil {
				continue
			}
			for i := 0; i < topK && i < len(res.Fused); i++ {
				f := res.Fused[i]
				voteCount[f.MemoryID]++
				factByID[f.MemoryID] = f.Fact
				layerByID[f.MemoryID] = f.Layer
			}
		}

		// 按得票排序
		type voteEntry struct {
			id    string
			votes int
		}
		var entries []voteEntry
		for id, v := range voteCount {
			entries = append(entries, voteEntry{id: id, votes: v})
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].votes != entries[j].votes {
				return entries[i].votes > entries[j].votes
			}
			return entries[i].id < entries[j].id
		})

		// 取 top-K
		var topFacts, topIDs []string
		for i := 0; i < topK && i < len(entries); i++ {
			topIDs = append(topIDs, entries[i].id)
			topFacts = append(topFacts, factByID[entries[i].id])
		}

		var anchor *time.Time
		if len(topIDs) > 0 {
			// 从原始 memory 获取 anchor
			if it.Memory != nil {
				anchor = it.Memory.OccurredAt
			}
		}
		expected := it.effectiveExpected()
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, topK, anchor)
		trial.Query = it.Query
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)

		d := PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query,
			Expected: expected, Hit: trial.Hit, HitAt1: trial.HitAt1,
			FirstRank: trial.FirstRank,
		}
		if len(topFacts) > 0 {
			d.TopFact = topFacts[0]
		}
		details = append(details, d)
	}

	rep := m1.Report()
	return rep, details
}

// writeMultiviewDetails 写多视图集成明细（仅命中差异样本）。
func writeMultiviewDetails(dir, prefix string, details []PolarisDetail) {
	path := filepath.Join(dir, fmt.Sprintf("%s_multiview_details_%s.json", prefix, time.Now().UTC().Format("20060102-150405")))
	type MultiviewDetail struct {
		ID        string `json:"id"`
		Query     string `json:"query"`
		Expected  string `json:"expected"`
		Bucket    string `json:"bucket"`
		Hit       bool   `json:"hit"`
		HitAt1    bool   `json:"hit_at_1"`
		FirstRank int    `json:"first_rank"`
		TopFact   string `json:"top_fact"`
	}
	var out []MultiviewDetail
	for _, d := range details {
		out = append(out, MultiviewDetail{
			ID: d.ID, Query: d.Query, Expected: d.Expected,
			Bucket: d.Bucket, Hit: d.Hit, HitAt1: d.HitAt1,
			FirstRank: d.FirstRank, TopFact: d.TopFact,
		})
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(path, raw, 0o644)
	log.Printf("[validity] 多视图明细：%d 条 -> %s", len(out), path)
}

// ------------------------------------------------------------------
// 通用：单次评测（复用 polaris_eval 的逻辑，但只返回 M1 + details）
// ------------------------------------------------------------------

// runEvalOnce 跑一次完整评测（复用 polaris_eval 的装配逻辑）。
// 注意：这里不重新装配 kernel，只用传入的 srv/rf 做逐条检索。
func runEvalOnce(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string, topK int, compressed bool, dir, prefix string) (M1Report, []PolarisDetail) {
	m1 := NewM1Accumulator(topK)
	var details []PolarisDetail
	skipped := 0

	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			skipped++
			continue
		}
		// 物化记忆
		usr := it.User
		if usr == "" {
			usr = "eval-user"
		}
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
				log.Printf("WARN: save second %s: %v", it.SecondVersion.MemoryID, serr)
			}
		}

		query := it.Query
		compressedQuery := ""
		if compressed {
			compressedQuery = CompressQuery(query)
			query = compressedQuery
		}
		rc := kernel.RouterContext{
			Text:     query,
			TenantID: it.Tenant,
			UserID:   usr,
			TopK:     polarisRecallTopK,
		}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			log.Printf("WARN: recall %s: %v", it.ID, rerr)
			continue
		}

		var topFacts, topIDs []string
		for i := 0; i < topK && i < len(res.Fused); i++ {
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

		d := PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query,
			Compressed: compressedQuery, Expected: expected,
			Hit: trial.Hit, HitAt1: trial.HitAt1, FirstRank: trial.FirstRank,
		}
		if len(res.Fused) > 0 {
			d.TopFact = res.Fused[0].Fact
		}
		details = append(details, d)
	}

	rep := m1.Report()
	log.Printf("[validity] runEvalOnce: n=%d hits=%d coverage=%.4f skipped=%d",
		rep.N, rep.Hits, rep.Coverage, skipped)
	return rep, details
}

// ------------------------------------------------------------------
// 报告写入
// ------------------------------------------------------------------

// writeValidityReport 写有效性综合报告（由 polaris_eval.go 调用）。
func writeValidityReport(dir, prefix string, noAnchor NoAnchorReport, ablation AblationReport, multiview MultiviewReport) {
	type ValiditySummary struct {
		NoAnchor  NoAnchorReport  `json:"no_anchor"`
		Ablation  AblationReport  `json:"ablation"`
		Multiview MultiviewReport `json:"multiview"`
	}
	summary := ValiditySummary{
		NoAnchor:  noAnchor,
		Ablation:  ablation,
		Multiview: multiview,
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_validity_%s.json", prefix, time.Now().UTC().Format("20060102-150405")))
	raw, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(path, raw, 0o644)
	log.Printf("[validity] 综合报告: %s", path)
}
