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
	"strings"
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

// backendProbeBucketsMS 是就绪探针耗时直方图桶（毫秒，R29-B §3.2.5 backend_probe_ms）。
// 探针预算 ≤2s，因此桶上限到 2000ms 即可；复用 ms 口径，无秒级大桶。
var backendProbeBucketsMS = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2000}

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

	// ── R29-B L2 健康硬化：生命周期 / 维护 / 排水 / 探针指标（§3.2.5）──
	// 命名沿用本文件无前缀家族，不引入第三套（lims_* 家族在 metrics.go）。
	// 键格式："from|to|reason"、"job|result"、"job|reason"、"check"。
	stateTransitionsTotal   map[string]int64 // 状态机迁移（I2 可证条件）
	maintenanceRunsTotal    map[string]int64 // 每类后台任务成功/失败
	maintenanceSkippedTotal map[string]int64 // 单飞跳过 / 预算用尽
	drainTimeoutsTotal      int64            // 排水超时（含硬兜底触发）
	readyzFailuresTotal     map[string]int64 // 就绪失败分类
	backendProbeMS          *histogram       // 探针耗时（验证 TTL 缓存是否真的减负）
}

// newServerMetricsValue 构造一个全新的指标注册表（不加锁，供 NewServerMetrics /
// DefaultServerMetrics 在持锁状态下复用，避免重入死锁）。
func newServerMetricsValue() *ServerMetrics {
	return &ServerMetrics{
		httpRequestsTotal:       map[string]int64{},
		httpDurationMS:          newHistogram(httpDurationBucketsMS),
		recallLatencyMS:         newHistogram(recallDurationBucketsMS),
		stateTransitionsTotal:   map[string]int64{},
		maintenanceRunsTotal:    map[string]int64{},
		maintenanceSkippedTotal: map[string]int64{},
		readyzFailuresTotal:     map[string]int64{},
		backendProbeMS:          newHistogram(backendProbeBucketsMS),
	}
}

// defaultMetrics 是进程级默认指标注册表（R29-B 新增）。
//
// 存在理由（不改变 NewServerMetrics 既有语义）：
//   - 指标实例目前在 main.go 局部创建（`metrics := kernel.NewServerMetrics()`），
//     而 health_probe.go 的探针、C/D 线的调度/排水**都没有 main.go 的句柄**，
//     且 main.go 只有 T0 能改（任务书 §2.2 红线⑥）；
//   - 因此提供一个包级默认实例作为共享出口：NewServerMetrics() 仍返回全新注册表
//     （语义不变），同时把最近一次创建的实例登记为默认；未显式创建时按需惰性创建。
//
// 默认关自证：未设任何新开关时，本机制不改变任何 HTTP 行为（只是多了一个可写指针）。
var (
	defaultMetricsMu sync.RWMutex
	defaultMetrics   *ServerMetrics
)

// NewServerMetrics 创建进程级指标注册表（语义不变：每次调用返回全新注册表）。
// 副作用：把该实例登记为包级默认实例（见 DefaultServerMetrics）。
func NewServerMetrics() *ServerMetrics {
	m := newServerMetricsValue()
	defaultMetricsMu.Lock()
	defaultMetrics = m
	defaultMetricsMu.Unlock()
	return m
}

// DefaultServerMetrics 返回包级默认指标注册表；未显式创建过则惰性创建一个。
// 供 health_probe.go 探针与 C/D 线调度/排水复用（它们无 main.go 句柄）。
func DefaultServerMetrics() *ServerMetrics {
	defaultMetricsMu.RLock()
	m := defaultMetrics
	defaultMetricsMu.RUnlock()
	if m != nil {
		return m
	}
	defaultMetricsMu.Lock()
	defer defaultMetricsMu.Unlock()
	if defaultMetrics == nil {
		defaultMetrics = newServerMetricsValue()
	}
	return defaultMetrics
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
// R29-B：生命周期 / 维护 / 排水 / 探针指标（§10 冻结方法，C/D 只调用）
// ------------------------------------------------------------------

// IncStateTransition 记录一次状态机迁移（from → to，附 reason）。
func (m *ServerMetrics) IncStateTransition(from, to, reason string) {
	m.mu.Lock()
	m.stateTransitionsTotal[from+"|"+to+"|"+reason]++
	m.mu.Unlock()
}

// IncMaintenanceRun 记录一类后台任务的一轮结果（result 建议 ok/fail）。
func (m *ServerMetrics) IncMaintenanceRun(job, result string) {
	m.mu.Lock()
	m.maintenanceRunsTotal[job+"|"+result]++
	m.mu.Unlock()
}

// IncMaintenanceSkipped 记录一次后台任务跳过（reason 如 single_flight/budget_exhausted）。
func (m *ServerMetrics) IncMaintenanceSkipped(job, reason string) {
	m.mu.Lock()
	m.maintenanceSkippedTotal[job+"|"+reason]++
	m.mu.Unlock()
}

// IncDrainTimeout 记录一次排水超时（含硬兜底触发）。
func (m *ServerMetrics) IncDrainTimeout() {
	m.mu.Lock()
	m.drainTimeoutsTotal++
	m.mu.Unlock()
}

// IncReadyzFailure 记录一次就绪失败（check 为失败分类，如 ping/read）。
func (m *ServerMetrics) IncReadyzFailure(check string) {
	if check == "" {
		check = "unknown"
	}
	m.mu.Lock()
	m.readyzFailuresTotal[check]++
	m.mu.Unlock()
}

// ObserveBackendProbe 记录一次后端探针耗时（毫秒）。
func (m *ServerMetrics) ObserveBackendProbe(ms float64) {
	m.mu.Lock()
	m.backendProbeMS.observe(ms)
	m.mu.Unlock()
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

	// ── R29-B：生命周期 / 维护 / 排水 / 探针指标 ──
	out += renderLabelledCounter("state_transitions_total",
		"状态机迁移总数（按 from/to/reason 切分）",
		[]string{"from", "to", "reason"}, m.stateTransitionsTotal)
	out += renderLabelledCounter("maintenance_runs_total",
		"后台维护任务运行总数（按 job/result 切分）",
		[]string{"job", "result"}, m.maintenanceRunsTotal)
	out += renderLabelledCounter("maintenance_skipped_total",
		"后台维护任务跳过总数（按 job/reason 切分）",
		[]string{"job", "reason"}, m.maintenanceSkippedTotal)

	out += "# HELP drain_timeouts_total 排水超时总数（含硬兜底触发）\n"
	out += "# TYPE drain_timeouts_total counter\n"
	out += fmt.Sprintf("drain_timeouts_total %d\n", m.drainTimeoutsTotal)

	out += renderLabelledCounter("readyz_failures_total",
		"就绪探针失败总数（按 check 分类）",
		[]string{"check"}, m.readyzFailuresTotal)

	out += "# HELP backend_probe_ms 后端就绪探针耗时（毫秒）\n"
	out += "# TYPE backend_probe_ms histogram\n"
	out += renderHistogram("backend_probe_ms", m.backendProbeMS, "ms")

	return out
}

// renderLabelledCounter 渲染一个带固定标签集的计数器家族（标签按 key 字典序，无 map 迭代序）。
// 键格式为以 '|' 连接的标签值，标签个数与 labels 长度一致。
func renderLabelledCounter(name, help string, labels []string, counts map[string]int64) string {
	var out string
	out += fmt.Sprintf("# HELP %s %s\n", name, help)
	out += fmt.Sprintf("# TYPE %s counter\n", name)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := splitLabels(k, len(labels))
		var b strings.Builder
		b.WriteString(name)
		b.WriteString("{")
		for i, ln := range labels {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(ln)
			b.WriteString("=")
			b.WriteString(labelVal(vals[i]))
		}
		b.WriteString("}")
		out += fmt.Sprintf("%s %d\n", b.String(), counts[k])
	}
	return out
}

// splitLabels 把以 '|' 连接的键切成 n 段；段数不足时补空串（防止脏键导致越界 panic）。
func splitLabels(k string, n int) []string {
	parts := strings.SplitN(k, "|", n)
	for len(parts) < n {
		parts = append(parts, "")
	}
	return parts
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

// formatFloat 输出 Prometheus 数值文本。
// R13-A 修复（R12 遗留缺陷）：原实现用 'g' 格式，大数会退化成科学计数法
// （如 1000000.001 → "1.000000001e+06"），而 Prometheus 文本格式与
// TestPrometheusText_NoScientificNotation 均要求**禁用科学计数法**。
// 改用 'f'（定点、-1 精度）+ 去尾零，保证既有小数值输出（如 3.2）逐字节不变。
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	// 'f' 格式不会产出科学计数法；仅需去掉定点可能留下的尾随零（如 "3.20" → "3.2"）。
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
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
