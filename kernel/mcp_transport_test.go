// R29-B：MCP 健康端点（/healthz + upstream_status；新增 /readyz）。
package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mcpUpstreamStub 起一个最小上游：/livez 与 /readyz 状态可控。
func mcpUpstreamStub(t *testing.T, livezCode, readyzCode int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(livezCode)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(readyzCode)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func mcpHandlerFor(t *testing.T, baseURL string) http.Handler {
	t.Helper()
	return NewMCPHTTPServer(NewMCPServer(baseURL)).Handler()
}

// 上游健康：/healthz 200 且 upstream_status=ok；/readyz 200。
func TestMCPHTTP_HealthzUpstreamStatusOK(t *testing.T) {
	up := mcpUpstreamStub(t, http.StatusOK, http.StatusOK)
	h := mcpHandlerFor(t, up.URL)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body not json: %v", err)
	}
	if body["upstream_status"] != "ok" {
		t.Fatalf("upstream_status = %v, want ok", body["upstream_status"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", rec.Code)
	}
}

// R10：上游不可达 → /healthz 仍 200 且 upstream_status=unreachable；/readyz 503。
func TestMCPHTTP_UpstreamUnreachable(t *testing.T) {
	down := mcpUpstreamStub(t, http.StatusOK, http.StatusOK)
	url := down.URL
	down.Close() // 连接拒绝：确定性不可达

	h := mcpHandlerFor(t, url)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("上游不可达时 /healthz 仍须 200（liveness 不依赖上游），got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["upstream_status"] != "unreachable" {
		t.Fatalf("upstream_status = %v, want unreachable", body["upstream_status"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("上游不可达时 /readyz = %d, want 503", rec.Code)
	}
	body = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "degraded" {
		t.Fatalf("/readyz status = %v, want degraded", body["status"])
	}
}

// 上游可达但未就绪（/livez 200、/readyz 503）：/healthz ok，/readyz 503（就绪态如实传播）。
func TestMCPHTTP_UpstreamNotReady(t *testing.T) {
	up := mcpUpstreamStub(t, http.StatusOK, http.StatusServiceUnavailable)
	h := mcpHandlerFor(t, up.URL)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["upstream_status"] != "ok" {
		t.Fatalf("/healthz = %d upstream_status=%v, want 200/ok", rec.Code, body["upstream_status"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("上游未就绪时 /readyz = %d, want 503", rec.Code)
	}
}

// 未配置上游（空 baseURL）→ 不谎报 ok：/healthz unreachable，/readyz 503。
func TestMCPHTTP_NoUpstream(t *testing.T) {
	// NewMCPServer("") 会回落默认地址；这里直接构造空 baseURL 的 MCPServer 以测边界。
	h := NewMCPHTTPServer(&MCPServer{baseURL: ""}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["upstream_status"] != "unreachable" {
		t.Fatalf("空上游 upstream_status = %v, want unreachable", body["upstream_status"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("空上游 /readyz = %d, want 503", rec.Code)
	}
}
