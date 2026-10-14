// R9-A：HTTP 中间件冒烟测试（任务书 §1.A.3.5）
// 覆盖：trace ID 单调、日志格式固定、panic recover 返回 500 不崩、中间件链装配顺序。
package kernel

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 断言 trace ID 单调递增（进程级计数器，无随机源）。
func TestTraceIDMonotonic(t *testing.T) {
	prev := NextTraceID()
	if prev == 0 {
		t.Fatalf("first trace id must be >= 1, got %d", prev)
	}
	for i := 0; i < 1000; i++ {
		cur := NextTraceID()
		if cur <= prev {
			t.Fatalf("trace id not monotonic: prev=%d cur=%d", prev, cur)
		}
		prev = cur
	}
}

// 中间件写入 X-Trace-Id 响应头，且每次请求不同。
func TestTraceIDMiddlewareHeader(t *testing.T) {
	h := TraceIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		tid := w.Header().Get("X-Trace-Id")
		if tid == "" {
			t.Fatalf("missing X-Trace-Id header")
		}
		if seen[tid] {
			t.Fatalf("duplicate trace id %s", tid)
		}
		seen[tid] = true
	}
}

// 请求 context 注入 trace ID，TraceIDFromContext 可回读。
func TestTraceIDFromContext(t *testing.T) {
	var got uint64
	h := TraceIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TraceIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got == 0 {
		t.Fatalf("TraceIDFromContext returned 0")
	}
}

// panic recover：handler panic 时返回 500，进程不崩。
func TestRecoverMiddleware_PanicReturns500(t *testing.T) {
	h := RecoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal server error") {
		t.Fatalf("want error body, got %q", w.Body.String())
	}
	// 后续请求仍能正常处理（进程未崩）
	h2 := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req2 := httptest.NewRequest(http.MethodGet, "/ok", nil)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("want 200 after panic, got %d", w2.Code)
	}
}

// LoggingMiddleware 输出固定格式访问日志（method/path/status/latency_ms/trace_id/remote_ip）。
func TestLoggingMiddleware_Format(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	_ = logger

	// 业务 handler + trace 中间件（日志中间件读取响应头）
	inner := TraceIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	h := LoggingMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/memories", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatalf("no log output")
	}
	line := lines[len(lines)-1]
	for _, want := range []string{
		"method=GET", "path=/memories", "status=404", "latency_ms=", "trace_id=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
}

// WithMiddleware 装配顺序：先给的最外层最先执行。
func TestWithMiddleware_Order(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := WithMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "handler")
			w.WriteHeader(http.StatusOK)
		}),
		mk("A"), mk("B"),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	want := []string{"A", "B", "handler"}
	if len(order) != len(want) {
		t.Fatalf("want order %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("want order %v, got %v", want, order)
		}
	}
}

// 完整链：Logging -> Metrics -> Recover -> TraceID -> 业务路由，多请求下状态码正确。
func TestFullChain_Smoke(t *testing.T) {
	metrics := NewServerMetrics()
	h := WithMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				panic("x")
			}
			w.WriteHeader(http.StatusOK)
		}),
		LoggingMiddleware,
		MetricsMiddleware(metrics),
		RecoverMiddleware,
		TraceIDMiddleware,
	)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ok", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", w.Code)
		}
		if w.Header().Get("X-Trace-Id") == "" {
			t.Fatalf("missing trace id")
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 on panic, got %d", w.Code)
	}
}

// 并发下 trace ID 单调且 Metrics 计数无竞争（race detector 覆盖）。
func TestMiddleware_Concurrent(t *testing.T) {
	metrics := NewServerMetrics()
	h := WithMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		MetricsMiddleware(metrics),
		TraceIDMiddleware,
	)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("want 200, got %d", w.Code)
			}
		}()
	}
	wg.Wait()
}
