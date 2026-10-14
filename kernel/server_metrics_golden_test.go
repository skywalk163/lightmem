// R10-C：ServerMetrics Prometheus 文本格式 Golden 快照测试
// 真源：`lightmem/.dumate/inbox/_R10_生产可靠性闭环并行任务书.md` §3.C.3
//
// 设计：
//   - 固定输入序列（method/path/status/latency + RecordConsolidate）→ 固定输出快照；
//   - 用 .golden 文件记录基线，`-update` 可重生成，二跑逐字节一致；
//   - 同时包含格式合规断言（+Inf 桶、标签字典序、HELP/TYPE 完整、命名合法）。
//
// 确定性红线：
//   - 输入序列固定，无 time.Now()、无随机源、无 map 迭代序；
//   - 输出按 key 字典序，桶边界固定。
package kernel

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrometheusTextGolden 固定输入 → 输出快照，断言与 .golden 文件逐字节一致。
// 首跑用 -update 生成 .golden，二跑比对。
func TestPrometheusTextGolden(t *testing.T) {
	m := NewServerMetrics()

	// 固定输入序列：覆盖 2xx/4xx、save/recall/other、不同延迟
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusCreated, 3.2)
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 7.5)
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, "/missing", http.StatusNotFound, 0.5)
	m.ObserveHTTP(http.MethodPost, "/tools/memory/save", http.StatusOK, 12.8)
	m.ObserveHTTP(http.MethodPost, "/tools/memory/recall", http.StatusOK, 5.1)
	m.ObserveHTTP(http.MethodGet, "/search/vector", http.StatusOK, 25.0)
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusBadRequest, 1.0) // 4xx 不计入业务动作

	// 记录 consolidate 时间戳
	m.RecordConsolidate(time.Date(2026, 10, 14, 8, 30, 0, 0, time.UTC))

	out := m.PrometheusText()

	goldenPath := filepath.Join("testdata", "server_metrics.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(out), 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to generate)", err)
	}

	if string(want) != out {
		// 友好 diff
		t.Errorf("output mismatch against golden\n--- want (%s) ---\n%s\n--- got ---\n%s", goldenPath, want, out)
	}
}

// TestPrometheusTextGolden_Empty 空注册表快照（无请求时输出仍含 HELP/TYPE 行）。
func TestPrometheusTextGolden_Empty(t *testing.T) {
	m := NewServerMetrics()
	out := m.PrometheusText()

	goldenPath := filepath.Join("testdata", "server_metrics_empty.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(out), 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to generate)", err)
	}

	if string(want) != out {
		t.Errorf("output mismatch against golden\n--- want (%s) ---\n%s\n--- got ---\n%s", goldenPath, want, out)
	}
}

// TestPrometheusText_Compliance 格式合规断言（不依赖 golden 文件，独立验证）。
func TestPrometheusText_Compliance(t *testing.T) {
	m := NewServerMetrics()
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusCreated, 3.2)
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 7.5)
	m.ObserveHTTP(http.MethodGet, "/a", http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, "/z", http.StatusOK, 2.0) // 验证字典序
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusBadRequest, 1.0)

	out := m.PrometheusText()

	// 1) HELP/TYPE 完整
	for _, metric := range []string{"http_requests_total", "http_request_duration_ms", "memories_saved_total", "memories_recalled_total", "recall_latency_ms"} {
		if !strings.Contains(out, "# HELP "+metric) {
			t.Errorf("missing # HELP for %s", metric)
		}
		if !strings.Contains(out, "# TYPE "+metric) {
			t.Errorf("missing # TYPE for %s", metric)
		}
	}

	// 2) 直方图 +Inf 桶存在
	if !strings.Contains(out, `http_request_duration_ms_bucket{le="+Inf"}`) {
		t.Errorf("missing +Inf bucket for http_request_duration_ms")
	}
	if !strings.Contains(out, `recall_latency_ms_bucket{le="+Inf"}`) {
		t.Errorf("missing +Inf bucket for recall_latency_ms")
	}

	// 3) 直方图 _sum / _count 存在
	if !strings.Contains(out, "http_request_duration_ms_sum ") {
		t.Errorf("missing http_request_duration_ms_sum")
	}
	if !strings.Contains(out, "http_request_duration_ms_count 4") { // 4 个 2xx 请求
		t.Errorf("missing http_request_duration_ms_count 4, got:\n%s", out)
	}

	// 4) 标签字典序：http_requests_total 的 key 按字典序排列
	// 输入顺序：POST|/memories|201, GET|/search|200, GET|/a|200, GET|/z|200, POST|/memories|400
	// 字典序：GET|/a|200, GET|/search|200, GET|/z|200, POST|/memories|201, POST|/memories|400
	lines := strings.Split(out, "\n")
	var reqLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "http_requests_total{") {
			reqLines = append(reqLines, line)
		}
	}
	if len(reqLines) != 5 {
		t.Errorf("expected 5 http_requests_total lines, got %d", len(reqLines))
	}
	// 验证字典序
	for i := 0; i < len(reqLines)-1; i++ {
		if reqLines[i] > reqLines[i+1] {
			t.Errorf("http_requests_total not sorted: %q > %q", reqLines[i], reqLines[i+1])
		}
	}

	// 5) 标签值转义：strconv.Quote 已处理引号/反斜杠，验证输出含引号
	if !strings.Contains(out, `method="POST"`) {
		t.Errorf("missing quoted method label")
	}

	// 6) 4xx 不计入业务动作
	if !strings.Contains(out, "memories_saved_total 1") { // 仅 201 的 /memories 计入
		t.Errorf("memories_saved_total should be 1, got:\n%s", out)
	}
	if !strings.Contains(out, "memories_recalled_total 2") { // /search 两次 200
		t.Errorf("memories_recalled_total should be 2, got:\n%s", out)
	}

	// 7) 指标名合法性：只含小写字母、下划线、数字
	for _, line := range lines {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		// 提取指标名（第一个空格前）
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		name := parts[0]
		if strings.Contains(name, "{") {
			name = strings.Split(name, "{")[0]
		}
		// 简单验证：不含大写、不含连字符
		for _, r := range name {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("metric name %q contains uppercase", name)
			}
			if r == '-' {
				t.Errorf("metric name %q contains hyphen", name)
			}
		}
	}
}

// TestPrometheusText_LabelEscaping 标签转义：含引号/反斜杠/换行的 path 值正确转义。
func TestPrometheusText_LabelEscaping(t *testing.T) {
	m := NewServerMetrics()
	m.ObserveHTTP(http.MethodGet, `/path/with"quote`, http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, `/path/with\backslash`, http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, "/path/with\nnewline", http.StatusOK, 1.0)

	out := m.PrometheusText()

	// strconv.Quote 会把 " 转成 \"，\ 转成 \\, \n 转成 \n
	// 验证输出不含未转义的引号（除了 strconv.Quote 包裹用的外层引号）
	// 实际输出格式：method="GET",path="/path/with\"quote",status="200"
	if !strings.Contains(out, `path="/path/with\"quote"`) {
		t.Errorf("quote not escaped in path label, got:\n%s", out)
	}
	if !strings.Contains(out, `path="/path/with\\backslash"`) {
		t.Errorf("backslash not escaped in path label, got:\n%s", out)
	}
	// 换行在 strconv.Quote 中会转成 \n
	// 验证输出可被解析（简单检查：每行含 = 且值被引号包裹）
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "{") || !strings.Contains(line, "}") {
			continue // 非 counter/histogram 行
		}
		// 简单验证：标签值被引号包裹
		start := strings.Index(line, "{")
		end := strings.Index(line, "}")
		if start == -1 || end == -1 {
			t.Errorf("malformed line: %s", line)
			continue
		}
		labels := line[start+1 : end]
		for _, label := range strings.Split(labels, ",") {
			label = strings.TrimSpace(label)
			kv := strings.SplitN(label, "=", 2)
			if len(kv) != 2 {
				t.Errorf("malformed label in line: %s", line)
				continue
			}
			val := kv[1]
			if len(val) < 2 || val[0] != '"' || val[len(val)-1] != '"' {
				t.Errorf("label value not quoted: %s in line: %s", label, line)
			}
		}
	}
}

// TestPrometheusText_NoScientificNotation 数值格式：避免科学计数法。
func TestPrometheusText_NoScientificNotation(t *testing.T) {
	m := NewServerMetrics()
	// 用小数值触发 formatFloat 的 'g' 格式
	m.ObserveHTTP(http.MethodGet, "/test", http.StatusOK, 0.001) // 0.001ms
	m.ObserveHTTP(http.MethodGet, "/test", http.StatusOK, 1000000.0) // 1000000ms

	out := m.PrometheusText()

	// 检查输出不含 'e' 或 'E'（科学计数法）
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 提取数值（最后一个空格后）
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		val := parts[len(parts)-1]
		if strings.Contains(strings.ToLower(val), "e") {
			t.Errorf("scientific notation found in value %q, line: %s", val, line)
		}
	}
}

// updateGolden 控制是否更新 golden 文件（go test -update）。
var updateGolden = flag.Bool("update", false, "update golden files")

// TestMain 解析 flag 后运行测试。
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}
