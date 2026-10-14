// R9-A：进程级 HTTP 指标注册表 + /metrics 文本输出
// 真源：`lightmem/.dumate/inbox/_R9_生产化收口任务书.md` §1.A.3.2。
//
// 设计：
//   - ServerMetrics：进程级计数器 / 直方图，**手写桶，无第三方依赖**（不引入 Prometheus 客户端）；
//   - MetricsMiddleware：自动埋点（请求计数 + 延迟直方图 + 业务动作计数）；
//   - PrometheusText()：输出 Prometheus 文本格式（http_requests_total / http_request_duration_ms /
//     memories_saved_total / memories_recalled_total / recall_latency_ms）。
//
// 与既有 kernel.MetricsCollector 的关系（§1.A.2 只增不改）：
//   - 本文件是**新增**进程级 HTTP 指标注册表，不修改 kernel/metrics.go 既有字段/方法语义；
//   - /metrics 端点把本注册表文本与既有 lims_* 文本拼接输出，二者互补，互不覆盖。
//
// 确定性红线：
//   - 无 time.Now()（除 consolidate 时间戳记录外）、无随机源、无 map 迭代序（输出按 key 排序）；
//   - 桶边界固定，seed 无关。
package kernel

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------
// 直方图桶（毫秒）
// ------------------------------------------------------------------

// httpDurationBucketsMS 是 HTTP 请求延迟直方图桶（毫秒）。
var httpDurationBucketsMS = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

// recallDurationBucketsMS 是检索延迟直方图桶（毫秒）。
var recallDurationBucketsMS = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

// histogram 手写直方图（bucket 计数 + 和 + 样本数）。
type histogram struct {
	bounds []float64
	counts []uint64 // len(bounds) 个桶 + 1 个 +Inf 桶（counts[len(bounds)]）
	sum    float64
	n      uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	idx := len(h.counts) - 1 // 默认 +Inf
	for i, b := range h.bounds {
		if v <= b {
			idx = i
			break
		}
	}
	h.counts[idx]++
	h.sum += v
	h.n++
}

// ------------------------------------------------------------------
// ServerMetrics 注册表
// ------------------------------------------------------------------

// ServerMetrics 是进程级 HTTP + 业务指标注册表（线程安全）。
type ServerMetrics struct {
	mu sync.RWMutex

	// httpRequestsTotal: 形如 "method|path|status" -> 计数
	httpRequestsTotal map[string]int64
	httpDurationMS    *histogram

	// 业务动作计数
	memoriesSavedTotal    int64
	memoriesRecalledTotal int64
	recallLatencyMS       *histogram

	// 最近一次后台整合时间戳（UTC），0 值表示从未执行
	lastConsolidateUnix atomic.Int64
}

// NewServerMetrics 创建进程级指标注册表。
func NewServerMetrics() *ServerMetrics {
	return &ServerMetrics{
		httpRequestsTotal: map[string]int64{},
		httpDurationMS:    newHistogram(httpDurationBucketsMS),
		recallLatencyMS:   newHistogram(recallDurationBucketsMS),
	}
}

// isRecallRequest 判定请求是否属于检索类端点（用于 recall 延迟埋点与计数）。
func isRecallRequest(method, path string) bool {
	switch {
	case method == http.MethodGet && path == "/search":
		return true
	case method == http.MethodPost && path == "/tools/memory/recall":
		return true
	case method == http.MethodPost && path == "/search/vector":
		return true
	}
	return false
}

// isSaveRequest 判定请求是否属于写入类端点（用于 memories_saved_total 计数）。
func isSaveRequest(method, path string) bool {
	switch {
	case method == http.MethodPost && path == "/memories":
		return true
	case method == http.MethodPost && path == "/tools/memory/save":
		return true
	}
	return false
}

// ObserveHTTP 记录一次 HTTP 请求（计数 + 延迟直方图 + 业务动作计数）。
// 仅当 status 落在 2xx 时计入业务动作计数（成功写入/检索才计数）。
func (m *ServerMetrics) ObserveHTTP(method, path string, status int, durationMS float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := method + "|" + path + "|" + strconv.Itoa(status)
	m.httpRequestsTotal[key]++
	m.httpDurationMS.observe(durationMS)

	if status >= 200 && status < 300 {
		switch {
		case isRecallRequest(method, path):
			m.memoriesRecalledTotal++
			m.recallLatencyMS.observe(durationMS)
		case isSaveRequest(method, path):
			m.memoriesSavedTotal++
		}
	}
}

// RecordConsolidate 记录最近一次后台整合执行时间（UTC）。
func (m *ServerMetrics) RecordConsolidate(t time.Time) {
	m.lastConsolidateUnix.Store(t.UTC().Unix())
}

// LastConsolidate 返回最近一次后台整合时间（UTC）；从未执行时返回零值 time.Time。
func (m *ServerMetrics) LastConsolidate() time.Time {
	u := m.lastConsolidateUnix.Load()
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(u, 0).UTC()
}

// ------------------------------------------------------------------
// Prometheus 文本输出
// ------------------------------------------------------------------

// PrometheusText 输出本注册表的 Prometheus 文本格式指标。
// 输出顺序确定（标签按 key 字典序），无 map 迭代序。
func (m *ServerMetrics) PrometheusText() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out string

	// http_requests_total
	out += "# HELP http_requests_total HTTP 请求总数（按 method/path/status 切分）\n"
	out += "# TYPE http_requests_total counter\n"
	keys := make([]string, 0, len(m.httpRequestsTotal))
	for k := range m.httpRequestsTotal {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mth, p, st := splitKey(k)
		out += fmt.Sprintf("http_requests_total{method=%s,path=%s,status=%s} %d\n",
			labelVal(mth), labelVal(p), labelVal(st), m.httpRequestsTotal[k])
	}

	// http_request_duration_ms 直方图
	out += "# HELP http_request_duration_ms HTTP 请求延迟（毫秒）\n"
	out += "# TYPE http_request_duration_ms histogram\n"
	out += renderHistogram("http_request_duration_ms", m.httpDurationMS, "ms")

	// memories_saved_total
	out += "# HELP memories_saved_total 已成功写入的记忆总数\n"
	out += "# TYPE memories_saved_total counter\n"
	out += fmt.Sprintf("memories_saved_total %d\n", m.memoriesSavedTotal)

	// memories_recalled_total
	out += "# HELP memories_recalled_total 已成功检索的次数总数\n"
	out += "# TYPE memories_recalled_total counter\n"
	out += fmt.Sprintf("memories_recalled_total %d\n", m.memoriesRecalledTotal)

	// recall_latency_ms 直方图
	out += "# HELP recall_latency_ms 检索延迟（毫秒）\n"
	out += "# TYPE recall_latency_ms histogram\n"
	out += renderHistogram("recall_latency_ms", m.recallLatencyMS, "ms")

	return out
}

// renderHistogram 渲染单一直方图为 Prometheus 文本（bucket / sum / count）。
func renderHistogram(name string, h *histogram, unit string) string {
	var out string
	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i]
		out += fmt.Sprintf("%s_bucket{le=%s} %d\n", name, labelVal(formatFloat(b)+unit), cum)
	}
	cum += h.counts[len(h.counts)-1]
	out += fmt.Sprintf("%s_bucket{le=%s} %d\n", name, labelVal("+Inf"), cum)
	out += fmt.Sprintf("%s_sum %s\n", name, formatFloat(h.sum))
	out += fmt.Sprintf("%s_count %d\n", name, h.n)
	return out
}

// labelVal 转义 Prometheus 标签值（反斜杠与双引号）。
func labelVal(s string) string {
	s = strconv.Quote(s)
	// strconv.Quote 已转义引号与反斜杠；Prometheus 接受 Go 转义格式。
	return s
}

func splitKey(k string) (method, path, status string) {
	// 格式为 "method|path|status"，path 中可能含 '|'，因此从右侧切分两次。
	end1 := len(k)
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '|' {
			end1 = i
			break
		}
	}
	status = k[end1+1:]
	end2 := end1
	for i := end1 - 1; i >= 0; i-- {
		if k[i] == '|' {
			end2 = i
			break
		}
	}
	method = k[:end2]
	path = k[end2+1 : end1]
	return method, path, status
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ------------------------------------------------------------------
// MetricsMiddleware：自动埋点
// ------------------------------------------------------------------

// MetricsMiddleware 对每个请求自动埋点（请求计数 + 延迟直方图 + 业务动作计数）。
// 放置顺序建议：Logging -> Metrics -> Recover -> TraceID -> 业务路由。
func MetricsMiddleware(m *ServerMetrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			dur := time.Since(start).Seconds() * 1000
			m.ObserveHTTP(r.Method, r.URL.Path, sw.status, dur)
		})
	}
}

// ------------------------------------------------------------------
// HealthzHandler：升级版健康检查（§1.A.3.3）
// ------------------------------------------------------------------

// HealthzHandler 返回 /healthz 处理器，返回 JSON：
//
//	{"status":"ok","backend":"sqlite","embedder":"hash","last_consolidate":"2026-10-14T..."}
//
// 语义：
//   - backend ping 失败 → 503 {"status":"degraded", ...}
//   - last_consolidate 为最近一次后台整合时间（UTC）；从未执行时为空字符串。
//
// 确定性红线：embedder 名与 backend 名来自装配参数，不引入随机源。
func HealthzHandler(srv *APIServer, backend StorageBackend, embedderName string, metrics *ServerMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		resp := map[string]string{
			"status":           "ok",
			"backend":          backend.Name(),
			"embedder":         embedderName,
			"last_consolidate": "",
		}
		if lc := metrics.LastConsolidate(); !lc.IsZero() {
			resp["last_consolidate"] = lc.Format(time.RFC3339)
		}
		if srv != nil {
			resp["profile"] = string(srv.profile.Profile)
		}

		if err := backend.Ping(ctx); err != nil {
			resp["status"] = "degraded"
			resp["error"] = err.Error()
			writeJSON(w, http.StatusServiceUnavailable, resp)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}
