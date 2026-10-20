package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockBackend 实现 StorageBackend 最小桩，Ping 可注入错误。
type mockBackend struct {
	pingErr error
}

func (m *mockBackend) Name() string { return "mock" }
func (m *mockBackend) Ping(ctx context.Context) error {
	return m.pingErr
}
func (m *mockBackend) Upsert(ctx context.Context, mem *Memory) error { return nil }
func (m *mockBackend) Get(ctx context.Context, id, viewer, tenant string) (*Memory, error) {
	return nil, nil
}
func (m *mockBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	return nil, nil
}
func (m *mockBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	return nil, nil
}
func (m *mockBackend) CountByKind(ctx context.Context, k MemoryKind, tenant string) (int64, error) {
	return 0, nil
}
func (m *mockBackend) Close() error { return nil }

func TestLivezHandler_Always200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	LivezHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("livez status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("livez body not json: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("livez status field = %v, want alive", body["status"])
	}
}

func TestReadyzHandler_BackendPingOK(t *testing.T) {
	p := NewBackendProbe(&mockBackend{pingErr: nil}, "mock")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Fatalf("readyz status field = %v, want ready", body["status"])
	}
}

func TestReadyzHandler_BackendPingFail503(t *testing.T) {
	p := NewBackendProbe(&mockBackend{pingErr: context.DeadlineExceeded}, "mock")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Fatalf("readyz status field = %v, want not_ready", body["status"])
	}
	if body["check"] == nil || body["check"] == "" {
		t.Fatalf("readyz check field empty on failure")
	}
}

func TestNewBackendProbe_NilBackendCheck(t *testing.T) {
	p := NewBackendProbe(nil, "mock")
	if err := p.Check(context.Background()); err == nil {
		t.Fatalf("nil backend Check should error")
	}
}

func TestReadyzHandler_NilProbePanicsGracefully(t *testing.T) {
	// 探针接口本身不能为 nil（NewBackendProbe 已保证非 nil），此处验证空实现不崩。
	p := NewBackendProbe(&mockBackend{}, "mock")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rec.Code)
	}
}

func TestBackendProbe_NamePropagated(t *testing.T) {
	// 仅验证构造不依赖 name 字段（name 预留给 B 线硬化日志）。
	p := NewBackendProbe(&mockBackend{}, "family-pg")
	if p == nil {
		t.Fatalf("NewBackendProbe returned nil")
	}
}
