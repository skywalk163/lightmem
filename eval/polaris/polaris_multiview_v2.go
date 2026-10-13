// R7-B 确定性多视图 v2（Q2 立项任务书 §B.3）。
//
// 与 R6-D legacy 多视图的区别：
//   - legacy 视图只是加语气词/引导词（"？"、"请查询"、"帮我查一下"），
//     与 view_0 高度相关且本身是劣化视图，集成 M1=0.8339 < 0.9525。
//   - v2 视图基于**结构化字段重排**，视图间低相关、高互补：
//     view_0：原 query（基线，保留全部 token）
//     view_1：槽值倒装——把括号锚前置为「锚：主体」（保留全部 token，换语序）
//     view_2：实体帧窄视——只保留 entity + 锚，丢属性（窄聚焦）
//     view_3：候选回读——用 baseline top-1 候选的 Fact 作为回读 query
//     （千脑「思考=在参考系中运动」：在候选的知识组织里运动）
//
// 集成：kernel.ConsensusVoter 倒数秩投票，输出共识序 + 分歧度。
//
// 红线：
//   - 不调 LLM；所有视图生成 = 确定性字符串重排。
//   - seed=42 确定性（本文件无 shuffle/采样）。
//   - 不修改 R6-D 的 generateMultiviews / runMultiview（legacy 原样保留）。
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
// v2 视图生成器（确定性，纯字符串重排）
// ------------------------------------------------------------------

// anchorV2Re 匹配行尾全角括号锚：（...），捕获组 1 为括号内内容。
var anchorV2Re = regexp.MustCompile(`（([^（）]+)）\s*$`)

// queryStruct 解析后的 query 结构。
type queryStruct struct {
	Body   string // 去括号锚后的主体
	Anchor string // 括号内内容（不含括号）；无锚则为空
}

// parseQueryStruct 拆解 query = body（anchor）。
// 无锚时 Anchor 为空，Body 为原 query。
func parseQueryStruct(query string) queryStruct {
	q := strings.TrimSpace(query)
	ms := anchorV2Re.FindStringSubmatch(q)
	if ms == nil {
		return queryStruct{Body: q, Anchor: ""}
	}
	// body = 去掉尾部括号锚后的部分
	loc := anchorV2Re.FindStringIndex(q)
	body := strings.TrimSpace(q[:loc[0]])
	anchor := strings.TrimSpace(ms[1])
	return queryStruct{Body: body, Anchor: anchor}
}

// entitySplitRe 从 body 中抽出 entity（「X的Y」/「X把Y」/「X偏好Y」的 X 部分）。
// 规则：取第一个「的」/「把」/「偏好」/「给」之前的最短实体短语。
var entitySplitRe = regexp.MustCompile(`^(.+?)(的|把|偏好|给|准备)`)

// extractEntity 从 body 中抽取实体短语（用于 view_2 实体帧）。
func extractEntity(body string) string {
	m := entitySplitRe.FindStringSubmatch(body)
	if len(m) == 3 {
		return strings.TrimSpace(m[1])
	}
	// 兜底：取前 6 个字符
	r := []rune(body)
	if len(r) > 6 {
		r = r[:6]
	}
	return strings.TrimSpace(string(r))
}

// generateViewV1 槽值倒装：把锚前置为「锚：body」。
// 例：「客户A的偏好是什么（周一）」→「周一：客户A的偏好是什么」。
// 无锚时回退为原 body（不生成劣化视图）。
func generateViewV1(qs queryStruct) string {
	if qs.Anchor == "" {
		return qs.Body
	}
	return qs.Anchor + "：" + qs.Body
}

// generateViewV2 实体帧窄视：「entity（anchor）」。
// 例：「客户A的偏好是什么（周一）」→「客户A（周一）」。
// 无锚时回退为 entity 本身。
func generateViewV2(qs queryStruct) string {
	ent := extractEntity(qs.Body)
	if ent == "" {
		return qs.Body
	}
	if qs.Anchor != "" {
		return ent + "（" + qs.Anchor + "）"
	}
	return ent
}

// generateViewV3 候选回读：把 baseline top-K 候选的 Fact 作为回读 query 列表。
// 返回长度 = min(len(topFacts), readbackK)；topFacts 为空时返回空切片。
func generateViewV3(topFacts []string, readbackK int) []string {
	if readbackK <= 0 {
		readbackK = 1
	}
	var out []string
	for i, f := range topFacts {
		if i >= readbackK {
			break
		}
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ------------------------------------------------------------------
// v2 评测报告结构
// ------------------------------------------------------------------

// viewSet 单条 query 生成的视图集（名称 + 对应 query 字符串）。
type viewSet struct {
	Names   []string
	Queries []string
}

// MultiviewV2Report v2 多视图综合报告。
type MultiviewV2Report struct {
	Version        string            `json:"version"` // "v1" 结构视图 only / "v2" 含候选回读
	ViewDefs       []MultiviewDef    `json:"view_defs"`
	BaselineM1     M1Report          `json:"baseline_m1"`     // view_0 单视图
	PerView        []PerViewResult   `json:"per_view"`        // 每视图独立 M1
	OverlapMatrix  map[string]string `json:"overlap_matrix"`  // "i:j" -> top-K 重叠率
	IntegratedM1   M1Report          `json:"integrated_m1"`   // ConsensusVoter 集成结果
	DivergenceMean float64           `json:"divergence_mean"` // 逐 query 平均分歧度
	Improvement    float64           `json:"improvement_delta"`
	Pass           bool              `json:"pass"` // 集成 M1 ≥ baseline - 0.001
}

// ------------------------------------------------------------------
// 主入口：runMultiviewV2
// ------------------------------------------------------------------

// runMultiviewV2 R7-B v2 多视图集成评测。
//   - version="v1"：仅 view_1（槽值倒装）+ view_2（实体帧）两个结构视图
//   - version="v2"：在 v1 基础上加 view_3（候选回读，top-1 fact 作为回读 query）
//   - viewMode="baseline"：只跑 view_0 单视图 M1（不集成）
//   - viewMode="integrated"：跑全部视图 + ConsensusVoter 集成
func runMultiviewV2(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion,
	holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string,
	topK int, compressed bool, dir, prefix, version, viewMode string) {

	log.Printf("[multiview-v2] 版本=%s view=%s 开始", version, viewMode)

	// ---- 第一步：跑 view_0（原 query）拿 baseline top-K + M1 ----
	// 同时收集每条 query 的 baseline top-K MemoryID 与 Fact（供 view_3 回读）。
	type baselineRun struct {
		M1      M1Report
		Details []PolarisDetail
		// per-id：baseline top-K 候选
		topIDs   map[string][]string
		topFacts map[string][]string
	}
	bl := runBaselinePassV2(ctx, items, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_baseline")

	if viewMode == "baseline" {
		// 只输出基线 M1，不跑集成
		rep := MultiviewV2Report{
			Version:    version,
			BaselineM1: bl.M1,
			Pass:       true, // 基线模式不做集成判定
		}
		writeJSONReport(dir, prefix+"_mv2_baseline", rep)
		log.Printf("[multiview-v2] 基线 M1=%.4f hit@1=%.4f", bl.M1.Coverage, bl.M1.HitAt1Rate)
		return
	}

	// ---- 第二步：生成 v2 视图集 ----
	// 对每条 query 生成 [view_0, view_1, view_2, (view_3)]
	// view_3 依赖 baseline top-1 fact。
	viewSets := map[string]viewSet{}
	for _, it := range items {
		if it.Query == "" {
			continue
		}
		qs := parseQueryStruct(it.Query)
		vs := viewSet{
			Names:   []string{"view_0"},
			Queries: []string{it.Query},
		}
		// view_1 槽值倒装
		v1q := generateViewV1(qs)
		if v1q != it.Query {
			vs.Names = append(vs.Names, "view_1_invert")
			vs.Queries = append(vs.Queries, v1q)
		}
		// view_2 实体帧
		v2q := generateViewV2(qs)
		if v2q != it.Query {
			vs.Names = append(vs.Names, "view_2_entity")
			vs.Queries = append(vs.Queries, v2q)
		}
		// view_3 候选回读（仅 v2 版本）
		if version == "v2" {
			if facts, ok := bl.TopFacts[it.ID]; ok && len(facts) > 0 {
				rb := generateViewV3(facts, 1)
				for _, q := range rb {
					vs.Names = append(vs.Names, "view_3_readback")
					vs.Queries = append(vs.Queries, q)
				}
			}
		}
		viewSets[it.ID] = vs
	}

	// 视图定义（用第一条非空 query 的视图数为准）
	viewDefs := []MultiviewDef{}
	for _, it := range items {
		if vs, ok := viewSets[it.ID]; ok {
			for i, name := range vs.Names {
				tpl := ""
				if i < len(vs.Queries) {
					tpl = vs.Queries[i]
				}
				viewDefs = append(viewDefs, MultiviewDef{Index: i, Name: name, Template: tpl})
			}
			break
		}
	}
	log.Printf("[multiview-v2] 视图数=%d（%v）", len(viewDefs), viewDefs)

	// ---- 第三步：逐视图跑独立 M1 ----
	// 为每个视图 idx 构造 items 并跑一次检索。
	perView := []PerViewResult{}
	viewTopIDs := map[string]map[string][]string{} // viewName -> itemID -> top-K IDs
	nViews := len(viewDefs)
	for idx := 0; idx < nViews; idx++ {
		vName := viewDefs[idx].Name
		viewItems := make([]HoldoutItem, len(items))
		for i, it := range items {
			vs := viewSets[it.ID]
			q := it.Query
			if idx < len(vs.Queries) {
				q = vs.Queries[idx]
			}
			it.Query = q
			viewItems[i] = it
		}
		m1, _, topIDsMap := runViewPassV2(ctx, viewItems, srv, rf, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, fmt.Sprintf("%s_view_%d", prefix, idx))
		perView = append(perView, PerViewResult{
			Index: idx, Name: vName,
			M1Coverage: m1.Coverage, HitAt1Rate: m1.HitAt1Rate,
		})
		viewTopIDs[vName] = topIDsMap
		log.Printf("[multiview-v2] %s M1=%.4f hit@1=%.4f", vName, m1.Coverage, m1.HitAt1Rate)
	}

	// ---- 第四步：视图间重叠率（互补性度量） ----
	overlapMatrix := map[string]string{}
	{
		names := make([]string, nViews)
		for i, d := range viewDefs {
			names[i] = d.Name
		}
		for i := 0; i < nViews; i++ {
			for j := i + 1; j < nViews; j++ {
				ni, nj := names[i], names[j]
				overlap := computeOverlapV2(viewTopIDs[ni], viewTopIDs[nj])
				key := ni + ":" + nj
				overlapMatrix[key] = fmt.Sprintf("%.4f", overlap)
				log.Printf("[multiview-v2] 重叠率 %s = %.4f", key, overlap)
			}
		}
	}

	// ---- 第五步：ConsensusVoter 集成 ----
	integratedM1, integratedDetails, divMean := runIntegrateV2(ctx, items, srv, rf, viewSets, viewDefs, holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed, topK, compressed, dir, prefix+"_integrated")

	improvement := integratedM1.Coverage - bl.M1.Coverage
	pass := integratedM1.Coverage >= bl.M1.Coverage-0.001

	rep := MultiviewV2Report{
		Version:        version,
		ViewDefs:       viewDefs,
		BaselineM1:     bl.M1,
		PerView:        perView,
		OverlapMatrix:  overlapMatrix,
		IntegratedM1:   integratedM1,
		DivergenceMean: divMean,
		Improvement:    improvement,
		Pass:           pass,
	}
	writeJSONReport(dir, prefix+"_mv2_integrated", rep)
	log.Printf("[multiview-v2] 基线 M1=%.4f 集成 M1=%.4f delta=%.4f 平均分歧=%.4f %s",
		bl.M1.Coverage, integratedM1.Coverage, improvement, divMean, passFail(pass))

	writeV2Details(dir, prefix, integratedDetails)
}

// ------------------------------------------------------------------
// 子步骤实现
// ------------------------------------------------------------------

// runBaselinePassV2 跑 view_0 基线，返回 M1 + 每 query 的 top-K IDs/Facts。
func runBaselinePassV2(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion,
	holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string,
	topK int, compressed bool, dir, prefix string) struct {
	M1       M1Report
	Details  []PolarisDetail
	TopIDs   map[string][]string
	TopFacts map[string][]string
} {

	m1 := NewM1Accumulator(topK)
	var details []PolarisDetail
	topIDsMap := map[string][]string{}
	topFactsMap := map[string][]string{}

	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			continue
		}
		usr := it.User
		if usr == "" {
			usr = "eval-user"
		}
		materializeV2(ctx, srv, it, usr)

		query := it.Query
		if compressed {
			query = CompressQuery(query)
		}
		rc := kernel.RouterContext{
			Text: query, TenantID: it.Tenant, UserID: usr, TopK: polarisRecallTopK,
		}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			continue
		}
		var topFacts, topIDs []string
		for i := 0; i < topK && i < len(res.Fused); i++ {
			topFacts = append(topFacts, res.Fused[i].Fact)
			topIDs = append(topIDs, res.Fused[i].MemoryID)
		}
		topIDsMap[it.ID] = topIDs
		topFactsMap[it.ID] = topFacts

		var anchor *time.Time
		if len(res.Fused) > 0 && res.Fused[0].Memory != nil {
			anchor = res.Fused[0].Memory.OccurredAt
		}
		expected := it.effectiveExpected()
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, topK, anchor)
		trial.Query = it.Query
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)
		details = append(details, PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query, Expected: expected,
			Hit: trial.Hit, HitAt1: trial.HitAt1, FirstRank: trial.FirstRank,
			TopFact: topFactsAt(topFacts, 0),
		})
	}

	return struct {
		M1       M1Report
		Details  []PolarisDetail
		TopIDs   map[string][]string
		TopFacts map[string][]string
	}{
		M1: m1.Report(), Details: details, TopIDs: topIDsMap, TopFacts: topFactsMap,
	}
}

// runViewPassV2 跑单个视图 idx，返回 M1 + topIDs 映射。
func runViewPassV2(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion,
	holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string,
	topK int, compressed bool, dir, prefix string) (M1Report, []PolarisDetail, map[string][]string) {

	m1 := NewM1Accumulator(topK)
	var details []PolarisDetail
	topIDsMap := map[string][]string{}

	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			continue
		}
		usr := it.User
		if usr == "" {
			usr = "eval-user"
		}
		materializeV2(ctx, srv, it, usr)

		query := it.Query
		if compressed {
			query = CompressQuery(query)
		}
		rc := kernel.RouterContext{
			Text: query, TenantID: it.Tenant, UserID: usr, TopK: polarisRecallTopK,
		}
		if it.AsOf != "" {
			asOf := it.AsOf
			rc.AsOf = &asOf
		}
		res, rerr := srv.RecallAndFuse(ctx, rc)
		if rerr != nil {
			continue
		}
		var topFacts, topIDs []string
		for i := 0; i < topK && i < len(res.Fused); i++ {
			topFacts = append(topFacts, res.Fused[i].Fact)
			topIDs = append(topIDs, res.Fused[i].MemoryID)
		}
		topIDsMap[it.ID] = topIDs

		var anchor *time.Time
		if len(res.Fused) > 0 && res.Fused[0].Memory != nil {
			anchor = res.Fused[0].Memory.OccurredAt
		}
		expected := it.effectiveExpected()
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, topK, anchor)
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)
		details = append(details, PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query, Expected: expected,
			Hit: trial.Hit, HitAt1: trial.HitAt1, FirstRank: trial.FirstRank,
			TopFact: topFactsAt(topFacts, 0),
		})
	}
	return m1.Report(), details, topIDsMap
}

// runIntegrateV2 用 ConsensusVoter 对每条 query 的多视图 top-K 做集成。
func runIntegrateV2(ctx context.Context, items []HoldoutItem, srv *kernel.APIServer, rf *kernel.RankFusion,
	viewSets map[string]viewSet, viewDefs []MultiviewDef,
	holdoutPath, weightsPath, embedderMode, embedderName, vocabPath, nowFixed string,
	topK int, compressed bool, dir, prefix string) (M1Report, []PolarisDetail, float64) {

	m1 := NewM1Accumulator(topK)
	var details []PolarisDetail
	voter := &kernel.ConsensusVoter{K: topK}
	divSum := 0.0
	divN := 0

	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			continue
		}
		usr := it.User
		if usr == "" {
			usr = "eval-user"
		}
		materializeV2(ctx, srv, it, usr)

		vs, ok := viewSets[it.ID]
		if !ok || len(vs.Queries) == 0 {
			continue
		}

		// 逐视图检索，收集 Interpretation
		interps := []kernel.Interpretation{}
		factByID := map[string]string{}
		for vi, vq := range vs.Queries {
			query := vq
			if compressed {
				query = CompressQuery(query)
			}
			rc := kernel.RouterContext{
				Text: query, TenantID: it.Tenant, UserID: usr, TopK: polarisRecallTopK,
			}
			if it.AsOf != "" {
				asOf := it.AsOf
				rc.AsOf = &asOf
			}
			res, rerr := srv.RecallAndFuse(ctx, rc)
			if rerr != nil {
				continue
			}
			var topIDs []string
			for i := 0; i < topK && i < len(res.Fused); i++ {
				topIDs = append(topIDs, res.Fused[i].MemoryID)
				factByID[res.Fused[i].MemoryID] = res.Fused[i].Fact
			}
			srcName := fmt.Sprintf("view_%d", vi)
			if vi < len(vs.Names) {
				srcName = vs.Names[vi]
			}
			// view_0（原 query）权重 1.0；其他视图权重 0.8（结构重排视图信度略低）
			w := 1.0
			if vi > 0 {
				w = 0.8
			}
			interps = append(interps, kernel.Interpretation{
				Source: srcName, TopIDs: topIDs, Weight: w,
			})
		}

		if len(interps) == 0 {
			continue
		}
		cons := voter.Vote(interps)
		divSum += cons.Divergence
		divN++

		// 取 consensus ranking 前 topK 作为最终结果
		var topIDs, topFacts []string
		for i, id := range cons.Ranking {
			if i >= topK {
				break
			}
			topIDs = append(topIDs, id)
			topFacts = append(topFacts, factByID[id])
		}

		var anchor *time.Time
		if it.Memory != nil {
			anchor = it.Memory.OccurredAt
		}
		expected := it.effectiveExpected()
		trial := JudgeM1(expected, it.ExpectedMemoryIDs, topFacts, topIDs, topK, anchor)
		trial.Query = it.Query
		trial.Bucket = it.Bucket
		m1.Add(it.Bucket, trial)
		details = append(details, PolarisDetail{
			ID: it.ID, Bucket: it.Bucket, Query: it.Query, Expected: expected,
			Hit: trial.Hit, HitAt1: trial.HitAt1, FirstRank: trial.FirstRank,
			TopFact: topFactsAt(topFacts, 0),
		})
	}

	divMean := 0.0
	if divN > 0 {
		divMean = divSum / float64(divN)
	}
	return m1.Report(), details, divMean
}

// ------------------------------------------------------------------
// 辅助
// ------------------------------------------------------------------

func materializeV2(ctx context.Context, srv *kernel.APIServer, it HoldoutItem, usr string) {
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
}

func topFactsAt(facts []string, i int) string {
	if i >= 0 && i < len(facts) {
		return facts[i]
	}
	return ""
}

// computeOverlapV2 计算两视图 top-K 候选集合的 Jaccard 重叠率。
func computeOverlapV2(a, b map[string][]string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersect := 0
	union := 0
	// 遍历 a 的 key（两视图应覆盖相同 item 集；以 a 为主）
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 确定性
	for _, k := range keys {
		sa := toSet(a[k])
		sb := toSet(b[k])
		for id := range sa {
			if sb[id] {
				intersect++
			}
			union++
		}
		for id := range sb {
			if !sa[id] {
				union++
			}
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

func toSet(ids []string) map[string]bool {
	s := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			s[id] = true
		}
	}
	return s
}

// writeV2Details 写 v2 集成明细。
func writeV2Details(dir, prefix string, details []PolarisDetail) {
	path := filepath.Join(dir, fmt.Sprintf("%s_mv2_details_%s.json", prefix, time.Now().UTC().Format("20060102-150405")))
	type D struct {
		ID        string `json:"id"`
		Bucket    string `json:"bucket"`
		Query     string `json:"query"`
		Expected  string `json:"expected"`
		Hit       bool   `json:"hit"`
		HitAt1    bool   `json:"hit_at_1"`
		FirstRank int    `json:"first_rank"`
		TopFact   string `json:"top_fact"`
	}
	var out []D
	for _, d := range details {
		out = append(out, D{
			ID: d.ID, Bucket: d.Bucket, Query: d.Query, Expected: d.Expected,
			Hit: d.Hit, HitAt1: d.HitAt1, FirstRank: d.FirstRank, TopFact: d.TopFact,
		})
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(path, raw, 0o644)
	log.Printf("[multiview-v2] 明细 %d 条 -> %s", len(out), path)
}
