package bench

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lightmem/kernel"
)

var weekdayParen = regexp.MustCompile(`\s*\((?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)\)\s*`)

// dateLayouts 覆盖三基准里出现过的常见日期格式；解析失败则留零值（确定性，不取 time.Now）。
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02",
	"2006/01/02 15:04",
	"3:04 PM on 2 January, 2006", // LoCoMo: "1:56 pm on 8 May, 2023"
	"January 2, 2006",
	"Jan 2, 2006",
}

// parseDate 确定性解析日期字符串；失败返回零值。
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// 规整：去掉 "(Mon)" 星期括号，am/pm 大写。
	s = weekdayParen.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, " am", " AM")
	s = strings.ReplaceAll(s, " pm", " PM")
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// newMemory 构造一条符合 LIMS Schema 的事实记忆（字段口径对齐 eval/eval_generate.go）。
// origin=数据来源，snippet=原文证据片段，dateStr=turn/question 时间戳（可空）。
func newMemory(memID, tenant, user, fact, origin, snippet, dateStr string) kernel.Memory {
	m := kernel.Memory{
		MemoryID:       memID,
		IdempotencyKey: memID,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     kernel.VisTenant,
		MemoryKind:     kernel.KindFact,
		Fact:           fact,
		Confidence:     0.9,
		Source:         "chat",
		Version:        1,
		Layer:          kernel.LayerL1,
		Provenance:     kernel.ProvNormal,
		Permission:     kernel.Permission{Owner: user},
		ReferenceFrame: kernel.ReferenceFrame{Tenant: tenant, User: user, Scene: origin},
	}
	if strings.TrimSpace(snippet) != "" {
		m.SourceEvidence = &kernel.SourceEvidence{Origin: origin, Snippet: truncateRunes(snippet, 200)}
	}
	if t, ok := parseDate(dateStr); ok {
		m.RecordedAt = t
		m.OccurredAt = &t
	}
	return m
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// SanitizeID 归一化 id（去空格、替换分隔符）。
func SanitizeID(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// RawToString 把 JSON 标量转成字符串，数组取 " | " 连接。
func RawToString(r json.RawMessage) string {
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(r, &n); err == nil {
		return n.String()
	}
	var arr []string
	if err := json.Unmarshal(r, &arr); err == nil {
		return strings.Join(arr, " | ")
	}
	return string(r)
}

// MemKey 生成 lc- 前缀记忆 id。
func MemKey(sampleID, dia string) string {
	return fmt.Sprintf("lc-%s-%s", SanitizeID(sampleID), strings.ReplaceAll(dia, ":", "_"))
}

// dateOnly 把 time 转成 date-only UTC 字符串（YYYY-MM-DD），用于 ValidFrom/ValidUntil。
func dateOnly(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// asOfDate 解析 as_of 字符串为 date-only 时间；失败返回零值（调用方需再判 ok）。
func asOfDate(s string) (time.Time, bool) {
	t, ok := parseDate(s)
	if !ok {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// makeSecondVersion 为 temporal 样本构造一个「在 as_of 之前已过期」的历史版本记忆（R2 任务B）。
//
// 规则（确定性，不调 LLM、不用 time.Now）：
//   - memID       ：新 id（避免与当前版本冲突），基于当前 id 加 "-v0" 后缀
//   - fact        ：与当前版本相同事实（时间推理样本的 expected 答案不随版本变化）；
//     仅 ValidFrom/ValidUntil/SupersededBy 体现版本差异
//   - ValidUntil  ：as_of 前一日（date-only），as_of 查询时 ScanLayer as_of 分支
//     （*rc.AsOf > m.ValidUntil → continue）判为过期 → 被过滤
//   - ValidFrom   ：与 ValidUntil 同一天（窗口收紧到单日），使 SecondVersion 只在与自身
//     as_of 相关时才可见，最大程度避免跨样本残留污染其他查询的候选集。
//     实测：窗口 = [asof-30, asof-1] 时平均 17.35 个跨样本 SV 可见；收紧到 [asof-1, asof-1]
//     后降到平均 0.78、最多 5 个（R2 实测根因）
//   - SupersededBy：指向当前版本 MemoryID，使**非 as_of 查询**（router_mem.go ScanLayer
//     默认分支）也过滤本版本，避免它作为「当前生效版本」混入共享 backend 干扰 top-1 排序
//   - OccurredAt/RecordedAt：以 ValidUntil 为基准，确定性构造
//   - IsHistorical：true，标记为历史版本
//
// asOf 为空或解析失败时返回 nil（调用方降级：该样本不补 SecondVersion）。
func makeSecondVersion(m kernel.Memory, asOfStr string) *kernel.Memory {
	t, ok := asOfDate(asOfStr)
	if !ok || t.IsZero() {
		return nil
	}
	validUntil := t.AddDate(0, 0, -1).UTC()
	validFrom := validUntil
	v := m
	v.MemoryID = m.MemoryID + "-v0"
	v.IdempotencyKey = v.MemoryID
	v.Version = 0
	v.ValidFrom = dateOnly(validFrom)
	v.ValidUntil = dateOnly(validUntil)
	// SupersededBy 指向当前版本：非 as_of 查询（ScanLayer 默认分支）会过滤
	// SupersededBy != nil 的记忆，避免污染其它样本的排序。
	curID := m.MemoryID
	v.SupersededBy = &curID
	v.IsHistorical = true
	occ := validUntil
	v.OccurredAt = &occ
	v.RecordedAt = validUntil
	return &v
}
