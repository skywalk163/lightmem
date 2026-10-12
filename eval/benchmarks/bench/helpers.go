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
