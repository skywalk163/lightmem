// backend_qdrant_httptest_test.go — R16 S1 Qdrant HTTP 契约测试（离线 mock Qdrant）。
//
// 背景：0.88 的 Qdrant 端口 6333 不在 pf 放行范围（仅 443/8443），真机联调
// 只能在维护窗口做。本文件用 httptest 起一个假 Qdrant REST 服务，端到端验证
// backend_qdrant.go 的全部 HTTP 交互路径（建集合 / Upsert / Get / Query /
// VectorSearch / UpdateLayer / Delete / Export / scroll / API key / 向量保留），
// 证明 stdlib-only HTTP 实现与 Qdrant REST 契约一致。
//
// 只增不改：不修改 backend_qdrant.go / 既有接口，纯新增测试文件。
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockQdrant 是内存版 Qdrant REST 服务（只实现 backend 用到的端点）。
type mockQdrant struct {
	mu      sync.Mutex
	exists  bool
	points  map[string]*qdrantPoint
	creates int
	upserts int
	search  int
	deletes int
}

func newMockQdrant() *mockQdrant {
	return &mockQdrant{points: map[string]*qdrantPoint{}}
}

func (m *mockQdrant) serve(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(m.handler))
	t.Cleanup(s.Close)
	return s
}

func (m *mockQdrant) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/collections/lightmem_memories"):
		if !m.exists {
			qtestWriteJSON(w, http.StatusNotFound, map[string]any{"status": map[string]any{"error": "Not found"}})
			return
		}
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "green"}})

	case r.Method == http.MethodPut && strings.HasSuffix(p, "/collections/lightmem_memories"):
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			qtestWriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		m.exists = true
		m.creates++
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": true})

	case r.Method == http.MethodPut && strings.HasSuffix(p, "/points"):
		var body struct {
			Points []qdrantPoint `json:"points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			qtestWriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		for i := range body.Points {
			cp := body.Points[i]
			m.points[cp.ID] = &cp
		}
		m.upserts++
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "completed"}})

	case r.Method == http.MethodGet && strings.Contains(p, "/points/"):
		id := p[strings.LastIndex(p, "/")+1:]
		pt, ok := m.points[id]
		if !ok {
			qtestWriteJSON(w, http.StatusNotFound, map[string]any{"status": map[string]any{"error": "Not found"}})
			return
		}
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": pt})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/points/scroll"):
		out := make([]qdrantPoint, 0, len(m.points))
		for _, pt := range m.points {
			out = append(out, *pt)
		}
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"points": out}})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/points/search"):
		m.search++
		var body struct {
			Limit int `json:"limit"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		lim := body.Limit
		if lim <= 0 {
			lim = 10
		}
		out := make([]map[string]any, 0, len(m.points))
		for _, pt := range m.points {
			if len(out) >= lim {
				break
			}
			out = append(out, map[string]any{"id": pt.ID, "score": 0.9, "payload": pt.Payload})
		}
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": out})

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/points/delete"):
		var body struct {
			Points []string `json:"points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			qtestWriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		for _, id := range body.Points {
			delete(m.points, id)
		}
		m.deletes++
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "completed"}})

	default:
		qtestWriteJSON(w, http.StatusNotFound, map[string]any{"status": map[string]any{"error": "route not mocked: " + p}})
	}
}

func qtestWriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// newTestQdrantBackend 在 mock Qdrant 服务上构造真实 backend 实例。
func newTestQdrantBackend(t *testing.T, mock *mockQdrant) *qdrantBackend {
	t.Helper()
	s := mock.serve(t)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}
	cfg := &StorageConfig{
		Qdrant: &QdrantConfig{Host: u.Hostname(), Port: qtestAtoi(u.Port())},
	}
	got, err := newQdrantBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newQdrantBackend: %v", err)
	}
	gb, ok := got.(*qdrantBackend)
	if !ok {
		t.Fatalf("newQdrantBackend returned %T, want *qdrantBackend", got)
	}
	return gb
}

func qtestAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func mkMemory(id, fact string, kind MemoryKind) *Memory {
	return &Memory{
		MemoryID:   id,
		Fact:       fact,
		Layer:      LayerL1,
		TenantID:   "t1",
		UserID:     "u1",
		Visibility: VisPrivate, // R17 基线修复：零值 Visibility 不匹配 canView 任何 case（全代码库 fixture 惯例为显式设置）
		MemoryKind: kind,
		Confidence: 0.8,
		RecordedAt: time.Now().UTC().Truncate(time.Second),
		ValidFrom:  "2026-10-01",
		ValidUntil: "2026-12-31",
	}
}

// TestQdrantHTTPEndToEnd 端到端走一遍 StorageBackend 主路径。
func TestQdrantHTTPEndToEnd(t *testing.T) {
	mock := newMockQdrant()
	b := newTestQdrantBackend(t, mock)
	ctx := context.Background()

	if mock.creates != 1 {
		t.Fatalf("expected 1 create, got %d", mock.creates)
	}
	if !mock.exists {
		t.Fatal("mock collection should exist")
	}
	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	m1 := mkMemory("m001", "客户A 偏好 简洁文档", KindPreference)
	m2 := mkMemory("m002", "客户B 偏好 长报告", KindPreference)
	if err := b.Upsert(ctx, m1); err != nil {
		t.Fatalf("Upsert m1: %v", err)
	}
	if err := b.Upsert(ctx, m2); err != nil {
		t.Fatalf("Upsert m2: %v", err)
	}
	if mock.upserts != 2 {
		t.Fatalf("expected 2 upserts, got %d", mock.upserts)
	}

	got, err := b.Get(ctx, "m001", "u1", "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Fact != m1.Fact || got.MemoryID != "m001" {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if _, err := b.Get(ctx, "m001", "u_other", "t1"); err == nil {
		t.Fatal("expected visibility error for foreign viewer")
	}

	res, err := b.Query(ctx, QueryParams{Text: "客户A 简洁", TenantID: "t1", Viewer: "u1", TopK: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected query hits")
	}
	if res[0].MemoryID != "m001" {
		t.Fatalf("expected m001 first, got %s", res[0].MemoryID)
	}

	vs, err := b.VectorSearch(ctx, QueryParams{
		Vector: make([]float32, qdrantDefaultDim), TenantID: "t1", Viewer: "u1", TopK: 10,
	})
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("expected 2 vector results, got %d", len(vs))
	}
	if mock.search != 1 {
		t.Fatalf("expected 1 search call, got %d", mock.search)
	}

	vs2, err := b.VectorSearch(ctx, QueryParams{TenantID: "t1", Viewer: "u1", TopK: 5})
	if err != nil {
		t.Fatalf("VectorSearch empty vector: %v", err)
	}
	if len(vs2) == 0 {
		t.Fatal("expected degraded query results")
	}

	if err := b.UpdateLayer(ctx, "m001", LayerL2); err != nil {
		t.Fatalf("UpdateLayer: %v", err)
	}
	got2, _ := b.Get(ctx, "m001", "u1", "t1")
	if got2.Layer != LayerL2 {
		t.Fatalf("UpdateLayer mismatch: %s", got2.Layer)
	}

	if n, err := b.CountByKind(ctx, KindPreference, "t1"); err != nil || n != 2 {
		t.Fatalf("CountByKind: n=%d err=%v", n, err)
	}

	if err := b.DeleteByID(ctx, "m002"); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	if mock.deletes != 1 {
		t.Fatalf("expected 1 delete, got %d", mock.deletes)
	}
	if _, err := b.Get(ctx, "m002", "u1", "t1"); err == nil {
		t.Fatal("expected error after delete")
	}
}

// TestQdrantHTTPComplianceAndLayer 验证 ComplianceBackend 与 LayerScanner 走 HTTP。
func TestQdrantHTTPComplianceAndLayer(t *testing.T) {
	mock := newMockQdrant()
	b := newTestQdrantBackend(t, mock)
	ctx := context.Background()

	m1 := mkMemory("c001", "机密事项", KindEvent)
	m2 := mkMemory("c002", "另一租户", KindEvent)
	m2.TenantID = "t2"
	for _, m := range []*Memory{m1, m2} {
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	exp, err := b.ExportByScope(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("ExportByScope: %v", err)
	}
	if len(exp) != 1 || exp[0].MemoryID != "c001" {
		t.Fatalf("Export mismatch: %+v", exp)
	}

	sc, err := b.ScanLayer(ctx, LayerL1, "t1", 100)
	if err != nil {
		t.Fatalf("ScanLayer: %v", err)
	}
	if len(sc) != 1 || sc[0].MemoryID != "c001" {
		t.Fatalf("ScanLayer mismatch: %+v", sc)
	}

	ids, err := b.DeleteByScope(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if len(ids) != 1 || ids[0] != "c001" {
		t.Fatalf("DeleteByScope mismatch: %v", ids)
	}
	if mock.deletes != 1 {
		t.Fatalf("expected 1 delete, got %d", mock.deletes)
	}
	if n, _ := b.CountByKind(ctx, KindEvent, "t2"); n != 1 {
		t.Fatalf("expected t2 to retain c002, got %d", n)
	}
}

// TestQdrantHTTPUpsertPreservesVector 验证 Upsert 更新时保留已有向量。
func TestQdrantHTTPUpsertPreservesVector(t *testing.T) {
	mock := newMockQdrant()
	mock.mu.Lock()
	mock.points["v001"] = &qdrantPoint{
		ID:      "v001",
		Vector:  make([]float32, qdrantDefaultDim),
		Payload: map[string]any{"_memory": "{}"},
	}
	mock.points["v001"].Vector[0] = 0.5
	mock.mu.Unlock()
	b := newTestQdrantBackend(t, mock)

	if err := b.Upsert(context.Background(), mkMemory("v001", "已有向量记忆", KindFact)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	mock.mu.Lock()
	pt := mock.points["v001"]
	mock.mu.Unlock()
	if pt == nil {
		t.Fatal("point missing after upsert")
	}
	if len(pt.Vector) != qdrantDefaultDim || pt.Vector[0] != 0.5 {
		t.Fatalf("expected vector preserved, got len=%d first=%.2f", len(pt.Vector), pt.Vector[0])
	}
}

// TestQdrantHTTPAPIKeyHeader 验证配置了 api_key 时请求带 api-key 头。
func TestQdrantHTTPAPIKeyHeader(t *testing.T) {
	var gotKey string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("api-key")
		qtestWriteJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "green"}})
	}))
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	cfg := &StorageConfig{Qdrant: &QdrantConfig{
		Host: u.Hostname(), Port: qtestAtoi(u.Port()), APIKey: "secret-abc",
	}}
	if _, err := newQdrantBackend(context.Background(), cfg); err != nil {
		t.Fatalf("newQdrantBackend: %v", err)
	}
	if gotKey != "secret-abc" {
		t.Fatalf("expected api-key header, got %q", gotKey)
	}
}
