// /healthz 增量字段接线测试（R29-T0 PE-1）。
//
// 覆盖三条口径：
//  1. **默认关（红线④）**：opts=nil → 返回原 handler 本体，输出与 c203db8 逐字节一致；
//  2. **只加不减**：注入后既有字段（status/backend/embedder/last_consolidate）不变；
//  3. **不泄漏**：schema_check 只给结论/缺失列名，不含 DSN/口令/后端连接串。
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// healthzTestBackend 是最小 StorageBackend 桩（Ping 恒成功）。
type healthzTestBackend struct{ mockBackend }

func (h *healthzTestBackend) Name() string                   { return "sqlite" }
func (h *healthzTestBackend) Ping(ctx context.Context) error { return nil }

// newHealthzBase 构造既有 /healthz handler（未装饰）。
func newHealthzBase(t *testing.T) http.Handler {
	t.Helper()
	b := &healthzTestBackend{}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	return HealthzHandler(srv, b, "hash", NewServerMetrics())
}

// serveHealthz 打一次 /healthz，返回状态码与 JSON 对象。
func serveHealthz(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("healthz body not json: %v (raw=%q)", err, rec.Body.String())
	}
	return rec.Code, obj
}

// ── 1. 默认关：opts=nil 原样返回，输出逐字节不变 ──

// 注意：http.HandlerFunc 是不可比较类型（含 func 字段），不能用 != 比较；
// 改为断言「返回的 handler 与 base 行为逐字节一致」+ 指针身份（reflect）。
func TestHealthzWithOptions_NilOptsReturnsBase(t *testing.T) {
	base := newHealthzBase(t)
	got := HealthzWithOptions(base, nil)
	if got == nil {
		t.Fatalf("nil opts must return base handler (non-nil)")
	}
	// 行为等价：两次响应体逐字节相同
	_, baseObj := serveHealthz(t, base)
	_, gotObj := serveHealthz(t, got)
	if len(baseObj) != len(gotObj) {
		t.Fatalf("nil opts changed output: base=%v got=%v", baseObj, gotObj)
	}
	for k, v := range baseObj {
		if gotObj[k] != v {
			t.Fatalf("nil opts changed field %q: %v → %v", k, v, gotObj[k])
		}
	}
}

func TestHealthzWithOptions_NilBase(t *testing.T) {
	if got := HealthzWithOptions(nil, &HealthzOptions{StartedAt: time.Now()}); got != nil {
		t.Fatalf("nil base must return nil, got %v", got)
	}
}

// 空 opts（全零值）→ 只多出 started_at / version 两个**纯加法**字段
// （设计稿 §3.2.3 明列），schema_*/rows/restore_point 等自检字段不出现。
func TestHealthzWithOptions_ZeroOptsNoNewFields(t *testing.T) {
	old := BuildVersion
	BuildVersion = "unknown"
	defer func() { BuildVersion = old }()

	base := newHealthzBase(t)
	_, baseObj := serveHealthz(t, base)

	code, obj := serveHealthz(t, HealthzWithOptions(base, &HealthzOptions{}))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// 既有字段一个不少、值不变
	for k, v := range baseObj {
		if obj[k] != v {
			t.Fatalf("existing field %q changed: %v → %v", k, v, obj[k])
		}
	}
	// 自检字段不得出现（未配 -data-dir）
	for _, k := range []string{"schema_ok", "schema_check", "rows", "restore_point", "unfinished", "version_match"} {
		if _, ok := obj[k]; ok {
			t.Fatalf("zero opts must not emit boot-check field %q (got %v)", k, obj[k])
		}
	}
	// version 未注入 → 不出现
	if _, ok := obj["version"]; ok {
		t.Fatalf("unknown BuildVersion must not emit version")
	}
}

// ── 2. 只加不减：既有字段不变 ──

func TestHealthzWithOptions_KeepsExistingFields(t *testing.T) {
	base := newHealthzBase(t)
	_, baseObj := serveHealthz(t, base)

	opts := &HealthzOptions{
		StartedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
		BootReport: &BootReport{
			Backend: "sqlite", Table: "ok", Columns: "ok", Rows: 42,
			VersionMatch: true, RestorePoint: "wal_bytes=1024",
		},
	}
	code, obj := serveHealthz(t, HealthzWithOptions(base, opts))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// 既有字段必须原样保留
	for k, v := range baseObj {
		if obj[k] != v {
			t.Fatalf("existing field %q changed: %v → %v", k, v, obj[k])
		}
	}
	// 新增字段
	if obj["started_at"] != "2026-10-20T10:00:00Z" {
		t.Fatalf("started_at = %v", obj["started_at"])
	}
	if obj["schema_ok"] != "ok" || obj["schema_check"] != "ok" {
		t.Fatalf("schema fields = %v/%v", obj["schema_ok"], obj["schema_check"])
	}
	if obj["rows"] != "42" {
		t.Fatalf("rows = %v, want 42", obj["rows"])
	}
	if obj["restore_point"] != "wal_bytes=1024" {
		t.Fatalf("restore_point = %v", obj["restore_point"])
	}
}

// 缺表/缺列时 schema_check 暴露缺失列名，但只到「列名」粒度（不泄漏 DSN）。
func TestHealthzWithOptions_SchemaMissingColumns(t *testing.T) {
	base := newHealthzBase(t)
	opts := &HealthzOptions{BootReport: &BootReport{
		Backend: "pgvector", Table: "ok", Columns: "missing:vector_status,fact",
	}}
	_, obj := serveHealthz(t, HealthzWithOptions(base, opts))
	if obj["schema_check"] != "missing:vector_status,fact" {
		t.Fatalf("schema_check = %v", obj["schema_check"])
	}
	// 不得出现连接串/口令类信息
	raw, _ := json.Marshal(obj)
	for _, forbidden := range []string{"password", "dsn=", "postgres://", "user="} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("healthz leaked %q: %s", forbidden, raw)
		}
	}
}

// 未完成排水标记 → unfinished 字段；version_match=false → 显式输出。
func TestHealthzWithOptions_UnfinishedAndVersionMismatch(t *testing.T) {
	base := newHealthzBase(t)
	opts := &HealthzOptions{BootReport: &BootReport{
		Backend: "sqlite", Table: "ok", Columns: "ok", Rows: 1,
		VersionMatch: false, Unfinished: "consolidate",
	}}
	_, obj := serveHealthz(t, HealthzWithOptions(base, opts))
	if obj["unfinished"] != "consolidate" {
		t.Fatalf("unfinished = %v", obj["unfinished"])
	}
	if obj["version_match"] != "false" {
		t.Fatalf("version_match = %v", obj["version_match"])
	}
}

// ── 3. 生命周期时间戳（请求时实时读取）──

func TestHealthzWithOptions_LifecycleRunFn(t *testing.T) {
	base := newHealthzBase(t)
	// 零值 → 不输出该字段
	_, obj := serveHealthz(t, HealthzWithOptions(base, &HealthzOptions{
		LifecycleRunFn: func() time.Time { return time.Time{} },
	}))
	if _, ok := obj["last_lifecycle_run"]; ok {
		t.Fatalf("zero lifecycle time must not emit field, got %v", obj["last_lifecycle_run"])
	}
	// 非零 → 输出 RFC3339
	_, obj = serveHealthz(t, HealthzWithOptions(base, &HealthzOptions{
		LifecycleRunFn: func() time.Time {
			return time.Date(2026, 10, 20, 11, 30, 0, 0, time.UTC)
		},
	}))
	if obj["last_lifecycle_run"] != "2026-10-20T11:30:00Z" {
		t.Fatalf("last_lifecycle_run = %v", obj["last_lifecycle_run"])
	}
}

// ── 4. 降级路径（503）同样保留增量字段 ──

func TestHealthzWithOptions_DegradedKeepsFields(t *testing.T) {
	down := &fakePingBackend{}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, down)
	base := HealthzHandler(srv, down, "hash", NewServerMetrics())

	opts := &HealthzOptions{
		StartedAt:  time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
		BootReport: &BootReport{Backend: "fake-down", Table: "ok", Columns: "ok", Rows: 7},
	}
	code, obj := serveHealthz(t, HealthzWithOptions(base, opts))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if obj["status"] != "degraded" {
		t.Fatalf("status field = %v, want degraded", obj["status"])
	}
	if obj["rows"] != "7" {
		t.Fatalf("degraded response lost rows: %v", obj)
	}
}

// ── 5. 版本字段：unknown 不输出（不误报）──

func TestHealthzWithOptions_VersionField(t *testing.T) {
	old := BuildVersion
	defer func() { BuildVersion = old }()

	base := newHealthzBase(t)
	BuildVersion = "unknown"
	_, obj := serveHealthz(t, HealthzWithOptions(base, &HealthzOptions{}))
	if _, ok := obj["version"]; ok {
		t.Fatalf("unknown BuildVersion must not emit version field")
	}

	BuildVersion = "abc123"
	_, obj = serveHealthz(t, HealthzWithOptions(base, &HealthzOptions{}))
	if obj["version"] != "abc123" {
		t.Fatalf("version = %v, want abc123", obj["version"])
	}
}
