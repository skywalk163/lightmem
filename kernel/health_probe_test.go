package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockBackend 实现 StorageBackend 最小桩，Ping 可注入错误。
// 注意：其读探针走 readProbe 的 default 分支（非 sqlite/pg），退化为仅 Ping。
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

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (raw=%q)", err, rec.Body.String())
	}
	return body
}

func TestLivezHandler_Always200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	LivezHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("livez status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "alive" {
		t.Fatalf("livez status field = %v, want alive", body["status"])
	}
}

// R29-B：/readyz 成功响应最小化为 {"status":"ok","check":""}，不再输出后端名/profile/version。
func TestReadyzHandler_BackendPingOK(t *testing.T) {
	p := NewBackendProbe(&mockBackend{pingErr: nil}, "mock")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("readyz status field = %v, want ok", body["status"])
	}
	if _, has := body["check"]; !has {
		t.Fatalf("readyz 应仅含 {status,check}，缺 check 字段: %v", body)
	}
	for _, forbidden := range []string{"backend", "profile", "version", "embedder"} {
		if _, has := body[forbidden]; has {
			t.Fatalf("readyz 泄露字段 %q（响应须最小化）: %v", forbidden, body)
		}
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
	body := decodeBody(t, rec)
	if body["status"] != "degraded" {
		t.Fatalf("readyz status field = %v, want degraded", body["status"])
	}
	if s, _ := body["check"].(string); s == "" {
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
	// 仅验证构造不依赖 name 字段（name 预留给硬化日志）。
	p := NewBackendProbe(&mockBackend{}, "family-pg")
	if p == nil {
		t.Fatalf("NewBackendProbe returned nil")
	}
}

// ── R29-B 读探针（SELECT 1 FROM memories LIMIT 1）──

// R8：SQLite 空表（表存在、无行）→ 读探针 ErrNoRows → 合法 → 200。
func TestBackendProbe_ReadProbe_EmptyTableOK(t *testing.T) {
	backend, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer backend.Close()

	p := NewBackendProbe(backend, "solo")
	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("空表应就绪（ErrNoRows 合法），got %v", err)
	}

	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("空表 /readyz = %d, want 200", rec.Code)
	}
}

// R7 复现（SQLite 等价）：memories 表缺失 → 读探针报错 → 503（修 L-244）。
func TestBackendProbe_ReadProbe_TableMissing503(t *testing.T) {
	backend, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer backend.Close()

	sb, ok := backend.(*sqliteBackend)
	if !ok {
		t.Fatalf("backend 应为 *sqliteBackend")
	}
	sb.mu.Lock()
	if _, err := sb.db.Exec(`DROP TABLE memories`); err != nil {
		sb.mu.Unlock()
		t.Fatalf("drop table: %v", err)
	}
	sb.mu.Unlock()

	p := NewBackendProbe(backend, "solo")
	err = p.Check(context.Background())
	if err == nil {
		t.Fatalf("缺表应报错（L-244 复现），却返回 nil")
	}
	if name := probeCheckName(err); name != "read" {
		t.Fatalf("失败分类 = %q, want read（err=%v）", name, err)
	}

	rec := httptest.NewRecorder()
	ReadyzHandler(p).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺表 /readyz = %d, want 503", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "degraded" {
		t.Fatalf("缺表 status = %v, want degraded", body["status"])
	}
}

// pgBackend 分支：读探针复用同一段逻辑（此处以 sqlite 驱动的 *sql.DB 注入 pgBackend 桩，
// 端口无关、离线可跑）。
func TestBackendProbe_ReadProbe_PgBranch(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	// 表存在但空 → ErrNoRows → 合法。
	if _, err := db.Exec(`CREATE TABLE memories (memory_id TEXT PRIMARY KEY, data TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	pgEmpty := &pgBackend{db: db, table: "memories"}
	if err := readProbe(context.Background(), pgEmpty); err != nil {
		t.Fatalf("pg 分支空表应合法，got %v", err)
	}

	// 缺表 → 报错。
	pgMissing := &pgBackend{db: db, table: "memories"}
	if _, err := db.Exec(`DROP TABLE memories`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := readProbe(context.Background(), pgMissing); err == nil {
		t.Fatalf("pg 分支缺表应报错，却返回 nil")
	}
}

// 失败分类：Ping 失败 → "ping"；nil 后端 → "backend"。
func TestProbeCheckName_Classification(t *testing.T) {
	if got := probeCheckName(&probeCheckError{check: "ping", err: context.DeadlineExceeded}); got != "ping" {
		t.Fatalf("probeCheckName(ping) = %q", got)
	}
	if got := probeCheckName(errNilBackend); got != "backend" {
		t.Fatalf("probeCheckName(nilBackend) = %q", got)
	}
	if got := probeCheckName(context.Canceled); got != "unknown" {
		t.Fatalf("probeCheckName(other) = %q", got)
	}
}

// countingProbe 计数探针：验证 TTL 缓存确实把高频 HTTP 请求收敛为低频真实探测。
type countingProbe struct {
	calls atomic.Int64
	err   error
}

func (c *countingProbe) Check(ctx context.Context) error {
	c.calls.Add(1)
	return c.err
}

// R25 逻辑内核（单测版）：TTL 内连打多次 /readyz，真实探测次数应远小于请求次数。
func TestReadyzHandler_TTLCacheCollapsesProbes(t *testing.T) {
	cp := &countingProbe{}
	c := newReadyzCache(cp, 30*time.Second, time.Second)
	defer c.stopLoop()
	// 构造时已同步探 1 次。
	if n := cp.calls.Load(); n != 1 {
		t.Fatalf("构造后真实探测次数 = %d, want 1", n)
	}

	h := ReadyzHandler(cp) // 独立 handler（会各自起缓存/后台 goroutine）
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 /readyz = %d, want 200", i, rec.Code)
		}
	}
	// 30s TTL 内，50 次请求不应触发 50 次真实探测（容忍构造/兜底各 1 次）。
	if n := cp.calls.Load(); n > 3 {
		t.Fatalf("TTL 缓存未生效：50 次请求触发 %d 次真实探测", n)
	}
}

// 缓存的失败态同样被缓存（TTL 内不重复打库）。
func TestReadyzCache_FailureCached(t *testing.T) {
	cp := &countingProbe{err: &probeCheckError{check: "read", err: context.DeadlineExceeded}}
	c := newReadyzCache(cp, 30*time.Second, time.Second)
	defer c.stopLoop()
	ok, check, fresh := c.status(time.Now())
	if ok || !fresh {
		t.Fatalf("失败态缓存：ok=%v fresh=%v, want false/true", ok, fresh)
	}
	if !strings.Contains(check, "read") {
		t.Fatalf("失败 check 应含分类 read，got %q", check)
	}
}
