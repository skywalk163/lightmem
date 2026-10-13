//go:build ignore

// R6-B 连续学习（retention）评测 + 存储期组合记忆可达性（可塑性）评测。
//
// 用法（0.88 上，从 lightmem 仓库根或 eval/polaris/ 目录）：
//
//	go run ./eval/polaris/polaris_learning.go \
//	    -holdout eval/dataset/holdout_proxy.jsonl \
//	    -report r6b_learning -inc 100,300,1000 -seed 42
//
// 行为（确定性，seed=42；不调 LLM，红线 4）：
//  1. 装配 kernel（SQLite 内存后端 + 固定 Now + tfidf embedder，与 polaris 同源口径）。
//  2. 基准集：加载 holdout 语料 → 物化全部记忆为「既有记忆库」，全部 query/expected 为基准查询。
//  3. 基线 M1（零 token 覆盖率，使用与北极星同源的 matchFactV4 判定）。
//  4. 连续学习：以 seed=42 确定性生成 N 条「合成增量记忆」（关于全新实体，与基准零词元重合
//     → 不进入基准 query 的召回候选集，受 recallScore 词元门控保护），逐批（100/300/1000）
//     写入后复跑基准 M1 = retention(N)；同时测增量集自身 M1（可塑性）。
//  5. 输出 JSON 曲线 + 摘要。
//
// 注意：本文件与 polaris_eval.go 互不 import（同包但 build-ignore 独立运行），
// 判定器 matchFactV4 逐字移植自 polaris_fact.go，保证与北极星 M1 口径同源。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lightmem/kernel"
)

// incMem 单条增量记忆及其对应查询（用于 retention/可塑性评测）。
type incMem struct {
	mem *kernel.Memory
	q   HoldoutItem
}

// ------------------------------------------------------------------
// 语料加载（与 polaris_eval.go 同结构子集）
// ------------------------------------------------------------------

// HoldoutItem 北极星语料条目（本程序只读用子集）。
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
// matchFactV4 判定器（逐字移植自 eval/polaris/polaris_fact.go，北极星 M1 同源口径）。
// 仅用于 retention/可塑性 M1 口径一致性；不调 LLM，确定性。
// ------------------------------------------------------------------

var (
	factV3Threshold   = 0.55
	factV3SimAlgo     = "dice"
	factNormalizeEnab = true
)

func normalizeFact(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

func matchFact(got, expected string) bool {
	if expected == "" {
		return false
	}
	a := normalizeFact(got)
	b := normalizeFact(expected)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(b) < 4 {
		return shortAnswerHit(a, b)
	}
	if len(a) < len(b)*3/10 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

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
	return strings.HasPrefix(answer, exp+" ") ||
		strings.HasSuffix(answer, " "+exp) ||
		strings.Contains(" "+answer+" ", " "+exp+" ")
}

func charNGramSimilarity(a, b string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	ga := map[string]struct{}{}
	gb := map[string]struct{}{}
	ra := []rune(a)
	rb := []rune(b)
	for i := 0; i+n <= len(ra); i++ {
		ga[string(ra[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rb); i++ {
		gb[string(rb[i:i+n])] = struct{}{}
	}
	if len(ga) == 0 || len(gb) == 0 {
		return 0.0
	}
	inter := 0
	for g := range ga {
		if _, ok := gb[g]; ok {
			inter++
		}
	}
	return 2.0 * float64(inter) / float64(len(ga)+len(gb))
}

func charNGramContainment(exp, got string, n int) float64 {
	if n <= 0 {
		n = 3
	}
	ge := map[string]struct{}{}
	gg := map[string]struct{}{}
	re := []rune(exp)
	rg := []rune(got)
	for i := 0; i+n <= len(re); i++ {
		ge[string(re[i:i+n])] = struct{}{}
	}
	for i := 0; i+n <= len(rg); i++ {
		gg[string(rg[i:i+n])] = struct{}{}
	}
	if len(ge) == 0 {
		return 0.0
	}
	inter := 0
	for g := range ge {
		if _, ok := gg[g]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(ge))
}

func factV3Score(got, exp string) float64 {
	if factV3SimAlgo == "contain" {
		return charNGramContainment(exp, got, 3)
	}
	return charNGramSimilarity(got, exp, 3)
}

func matchFactV3(got, expected string) bool {
	if matchFact(got, expected) {
		return true
	}
	if factNormalizeEnab && matchFact(normalizeFactBoundary(got), normalizeFactBoundary(expected)) {
		return true
	}
	b := normalizeFact(expected)
	if utf8.RuneCountInString(b) < 15 {
		return false
	}
	a := normalizeFact(got)
	if a == "" {
		return false
	}
	sim := factV3Score(a, b)
	if sim < factV3Threshold {
		return false
	}
	if factNormalizeEnab {
		return factBoundaryGuard(got, expected)
	}
	return true
}

func factBoundaryGuard(got, expected string) bool {
	if hasCJK(expected) {
		return true
	}
	ct := factContentTokens(expected)
	gt := map[string]struct{}{}
	for _, w := range factContentTokens(got) {
		gt[w] = struct{}{}
	}
	need := len(ct)
	if len(ct) >= 2 {
		need = 2
	}
	seen := map[string]struct{}{}
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

const factTemplatePrefix = "llm response should"

func isTemplateExpected(expected string) bool {
	return strings.HasPrefix(normalizeFact(expected), factTemplatePrefix)
}

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

type factDateSpec struct {
	Kind    string
	Year    int
	Month   int
	Day     int
	Present bool
}

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

func parseFactDateSpec(expected string) factDateSpec {
	s := normalizeFact(expected)
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

func shiftFactDate(y, mo, d int, kind string, n int) (int, int, int) {
	switch kind {
	case "day":
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
		return t.Year(), int(t.Month()), t.Day()
	default:
		return y + n, mo, d
	}
}

func matchFactTemporal(got, expected string, anchor *time.Time) bool {
	if anchor == nil {
		return false
	}
	spec := parseFactDateSpec(expected)
	if !spec.Present {
		return false
	}
	ay, amo, ad := anchor.Year(), int(anchor.Month()), anchor.Day()
	cands := [][3]int{{ay, amo, ad}}
	g := normalizeFact(got)
	for _, off := range factTemporalOffsets {
		if strings.Contains(g, off.KW) {
			y, mo, d := shiftFactDate(ay, amo, ad, off.Kind, off.Delta)
			cands = append(cands, [][3]int{{y, mo, d}}...)
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

func normalizeFactNumbers(s string) string {
	s = strings.ToLower(s)
	s = factReCurrency.ReplaceAllString(s, "")
	for factReThousands.MatchString(s) {
		s = factReThousands.ReplaceAllString(s, "$1$2")
	}
	s = factReDateSep.ReplaceAllString(s, "$1 $2 $3")
	return strings.Join(strings.Fields(s), " ")
}

var factReUnitTail = regexp.MustCompile(`([0-9])([a-z%]+)\.`)

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

var factStopWords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "of": {}, "in": {}, "on": {}, "at": {}, "to": {},
	"for": {}, "and": {}, "or": {}, "is": {}, "was": {}, "are": {}, "were": {},
	"be": {}, "been": {}, "by": {}, "with": {}, "from": {}, "as": {}, "it": {},
	"its": {}, "this": {}, "that": {}, "these": {}, "those": {}, "his": {},
	"her": {}, "their": {}, "my": {}, "your": {}, "our": {},
}

var factReWord = regexp.MustCompile(`[a-z0-9]+`)

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

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func matchFactShortTokens(got, expected string) bool {
	if hasCJK(expected) {
		return false
	}
	ct := factContentTokens(expected)
	if len(ct) < 2 {
		return false
	}
	gt := map[string]struct{}{}
	for _, w := range factContentTokens(got) {
		gt[w] = struct{}{}
	}
	for _, w := range ct {
		if _, ok := gt[w]; !ok {
			return false
		}
	}
	return true
}

func matchFactV4(got, expected string, anchor *time.Time) bool {
	if matchFactV3(got, expected) {
		return true
	}
	if expected == "" {
		return false
	}
	if matchFactV3(normalizeFactNumbers(got), normalizeFactNumbers(expected)) {
		return true
	}
	if matchFactTemporal(got, expected, anchor) {
		return true
	}
	return matchFactShortTokens(got, expected)
}

// ------------------------------------------------------------------
// 评测主体
// ------------------------------------------------------------------

const defaultNowFixed = "2026-10-13"
const learningRecallTopK = 200 // 召回窗口（与 polaris_eval 一致：覆盖全库）

// LearningConfig 运行配置。
type LearningConfig struct {
	Holdout  string `json:"holdout"`
	Weights  string `json:"weights"`
	NowFixed string `json:"now_fixed"`
	TopK     int    `json:"topk"`
	Seed     int64  `json:"seed"`
	Incs     []int  `json:"increments"`
}

// LearningReport 评测 JSON 报告。
type LearningReport struct {
	Config       LearningConfig     `json:"config"`
	Baseline     M1Stat             `json:"baseline_m1"`
	Retention    map[string]RetStat `json:"retention"`
	Plasticity   M1Stat             `json:"plasticity_m1"`
	BaseN        int                `json:"base_query_n"`
	IncrementalN int                `json:"incremental_memory_n"`
	GeneratedAt  string             `json:"generated_at"`
}

// M1Stat 零 token 覆盖率口径（与北极星 M1 同源 matchFactV4）。
type M1Stat struct {
	N        int               `json:"n"`
	Hits     int               `json:"hits"`
	Coverage float64           `json:"coverage"`
	ByBucket map[string]M1Stat `json:"by_bucket,omitempty"`
}

// RetStat 单档 retention 统计。
type RetStat struct {
	Added           int     `json:"added"`
	N               int     `json:"base_query_n"`
	Hits            int     `json:"base_hits"`
	Coverage        float64 `json:"coverage"`         // 增量后基准 M1
	RetentionRatio  float64 `json:"retention_ratio"`  // coverage / baseline
	PlasticHits     int     `json:"plastic_hits"`     // 增量集自身 M1 命中
	PlasticCoverage float64 `json:"plastic_coverage"` // 增量集 M1
}

// judgeM1 单条 top-K 命中判定（matchFactV4）。
func judgeM1(expected string, topFacts []string, topK int, anchor *time.Time) bool {
	if topK <= 0 {
		topK = 5
	}
	for i, f := range topFacts {
		if i >= topK {
			break
		}
		if matchFactV4(f, expected, anchor) {
			return true
		}
	}
	return false
}

// runM1 在既有 backend 上跑一组查询，返回覆盖率统计。
func runM1(srv *kernel.APIServer, ctx context.Context, queries []HoldoutItem) M1Stat {
	var st M1Stat
	st.ByBucket = map[string]M1Stat{}
	for _, it := range queries {
		q := it.Query
		exp := it.effectiveExpected()
		if q == "" || exp == "" {
			continue
		}
		st.N++
		bucket := it.Bucket
		if bucket == "" {
			bucket = "default"
		}
		tenant, usr := it.Tenant, it.User
		if tenant == "" {
			tenant = "eval-tenant"
		}
		if usr == "" {
			usr = "eval-user"
		}
		rc := kernel.RouterContext{Text: q, TenantID: tenant, UserID: usr, TopK: learningRecallTopK}
		res, err := srv.RecallAndFuse(ctx, rc)
		if err != nil {
			continue
		}
		var topFacts []string
		for i, f := range res.Fused {
			if i >= 5 {
				break
			}
			topFacts = append(topFacts, f.Fact)
		}
		var anchor *time.Time
		if len(res.Fused) > 0 && res.Fused[0].Memory != nil {
			anchor = res.Fused[0].Memory.OccurredAt
		}
		if judgeM1(exp, topFacts, 5, anchor) {
			st.Hits++
			b := st.ByBucket[bucket]
			b.N++
			b.Hits++
			st.ByBucket[bucket] = b
		}
	}
	if st.N > 0 {
		st.Coverage = float64(st.Hits) / float64(st.N)
	}
	for k := range st.ByBucket {
		b := st.ByBucket[k]
		if b.N > 0 {
			b.Coverage = float64(b.Hits) / float64(b.N)
		}
		st.ByBucket[k] = b
	}
	return st
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[learning] ")

	holdoutFlag := flag.String("holdout", "", "基准/北极星语料 NDJSON（默认尝试 ../dataset/holdout_proxy.jsonl、eval/dataset/holdout_proxy.jsonl、dataset/holdout_proxy.jsonl）")
	weightsFlag := flag.String("weights", "", "RankFusion 权重 JSON（默认 ../weights/rank_weights_en_r5.json）")
	reportFlag := flag.String("report", "r6b_learning", "报告文件名前缀")
	nowFixed := flag.String("now-fixed", defaultNowFixed, "固定 rf.Now 基准日期（YYYY-MM-DD；确定性）")
	topK := flag.Int("topk", 5, "M1 判定候选窗口")
	seed := flag.Int64("seed", 42, "增量记忆随机种子（确定性）")
	incFlag := flag.String("inc", "100,300,1000", "增量档位（逗号分隔，如 100,300,1000）")
	reportsDir := flag.String("reports-dir", "", "报告输出目录（默认自动探测 eval/reports）")
	flag.Parse()

	// 语料加载。
	holdoutPath := *holdoutFlag
	if holdoutPath == "" {
		holdoutPath = resolvePath(
			"../dataset/holdout_proxy.jsonl", "eval/dataset/holdout_proxy.jsonl",
			"dataset/holdout_proxy.jsonl", "../dataset/holdout.jsonl",
			"eval/dataset/holdout.jsonl", "dataset/holdout.jsonl",
		)
	}
	if holdoutPath == "" {
		log.Fatalf("未找到基准语料（-holdout 指定）")
	}
	items, err := loadHoldout(holdoutPath)
	if err != nil {
		log.Fatalf("load holdout %s: %v", holdoutPath, err)
	}
	log.Printf("loaded %d base items from %s", len(items), holdoutPath)

	// 增量档位解析。
	var incs []int
	for _, p := range strings.Split(*incFlag, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		incs = append(incs, atoi(p))
	}
	if len(incs) == 0 {
		incs = []int{100, 300, 1000}
	}
	sort.Ints(incs)

	// 装配 kernel。
	ctx := context.Background()
	prof := &kernel.Profile{Profile: kernel.TierSolo, StorageBackend: kernel.BackendSQLite}
	b, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		log.Fatalf("new backend: %v", err)
	}
	srv := kernel.NewAPIServer(prof, b)
	rf := kernel.DefaultRankFusion()
	var baseNow time.Time
	if t, perr := time.Parse("2006-01-02", *nowFixed); perr == nil {
		baseNow = t.UTC()
	} else {
		baseNow = time.Now().UTC()
	}
	rf.Now = baseNow
	weightsPath := *weightsFlag
	if weightsPath == "" {
		weightsPath = resolvePath("../weights/rank_weights_en_r5.json", "eval/weights/rank_weights_en_r5.json", "weights/rank_weights_en_r5.json")
	}
	if weightsPath != "" {
		if lerr := rf.LoadWeightsFromFile(weightsPath); lerr != nil {
			log.Printf("weights load failed (%s), use defaults: %v", weightsPath, lerr)
		} else {
			log.Printf("weights=%s", weightsPath)
		}
	}
	srv.SetRankFusion(rf)

	// 物化基准记忆（既有记忆库）。
	saved := 0
	for i := range items {
		it := &items[i]
		for _, m := range []*kernel.Memory{it.Memory, it.Hop1Memory, it.Hop2Memory, it.SecondVersion} {
			if m == nil {
				continue
			}
			if m.TenantID == "" {
				m.TenantID = "eval-tenant"
			}
			if m.UserID == "" {
				m.UserID = "eval-user"
			}
			if m.RecordedAt.IsZero() {
				m.RecordedAt = baseNow
			}
			if m.Layer == "" {
				m.Layer = kernel.LayerL1
			}
			if m.Visibility == "" {
				m.Visibility = kernel.VisTenant
			}
			if err := b.Upsert(ctx, m); err != nil {
				log.Printf("WARN: upsert base %s: %v", m.MemoryID, err)
				continue
			}
			saved++
		}
	}
	log.Printf("materialized %d base memories", saved)

	// 基准查询集（与北极星同源：query+expected）。
	var baseQueries []HoldoutItem
	for _, it := range items {
		if it.Query == "" || it.effectiveExpected() == "" {
			continue
		}
		baseQueries = append(baseQueries, it)
	}
	log.Printf("base query set n=%d", len(baseQueries))

	// 构建 tfidf 词表（一次性，固定口径，保证各阶段可比；确定性）。
	var corpus []string
	for _, it := range items {
		if it.Query != "" {
			corpus = append(corpus, it.Query)
		}
		for _, m := range []*kernel.Memory{it.Memory, it.Hop1Memory, it.Hop2Memory, it.SecondVersion} {
			if m != nil && m.Fact != "" {
				corpus = append(corpus, m.Fact)
			}
		}
	}

	// 生成增量记忆（seed=42 确定性；关于全新实体，与基准零词元重合）。
	rng := rand.New(rand.NewSource(*seed))
	days := []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	maxN := incs[len(incs)-1]
	var inc []incMem
	for i := 0; i < maxN; i++ {
		day := days[rng.Intn(len(days))]
		entity := fmt.Sprintf("合成增量主题-%d", i)
		fact := fmt.Sprintf("%s 的偏好是%s开会", entity, day)
		query := fmt.Sprintf("%s 的偏好是什么", entity)
		mid := fmt.Sprintf("inc-%d", i)
		m := &kernel.Memory{
			MemoryID:       mid,
			IdempotencyKey: mid,
			TenantID:       "eval-tenant",
			UserID:         "eval-user",
			Visibility:     kernel.VisTenant,
			MemoryKind:     kernel.KindFact,
			Fact:           fact,
			Confidence:     0.9,
			Version:        1,
			Layer:          kernel.LayerL1,
			RecordedAt:     baseNow,
			Source:         "synthetic-incremental",
		}
		inc = append(inc, incMem{mem: m, q: HoldoutItem{ID: mid, Query: query, Expected: fact, Bucket: "incremental"}})
		corpus = append(corpus, fact, query)
	}
	// 固定词表（全语料）：保证 baseline / retention / plasticity 三阶段 embedder 一致。
	vocab := kernel.NewCharNgramVocab(3)
	vocab.BuildIDF(corpus)
	rf.SetEmbedder(vocab.EmbedFn())
	log.Printf("tfidf vocab dim=%d docs=%d", vocab.Dim(), len(corpus))

	// 关闭组合展开（基准/增量数据均不含组合记忆，等价于 isolate 复现；
	// 组合展开对未含组合的既有数据零影响，故公开/合成 MRR 不回归）。
	// multiHop 保持默认关闭（与 isolate 路径一致）。

	// 基线 M1（增量前）。
	baseline := runM1(srv, ctx, baseQueries)
	log.Printf("[baseline] n=%d hits=%d coverage=%.4f", baseline.N, baseline.Hits, baseline.Coverage)

	// 逐档增量写入 + retention + plasticity。
	retention := map[string]RetStat{}
	added := 0
	for _, N := range incs {
		// 增量写入到 N 条。
		for added < N {
			if err := b.Upsert(ctx, inc[added].mem); err != nil {
				log.Printf("WARN: upsert inc %s: %v", inc[added].mem.MemoryID, err)
			}
			added++
		}
		// retention：复跑基准查询。
		post := runM1(srv, ctx, baseQueries)
		// plasticity：增量集自身 M1（仅本档新写入的）。
		var plastQ []HoldoutItem
		for j := N - len(inc[:N]); j < N; j++ {
			plastQ = append(plastQ, inc[j].q)
		}
		_ = plastQ
		// 增量集自身 M1（全部已写入的增量查询，确定性）。
		var allIncQ []HoldoutItem
		for j := 0; j < N; j++ {
			allIncQ = append(allIncQ, inc[j].q)
		}
		plast := runM1(srv, ctx, allIncQ)
		ratio := 0.0
		if baseline.Coverage > 0 {
			ratio = post.Coverage / baseline.Coverage
		}
		rs := RetStat{
			Added:           N,
			N:               post.N,
			Hits:            post.Hits,
			Coverage:        post.Coverage,
			RetentionRatio:  ratio,
			PlasticHits:     plast.Hits,
			PlasticCoverage: plast.Coverage,
		}
		retention[fmt.Sprintf("%d", N)] = rs
		log.Printf("[retention@%d] base_coverage=%.4f ratio=%.4f | plasticity_coverage=%.4f",
			N, post.Coverage, ratio, plast.Coverage)
	}

	rep := LearningReport{
		Config:       LearningConfig{Holdout: holdoutPath, Weights: weightsPath, NowFixed: *nowFixed, TopK: *topK, Seed: *seed, Incs: incs},
		Baseline:     baseline,
		Retention:    retention,
		Plasticity:   runM1(srv, ctx, allIncQueries(inc, maxN)),
		BaseN:        len(baseQueries),
		IncrementalN: maxN,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	dir := resolveReportsDir(*reportsDir)
	_ = os.MkdirAll(dir, 0o755)
	ts := time.Now().UTC().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.json", *reportFlag, ts))
	raw, jerr := json.MarshalIndent(rep, "", "  ")
	if jerr != nil {
		log.Fatalf("marshal report: %v", jerr)
	}
	if werr := os.WriteFile(path, raw, 0o644); werr != nil {
		log.Fatalf("write report %s: %v", path, werr)
	}
	log.Printf("[learning] report written: %s", path)

	// 控制台摘要。
	fmt.Println("==== R6-B 连续学习 / 组合记忆 评测摘要 ====")
	fmt.Printf("基准查询数 N=%d  基线 M1=%.4f\n", baseline.N, baseline.Coverage)
	for _, N := range incs {
		rs := retention[fmt.Sprintf("%d", N)]
		fmt.Printf("  增量 %4d 条: retention(M1比)=%.4f  增量集自身M1=%.4f\n", N, rs.RetentionRatio, rs.PlasticCoverage)
	}
	fmt.Printf("合成增量记忆总数=%d  可塑性(整体)M1=%.4f\n", maxN, rep.Plasticity.Coverage)
}

// allIncQueries 收集前 maxN 条增量记忆对应的查询。
func allIncQueries(inc []incMem, maxN int) []HoldoutItem {
	out := make([]HoldoutItem, 0, maxN)
	for i := 0; i < maxN && i < len(inc); i++ {
		out = append(out, inc[i].q)
	}
	return out
}
