//go:build ignore

// polaris_injection.go — 投毒检测评测 runner（R15-A，§7.5.1）。
//
// 定位：把 §7.5 的「检测」阶段变成可测指标。与 holdout 北极星完全隔离——
//   - 不进入 eval/holdout/；不引用 holdout_v2；不经过检索/排序链路；
//   - 只调用 kernel.DetectInjection，即纯文本 → 判定结果，无 LLM、无随机源、无 I/O 副作用。
//
// 运行（在仓库根目录）：
//
//	cd eval/polaris && go run polaris_injection.go
//	go run eval/polaris/polaris_injection.go \
//	  -injection ../dataset/injection_v2.jsonl \
//	  -benign ../dataset/benign_near.jsonl \
//	  -report r15a_injection -now-fixed 2026-10-15
//
// 报告字段（任务书 §A.5）：recall / precision / false_positive_rate /
// severity_distribution / by_rule / by_pattern / missed / false_positives。
// 不输出 M1 / M2——那两项是 holdout 北极星的口径，注入语料不参与。
//
// 确定性：固定 clock（-now-fixed），无 time.Now()、无 rand、map 结果均排序后输出；
// 报告文件名含固定 clock，因此同参数重复运行产物逐位一致。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

// sample 注入/良性语料单行（只取检测所需字段）。
type sample struct {
	ID           string `json:"id"`
	Pattern      string `json:"pattern"`
	Lang         string `json:"lang"`
	Bucket       string `json:"bucket"`
	HasInjection bool   `json:"has_injection"`
	Fact         string `json:"fact"`
}

// ruleStat 单条规则的命中统计。
type ruleStat struct {
	Hits     int     `json:"hits"`
	Weight   float64 `json:"weight"`
	Severity string  `json:"severity"`
}

// patternStat 单个语料模式的检出统计。
type patternStat struct {
	Total    int `json:"total"`
	Detected int `json:"detected"`
	TP       int `json:"tp"`
	FP       int `json:"fp"`
}

// missCase 未检出对抗样本 / 误报良性样本的简录（供人复核）。
type missCase struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Fact    string `json:"fact"`
}

// injectionReport 评测报告（落盘 JSON）。
type injectionReport struct {
	Report            string                 `json:"report"`
	GeneratedAt       time.Time              `json:"generated_at"`
	InjectionPath     string                 `json:"injection_path"`
	InjectionTotal    int                    `json:"injection_total"`
	BenignPath        string                 `json:"benign_path"`
	BenignTotal       int                    `json:"benign_total"`
	Config            reportConfig           `json:"config"`
	Recall            float64                `json:"recall"`
	TruePositives     int                    `json:"true_positives"`
	FalsePositives    int                    `json:"false_positives"`
	Precision         float64                `json:"precision"`
	FalsePositiveRate float64                `json:"false_positive_rate"`
	SeverityDist      map[string]int         `json:"severity_distribution"`
	ByRule            map[string]ruleStat    `json:"by_rule"`
	ByPattern         map[string]patternStat `json:"by_pattern"`
	Missed            []missCase             `json:"missed,omitempty"`
	FalsePositiveList []missCase             `json:"false_positives_list,omitempty"`
	Note              string                 `json:"note"`
}

type reportConfig struct {
	IndirectMode string `json:"indirect_mode"`
	EnableRepeat bool   `json:"enable_repeat"`
	NowFixed     string `json:"now_fixed"`
}

// ruleSeverity 返回规则 ID 对应的严重度文本（与 kernel 规则表一致，重复读取不缓存）。
func ruleSeverity(id string) (severity string, weight float64) {
	for _, r := range kernel.InjectionRuleCatalog() {
		if r.ID == id {
			return r.Sev.String(), r.Weight
		}
	}
	return "low", 0
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func loadSamples(path string) ([]sample, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []sample
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if s.Fact == "" {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// resolvePath 依次尝试候选路径，返回第一个存在的；都不存在返回空串。
func resolvePath(candidates ...string) string {
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// resolveReportsDir 定位报告目录：显式 > eval/reports（自 eval/polaris 或仓库根两种工作目录）。
func resolveReportsDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, c := range []string{"../reports", "eval/reports"} {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "eval/reports"
}

func writeReport(dir, prefix string, rep *injectionReport) string {
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, prefix+".json")
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		log.Fatalf("encode report: %v", err)
	}
	return path
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[injection] ")

	injFlag := flag.String("injection", "", "对抗样本 NDJSON（默认 eval/dataset/injection_v2.jsonl）")
	benFlag := flag.String("benign", "", "良性近似 NDJSON（默认 eval/dataset/benign_near.jsonl）")
	reportFlag := flag.String("report", "r15a_injection", "报告文件名前缀")
	nowFixed := flag.String("now-fixed", "2026-10-15", "固定基准日期（写进 generated_at 与报告文件名，保证可复现）")
	reportsDir := flag.String("reports-dir", "", "报告输出目录（默认自动探测 eval/reports）")
	indirect := flag.Bool("indirect", false, "全部样本按间接注入面判定（IndirectMode=On，消融用）")
	enableRepeat := flag.Bool("enable-repeat", false, "启用重复写入判定（EnableRepeat）")
	flag.Parse()

	now, err := time.Parse("2006-01-02", *nowFixed)
	if err != nil {
		log.Fatalf("parse -now-fixed %q: %v", *nowFixed, err)
	}

	injPath := *injFlag
	if injPath == "" {
		injPath = resolvePath("../dataset/injection_v2.jsonl", "eval/dataset/injection_v2.jsonl",
			"dataset/injection_v2.jsonl")
	}
	if injPath == "" {
		log.Fatalf("未找到对抗语料（用 -injection 指定，或生成 eval/dataset/injection_v2.jsonl）")
	}
	benPath := *benFlag
	if benPath == "" {
		benPath = resolvePath("../dataset/benign_near.jsonl", "eval/dataset/benign_near.jsonl",
			"dataset/benign_near.jsonl")
	}
	if benPath == "" {
		log.Fatalf("未找到良性语料（用 -benign 指定，或生成 eval/dataset/benign_near.jsonl）")
	}

	inj, err := loadSamples(injPath)
	if err != nil {
		log.Fatalf("load %s: %v", injPath, err)
	}
	ben, err := loadSamples(benPath)
	if err != nil {
		log.Fatalf("load %s: %v", benPath, err)
	}
	log.Printf("loaded %d injection / %d benign samples", len(inj), len(ben))

	mode := kernel.DetectIndirectOff
	modeText := "off"
	if *indirect {
		mode = kernel.DetectIndirectOn
		modeText = "on"
	}
	cfg := kernel.DetectConfig{IndirectMode: mode, EnableRepeat: *enableRepeat}

	rep := &injectionReport{
		Report:            fmt.Sprintf("%s_%s", *reportFlag, now.Format("2006-01-02")),
		GeneratedAt:       now.UTC(),
		InjectionPath:     injPath,
		InjectionTotal:    len(inj),
		BenignPath:        benPath,
		BenignTotal:       len(ben),
		Config:            reportConfig{IndirectMode: modeText, EnableRepeat: *enableRepeat, NowFixed: *nowFixed},
		SeverityDist:      map[string]int{},
		ByRule:            map[string]ruleStat{},
		ByPattern:         map[string]patternStat{},
		Missed:            []missCase{},
		FalsePositiveList: []missCase{},
	}
	_ = rep // 字段填充在下面逐样本累积
	run := func(ss []sample, isInjection bool) {
		for _, s := range ss {
			v := kernel.DetectInjection(s.Fact, kernel.WriteMeta{}, cfg)
			sev := v.Severity.String()
			rep.SeverityDist[sev]++
			for _, sig := range v.Signals {
				rs, ok := rep.ByRule[sig]
				if !ok {
					sevText, w := ruleSeverity(sig)
					rs = ruleStat{Severity: sevText, Weight: w}
				}
				rs.Hits++
				rep.ByRule[sig] = rs
			}
			ps := rep.ByPattern[s.Pattern]
			ps.Total++
			if v.IsSuspect() {
				ps.Detected++
				if isInjection {
					rep.TruePositives++
					ps.TP++
				} else {
					rep.FalsePositives++
					ps.FP++
					if len(rep.FalsePositiveList) < 20 {
						rep.FalsePositiveList = append(rep.FalsePositiveList,
							missCase{ID: s.ID, Pattern: s.Pattern, Fact: s.Fact})
					}
				}
			} else if isInjection {
				if len(rep.Missed) < 20 {
					rep.Missed = append(rep.Missed,
						missCase{ID: s.ID, Pattern: s.Pattern, Fact: s.Fact})
				}
			}
			rep.ByPattern[s.Pattern] = ps
		}
	}
	run(inj, true)
	run(ben, false)

	rep.Recall = rate(rep.TruePositives, rep.InjectionTotal)
	rep.FalsePositiveRate = rate(rep.FalsePositives, rep.BenignTotal)
	rep.Precision = rate(rep.TruePositives, rep.TruePositives+rep.FalsePositives)

	sort.Slice(rep.Missed, func(i, j int) bool { return rep.Missed[i].ID < rep.Missed[j].ID })
	sort.Slice(rep.FalsePositiveList, func(i, j int) bool {
		return rep.FalsePositiveList[i].ID < rep.FalsePositiveList[j].ID
	})

	var ruleIDs []string
	for id := range rep.ByRule {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)
	var ptnIDs []string
	for p := range rep.ByPattern {
		ptnIDs = append(ptnIDs, p)
	}
	sort.Strings(ptnIDs)
	_ = ruleIDs
	_ = ptnIDs

	rep.Note = fmt.Sprintf(
		"R15-A 投毒检测评测：recall=%.4f（%d/%d）、precision=%.4f、false_positive_rate=%.4f（%d/%d）；"+
			"间接面模式=%s、重复写入判定=%v。检测阶段只置标签，处置（降权/截断）由 PoisoningGuardPolicy 承担。",
		rep.Recall, rep.TruePositives, rep.InjectionTotal, rep.Precision,
		rep.FalsePositiveRate, rep.FalsePositives, rep.BenignTotal,
		modeText, *enableRepeat)

	out := writeReport(resolveReportsDir(*reportsDir), rep.Report, rep)
	log.Printf("recall=%.4f precision=%.4f fpr=%.4f rules=%d patterns=%d",
		rep.Recall, rep.Precision, rep.FalsePositiveRate, len(rep.ByRule), len(rep.ByPattern))
	log.Printf("report: %s", out)
}
