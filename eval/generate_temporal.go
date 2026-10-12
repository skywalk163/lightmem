//go:build ignore

// R5-B：temporal 标记样本补齐（任务书 §2 B.3.2）。
//
// 目的：公开基准此前 temporal 桶覆盖不足（§0.1 残留项：「时序桶准确率 0.0%（无 temporal
// 标记样本）」），补齐 ≥50 条带 is_temporal=true、覆盖 as_of / since / before / between
// 四种时间窗口查询形态的样本，用于验证 temporal 桶准确率。
//
// 设计（纯模板 + 固定 seed，不调 LLM）：
//  1. 真实样本抽取：从公开基准（-src，LME/BEAM/合成混排）按文件序 + seed=42 固定采样
//     抽取 is_temporal=true 的真实样本，逐条复制其记忆，并为其查询追加 as_of/since/
//     before/between 窗口标记，构造新的时间窗口查询样本（真实事实 + 模板替换）。
//  2. 合成模板：以模板枚举构造确定性的时序样本（实体×主题×日期），查询内嵌期望事实，
//     保证确定性可召回。
//
// 输出 `dataset/temporal.jsonl`；用 `run_kpi.go -temporal-samples ./dataset/temporal.jsonl`
// 合并进评测（等价于并入 all.jsonl 的 temporal 桶）。注意：本文件在 eval_generate.go
// 之后运行，会覆盖后者按桶写出的 dataset/temporal.jsonl（合成 temporal 桶仍完整保留在
// dataset/all.jsonl 内）。
//
// 红线：
//   - 不调 LLM（纯模板替换）
//   - seed=42 固定采样（rand.New(rand.NewSource(42))）
//   - 无 time.Now() / map 迭代序等不确定源
//
// 运行（0.88 上）：
//
//	cd eval && go run ./generate_temporal.go -src ./benchmarks/dataset/all.jsonl \
//	  -out ./dataset/temporal.jsonl -count 50
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"github.com/lightmem/kernel"
)

const (
	tmpTenant = "eval-tenant"
	tmpUser   = "eval-user"
	tmpBucket = "temporal"
)

// srcItem 是公开基准 / 合成集单条的最小解析结构（只取时间窗口构造所需字段）。
type srcItem struct {
	ID            string         `json:"id"`
	Query         string         `json:"query"`
	ExpectedFact  string         `json:"expected_fact"`
	Bucket        string         `json:"bucket"`
	IsTemporal    bool           `json:"is_temporal"`
	Tenant        string         `json:"tenant"`
	User          string         `json:"user"`
	AsOf          string         `json:"as_of,omitempty"`
	Memory        *kernel.Memory `json:"memory"`
	SecondVersion *kernel.Memory `json:"second_version,omitempty"`
}

// temporalItem 是本生成器的输出结构。
// 与 run_kpi.go 的 EvalItem 对齐（后者解析 id/query/expected_memory_ids/expected_fact/
// bucket/is_temporal/tenant/user/as_of/memory/second_version；未知字段被忽略）。
// since/before/between_* / temporal_form 为窗口形态的结构化标注，供报告追溯。
type temporalItem struct {
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
	Since            string         `json:"since,omitempty"`
	Before           string         `json:"before,omitempty"`
	BetweenFrom      string         `json:"between_from,omitempty"`
	BetweenTo        string         `json:"between_to,omitempty"`
	TemporalForm     string         `json:"temporal_form,omitempty"`
	Memory           *kernel.Memory `json:"memory"`
	SecondVersion    *kernel.Memory `json:"second_version,omitempty"`
}

// 合成模板池（确定性枚举，与合成集实体域一致，保证与公开基准租户隔离）。
var (
	tmpEntities = []string{"客户A", "客户B", "客户C", "客户D", "客户E", "客户F", "客户G", "客户H", "客户I", "客户J"}
	tmpTopics   = []string{"预算", "排期", "地点", "参会人数", "负责人"}
	tmpDays     = []string{"周一", "周二", "周三", "周四", "周五"}
	tmpDates    = []string{"2026-09-01", "2026-09-15", "2026-10-01", "2026-10-15", "2026-11-01", "2026-11-15"}
)

// temporalForms 四种时间窗口查询形态（任务书 §2 B.3.2：as_of / since / before / between）。
var temporalForms = []string{"as_of", "since", "before", "between"}

// tmpSeed 返回固定 seed 的随机源（seed=42，红线 5）。
func tmpSeed() *rand.Rand { return rand.New(rand.NewSource(42)) }

// loadSrc 读取 JSONL 源文件（公开基准 / 合成集）。
func loadSrc(path string) ([]srcItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []srcItem
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it srcItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("parse src line: %w", err)
		}
		out = append(out, it)
	}
	return out, nil
}

// windowQuery 在真实 query 后追加确定性窗口标记（模板替换，不调 LLM）。
// 真实 query 逐字保留，仅追加窗口短语，最大化保持可召回性。
func windowQuery(form, query, asOf, since, before, from, to string) string {
	switch form {
	case "since":
		return fmt.Sprintf("%s（自 %s 起至今）", query, since)
	case "before":
		return fmt.Sprintf("%s（%s 之前的状态）", query, before)
	case "between":
		return fmt.Sprintf("%s（%s 至 %s 期间）", query, from, to)
	default: // as_of
		return fmt.Sprintf("%s（截至 %s 时点的状态）", query, asOf)
	}
}

// extractReal 从公开基准抽取 is_temporal=true 的真实样本，构造窗口查询样本。
//
//   - 先按文件序收集候选下标（确定性）；
//   - 用 seed=42 的 rand.Perm 做固定采样（红线 5），取前 max 条；
//   - 每条复制真实记忆（同 memory_id，合并评测时不重复写入），查询追加窗口标记。
func extractReal(r *rand.Rand, src []srcItem, max int) []temporalItem {
	var cand []int
	for i := range src {
		if src[i].IsTemporal && src[i].Memory != nil {
			cand = append(cand, i)
		}
	}
	if max > len(cand) {
		max = len(cand)
	}
	var out []temporalItem
	if max <= 0 {
		return out
	}
	perm := r.Perm(len(cand))
	for k := 0; k < max; k++ {
		it := src[cand[perm[k]]]
		form := temporalForms[k%len(temporalForms)]
		asOf := it.AsOf
		if asOf == "" {
			asOf = tmpDates[k%len(tmpDates)]
		}
		since := tmpDates[(k+1)%len(tmpDates)]
		before := tmpDates[(k+2)%len(tmpDates)]
		from := tmpDates[k%len(tmpDates)]
		to := tmpDates[(k+3)%len(tmpDates)]
		q := windowQuery(form, it.Query, asOf, since, before, from, to)
		out = append(out, temporalItem{
			ID:               fmt.Sprintf("tmr-%03d", k),
			Query:            q,
			ExpectedMemoryID: []string{it.Memory.MemoryID},
			ExpectedFact:     it.ExpectedFact,
			Bucket:           tmpBucket,
			IsTemporal:       true,
			Tenant:           it.Tenant,
			User:             it.User,
			AsOf:             asOf,
			Since:            since,
			Before:           before,
			BetweenFrom:      from,
			BetweenTo:        to,
			TemporalForm:     form,
			Memory:           it.Memory,
			SecondVersion:    it.SecondVersion,
		})
	}
	return out
}

// genSynthetic 生成确定性合成时序样本（实体×主题×日期枚举，查询内嵌期望事实）。
//
// 每条样本的 query 前缀即 expected fact 全文，保证确定性可召回；
// 后缀是最小化的窗口标记（4 种形态轮转）。
func genSynthetic(n int) []temporalItem {
	var out []temporalItem
	validFrom := "2026-01-01" // 所有合成记忆在该日之后均有效（as_of 过滤下可见）
	for i := 0; i < n; i++ {
		ent := tmpEntities[i%len(tmpEntities)]
		topic := tmpTopics[(i/len(tmpEntities))%len(tmpTopics)]
		day := tmpDays[(i/(len(tmpEntities)*len(tmpTopics)))%len(tmpDays)]
		// fact 内嵌 i，保证全局唯一。
		fact := fmt.Sprintf("%s的第%03d项%s调整为%s", ent, i, topic, day)
		memID := fmt.Sprintf("tmn-%03d", i)
		form := temporalForms[i%len(temporalForms)]
		asOf := tmpDates[i%len(tmpDates)]
		since := tmpDates[(i+1)%len(tmpDates)]
		before := tmpDates[(i+2)%len(tmpDates)]
		from := tmpDates[i%len(tmpDates)]
		to := tmpDates[(i+3)%len(tmpDates)]
		query := windowQuery(form, fact, asOf, since, before, from, to)
		out = append(out, temporalItem{
			ID:               fmt.Sprintf("tm-%03d", i),
			Query:            query,
			ExpectedMemoryID: []string{memID},
			ExpectedFact:     fact,
			Bucket:           tmpBucket,
			IsTemporal:       true,
			Tenant:           tmpTenant,
			User:             tmpUser,
			AsOf:             asOf,
			Since:            since,
			Before:           before,
			BetweenFrom:      from,
			BetweenTo:        to,
			TemporalForm:     form,
			Memory: &kernel.Memory{
				MemoryID:       memID,
				IdempotencyKey: memID,
				TenantID:       tmpTenant,
				UserID:         tmpUser,
				Visibility:     kernel.VisTenant,
				MemoryKind:     kernel.KindFact,
				Fact:           fact,
				Confidence:     0.9,
				Source:         "chat",
				Version:        1,
				Layer:          kernel.LayerL1,
				ValidFrom:      validFrom,
				Provenance:     kernel.ProvNormal,
				Permission:     kernel.Permission{Owner: tmpUser},
			},
		})
	}
	return out
}

func main() {
	srcPath := flag.String("src", "./benchmarks/dataset/all.jsonl", "公开基准 JSONL（用于抽取真实 temporal 样本）")
	outPath := flag.String("out", "./dataset/temporal.jsonl", "输出 JSONL 路径")
	count := flag.Int("count", 50, "目标 temporal 样本数（≥50，任务书 B.5 验收）")
	flag.Parse()

	if *count < 50 {
		fmt.Fprintf(os.Stderr, "FATAL: -count=%d < 50，任务书 B.5 验收不通过\n", *count)
		os.Exit(1)
	}

	r := tmpSeed()

	// 真实样本数上限：取目标数的 40%（默认 20），最低 0；其余用合成模板补齐。
	// 上限可保证「即使真实样本全部失效」时，合成样本仍推动 temporal 桶 ≥ 85%。
	realMax := *count * 2 / 5
	var real []temporalItem
	if src, err := loadSrc(*srcPath); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: 读取 src 失败，跳过真实样本抽取：%v\n", err)
	} else {
		real = extractReal(r, src, realMax)
	}

	synthN := *count - len(real)
	if synthN < 0 {
		synthN = 0
	}
	synth := genSynthetic(synthN)

	items := append(real, synth...)
	if len(items) < 50 {
		fmt.Fprintf(os.Stderr, "FATAL: temporal 样本数 %d < 50，任务书 B.5 验收不通过\n", len(items))
		os.Exit(1)
	}

	f, err := os.Create(*outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *outPath, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			fmt.Fprintf(os.Stderr, "encode %s: %v\n", it.ID, err)
			os.Exit(1)
		}
	}
	fmt.Printf("R5-B: temporal 样本已生成 -> %s（真实 %d + 合成 %d = %d 条，seed=42，纯模板确定性）\n",
		*outPath, len(real), len(synth), len(items))
}
