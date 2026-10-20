// R9-A：ServerMetrics 冒烟测试（任务书 §1.A.3.5）
// 覆盖：Prometheus 文本格式合法性、直方图桶、业务动作计数、健康检查 JSON。
package kernel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 空注册表输出仍含必需 HELP/TYPE 行（Prometheus 文本合法）。
func TestServerMetrics_EmptyOutput(t *testing.T) {
	m := NewServerMetrics()
	out := m.PrometheusText()
	for _, want := range []string{
		"# HELP http_requests_total",
		"# TYPE http_requests_total counter",
		"# HELP http_request_duration_ms",
		"# TYPE http_request_duration_ms histogram",
		"# HELP memories_saved_total",
		"# TYPE memories_saved_total counter",
		"# HELP memories_recalled_total",
		"# TYPE memories_recalled_total counter",
		"# HELP recall_latency_ms",
		"# TYPE recall_latency_ms histogram",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("empty output missing %q", want)
		}
	}
}

// 记录请求后输出包含带标签的 counter 行。
func TestServerMetrics_AfterRequests(t *testing.T) {
	m := NewServerMetrics()
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusCreated, 3.2)
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 7.5)
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, "/missing", http.StatusNotFound, 0.5)

	out := m.PrometheusText()
	checks := []string{
		`http_requests_total{method="POST",path="/memories",status="201"} 1`,
		`http_requests_total{method="GET",path="/search",status="200"} 2`,
		`http_requests_total{method="GET",path="/missing",status="404"} 1`,
		"memories_saved_total 1",
		"memories_recalled_total 2",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q\n---\n%s", want, out)
		}
	}
	// 直方图必须有 bucket/sum/count 且 count 与观测数一致
	if !strings.Contains(out, "http_request_duration_ms_bucket{le=\"1ms\"}") {
		t.Fatalf("missing duration bucket")
	}
	if !strings.Contains(out, "http_request_duration_ms_count 4") {
		t.Fatalf("missing duration count 4\n---\n%s", out)
	}
	if !strings.Contains(out, "recall_latency_ms_count 2") {
		t.Fatalf("missing recall count 2\n---\n%s", out)
	}
}

// 4xx 请求不计入业务动作计数。
func TestServerMetrics_4xxNotCounted(t *testing.T) {
	m := NewServerMetrics()
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusBadRequest, 1.0)
	out := m.PrometheusText()
	if !strings.Contains(out, "memories_saved_total 0") {
		t.Fatalf("bad request should not count saved\n---\n%s", out)
	}
}

// 输出顺序确定（标签按 key 字典序）：连续两次输出完全一致。
func TestServerMetrics_DeterministicOutput(t *testing.T) {
	m := NewServerMetrics()
	m.ObserveHTTP(http.MethodGet, "/search", http.StatusOK, 2.0)
	m.ObserveHTTP(http.MethodPost, "/memories", http.StatusCreated, 3.0)
	m.ObserveHTTP(http.MethodGet, "/z", http.StatusOK, 1.0)
	m.ObserveHTTP(http.MethodGet, "/a", http.StatusOK, 4.0)
	out1 := m.PrometheusText()
	out2 := m.PrometheusText()
	if out1 != out2 {
		t.Fatalf("output not deterministic\n---1---\n%s\n---2---\n%s", out1, out2)
	}
}

// MetricsMiddleware 自动埋点 + healthz 状态字段。
func TestMetricsMiddlewareAndHealthz(t *testing.T) {
	m := NewServerMetrics()
	// 用真实 sqlite 后端装配 APIServer（healthz 需要 Ping）
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)

	h := WithMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/memories":
				w.WriteHeader(http.StatusCreated)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}),
		MetricsMiddleware(m),
		RecoverMiddleware,
		TraceIDMiddleware,
	)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/memories", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/search", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}

	out := m.PrometheusText()
	if !strings.Contains(out, "memories_saved_total 3") {
		t.Fatalf("want saved=3\n---\n%s", out)
	}
	if !strings.Contains(out, "memories_recalled_total 2") {
		t.Fatalf("want recalled=2\n---\n%s", out)
	}

	// healthz：未执行过 consolidate 时 last_consolidate 为空
	hz := HealthzHandler(srv, b, "hash", m)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	hz.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"status":"ok"`, `"backend":"sqlite`, `"embedder":"hash"`, `"last_consolidate":""`} {
		if !strings.Contains(body, want) {
			t.Fatalf("healthz missing %q in %s", want, body)
		}
	}
}

// 记录 consolidate 时间戳后 healthz 返回 RFC3339 时间。
func TestHealthz_LastConsolidate(t *testing.T) {
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	m := NewServerMetrics()
	m.RecordConsolidate(time.Date(2026, 10, 14, 8, 30, 0, 0, time.UTC))

	hz := HealthzHandler(srv, b, "hash", m)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	hz.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"last_consolidate":"2026-10-14T08:30:00Z"`) {
		t.Fatalf("missing last_consolidate in %s", w.Body.String())
	}
}

// 后端 ping 失败时 healthz 返回 503 + degraded。
func TestHealthz_BackendDown(t *testing.T) {
	m := NewServerMetrics()
	hz := HealthzHandler(nil, &fakePingBackend{}, "hash", m)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	hz.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "degraded") {
		t.Fatalf("want degraded, got %s", w.Body.String())
	}
}

// fakePingBackend 用于模拟后端探活失败。
// 其余方法最小实现（panic 提示未被调用），仅满足 StorageBackend 接口。
type fakePingBackend struct{}

func (f *fakePingBackend) Name() string { return "fake-down" }
func (f *fakePingBackend) Ping(context.Context) error {
	return errFakeDown
}
func (f *fakePingBackend) Upsert(context.Context, *Memory) error {
	panic("unexpected Upsert")
}
func (f *fakePingBackend) Get(context.Context, string, string, string) (*Memory, error) {
	panic("unexpected Get")
}
func (f *fakePingBackend) Query(context.Context, QueryParams) ([]QueryResult, error) {
	panic("unexpected Query")
}
func (f *fakePingBackend) VectorSearch(context.Context, QueryParams) ([]QueryResult, error) {
	panic("unexpected VectorSearch")
}
func (f *fakePingBackend) CountByKind(context.Context, MemoryKind, string) (int64, error) {
	panic("unexpected CountByKind")
}
func (f *fakePingBackend) Close() error { return nil }

var errFakeDown = errors.New("fake backend down")

// ------------------------------------------------------------------
// R29-B：生命周期 / 维护 / 排水 / 探针指标
// ------------------------------------------------------------------

func TestServerMetrics_R29B_LabeledCounters(t *testing.T) {
	m := NewServerMetrics()
	m.IncStateTransition("idle", "running", "tick")
	m.IncStateTransition("idle", "running", "tick")
	m.IncMaintenanceRun("consolidate", "ok")
	m.IncMaintenanceSkipped("consolidate", "single_flight")
	m.IncReadyzFailure("read")
	m.IncReadyzFailure("read")
	m.IncReadyzFailure("ping")
	m.IncReadyzFailure("") // 空分类归一为 unknown
	m.IncDrainTimeout()
	m.ObserveBackendProbe(3.5)
	m.ObserveBackendProbe(12)

	out := m.PrometheusText()
	for _, want := range []string{
		`state_transitions_total{from="idle",to="running",reason="tick"} 2`,
		`maintenance_runs_total{job="consolidate",result="ok"} 1`,
		`maintenance_skipped_total{job="consolidate",reason="single_flight"} 1`,
		`readyz_failures_total{check="ping"} 1`,
		`readyz_failures_total{check="read"} 2`,
		`readyz_failures_total{check="unknown"} 1`,
		`drain_timeouts_total 1`,
		`# TYPE backend_probe_ms histogram`,
		`backend_probe_ms_count 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少指标行 %q\n输出:\n%s", want, out)
		}
	}
	// 直方图和校验：3.5 + 12 = 15.5。
	if !strings.Contains(out, "backend_probe_ms_sum 15.5") {
		t.Fatalf("backend_probe_ms_sum 期望 15.5\n输出:\n%s", out)
	}
	// 确定性：标签按字典序、无 map 迭代序 → 两次渲染逐字节一致。
	if out2 := m.PrometheusText(); out2 != out {
		t.Fatalf("PrometheusText 非确定性（两次输出不同）")
	}
}

// DefaultServerMetrics：同一进程内共享同一实例；NewServerMetrics 后默认实例被更新。
func TestServerMetrics_DefaultInstance(t *testing.T) {
	m := NewServerMetrics()
	if got := DefaultServerMetrics(); got != m {
		t.Fatalf("DefaultServerMetrics 应返回最近一次 NewServerMetrics 的实例")
	}
	m.IncDrainTimeout()
	if !strings.Contains(DefaultServerMetrics().PrometheusText(), "drain_timeouts_total 1") {
		t.Fatalf("默认实例未记录方法调用")
	}
}
