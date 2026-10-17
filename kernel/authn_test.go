// R22 认证器与 token 存储测试（docs/token-management-design.md §4-§5）
//
// 覆盖：IdentityKind 字符串、scope 细粒度判定（空=全部/精确/通配）、RBAC 角色
// （admin 隐式 auditor）、token 哈希与 hint、scope 归一化、DDL 红线（只 CREATE）、
// SQLite 实际建表、内部 DB 认证器（成功/吊销/过期/无匹配）、Bearer 请求提取。
package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newAuthSQLite 在内存 SQLite 上建 auth 表，返回可用的 *sql.DB。
func newAuthSQLite(t testing.TB) *sqliteBackend {
	t.Helper()
	b := newTestSQLiteBackend(t)
	if err := EnsureAuthTables(context.Background(), b.db); err != nil {
		t.Fatalf("EnsureAuthTables: %v", err)
	}
	return b
}

// seedAgentToken 插入一条 agent token。
func seedAgentToken(t testing.TB, b *sqliteBackend, raw, agentID, tenantID string) string {
	t.Helper()
	id := "at-" + tokenHint(raw)
	now := formatTime(timeNow())
	_, err := b.db.Exec(`INSERT INTO agent_tokens
		(id, token_hash, token_hint, agent_id, user_id, tenant_id, scope, created_at)
		VALUES (?, ?, ?, ?, '', ?, 'memories:write,goals:read', ?)`,
		id, tokenHash(raw), tokenHint(raw), agentID, tenantID, now)
	if err != nil {
		t.Fatalf("seed agent token: %v", err)
	}
	return id
}

func TestIdentityKindString(t *testing.T) {
	cases := []struct {
		k   IdentityKind
		exp string
	}{
		{IdentityKindAgent, "agent"},
		{IdentityKindUser, "user"},
		{IdentityKind(99), "agent"}, // 未知 → 视为 agent（兼容 R21）
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.exp {
			t.Errorf("kind=%d: want %q got %q", c.k, c.exp, got)
		}
	}
}

func TestScopeAllows(t *testing.T) {
	// 空 scope = 全部权限（保留测试版便利，设计文档 §11）
	if id := (Identity{}); !id.ScopeAllows("memories:write") {
		t.Error("empty scope: want allow all")
	}
	// 精确匹配
	if id := (Identity{Scope: []string{"memories:write"}}); !id.ScopeAllows("memories:write") {
		t.Error("exact scope: want allow")
	}
	if id := (Identity{Scope: []string{"memories:write"}}); id.ScopeAllows("memories:read") {
		t.Error("mismatch scope: want deny")
	}
	// 通配
	if id := (Identity{Scope: []string{"memories:*"}}); !id.ScopeAllows("memories:read") {
		t.Error("wildcard scope: want allow")
	}
	if id := (Identity{Scope: []string{"*"}}); !id.ScopeAllows("admin:manage") {
		t.Error("star scope: want allow all")
	}
	// 多 scope
	if id := (Identity{Scope: []string{"goals:read", "events:write"}}); !id.ScopeAllows("events:write") {
		t.Error("multi scope: want allow")
	}
}

func TestHasRole(t *testing.T) {
	if id := (Identity{}); id.HasRole("user") {
		t.Error("empty roles: want not admin/user")
	}
	admin := Identity{Roles: []string{"admin"}}
	if !admin.HasRole("admin") {
		t.Error("admin role: want admin")
	}
	// admin 隐式 auditor（只读权）
	if !admin.HasRole("auditor") {
		t.Error("admin role: want implicit auditor")
	}
	user := Identity{Roles: []string{"user"}}
	if user.HasRole("admin") || user.HasRole("auditor") {
		t.Error("user role: want not admin/auditor")
	}
	aud := Identity{Roles: []string{"auditor"}}
	if !aud.HasRole("auditor") {
		t.Error("auditor role: want auditor")
	}
}

func TestTokenHashAndHint(t *testing.T) {
	raw := "t-trae-3f8a2b7c"
	h1, h2 := tokenHash(raw), tokenHash(raw)
	if h1 != h2 {
		t.Error("hash not deterministic")
	}
	if len(h1) != 64 {
		t.Errorf("sha256 hex: want 64 chars got %d", len(h1))
	}
	if tokenHash("t-trae-3f8a2b7c") == tokenHash("t-trae-3f8a2b7d") {
		t.Error("hash collision on different tokens")
	}
	// hint 是前 8 位，不泄露完整 token
	if got := tokenHint(raw); got != "t-trae-3" {
		t.Errorf("hint: want 't-trae-3' got %q", got)
	}
	// 短 token：hint 原样返回
	if got := tokenHint("short"); got != "short" {
		t.Errorf("short hint: want 'short' got %q", got)
	}
}

func TestNormalizeScope(t *testing.T) {
	if got := normalizeScope(""); got != nil {
		t.Errorf("empty: want nil got %v", got)
	}
	got := normalizeScope(" memories:write, , goals:read,memories:write ")
	if len(got) != 2 {
		t.Fatalf("want 2 unique, got %v", got)
	}
	if got[0] != "memories:write" || got[1] != "goals:read" {
		t.Errorf("want [memories:write goals:read] got %v", got)
	}
}

func TestEnsureAuthTablesDDLRules(t *testing.T) {
	// DDL 常量必须只含 CREATE（红线：禁 ALTER/DROP/TRUNCATE）
	upper := strings.ToUpper(authDDL)
	for _, banned := range []string{"ALTER TABLE", "ALTER INDEX", "DROP TABLE", "DROP INDEX", "TRUNCATE"} {
		if strings.Contains(upper, banned) {
			t.Errorf("auth DDL 违反红线：含 %s", banned)
		}
	}
	for _, required := range []string{"USERS", "AGENT_TOKENS", "AUTH_TOKENS", "AUTH_AUDIT"} {
		if !strings.Contains(upper, "CREATE TABLE IF NOT EXISTS "+required) {
			t.Errorf("auth DDL 缺少必需表 %s", required)
		}
	}
}

func TestEnsureAuthTablesSQLite(t *testing.T) {
	b := newAuthSQLite(t)
	// 幂等：重复调用不报错
	if err := EnsureAuthTables(context.Background(), b.db); err != nil {
		t.Fatalf("EnsureAuthTables idempotent: %v", err)
	}
	// 四张表都可查
	for _, tbl := range []string{"users", "agent_tokens", "auth_tokens", "auth_audit"} {
		var n int
		if err := b.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s: want 1 got %d", tbl, n)
		}
	}
}

func TestDBAuthenticatorAgent(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-trae-abc12345"
	seedAgentToken(t, b, raw, "trae", "dshteam")

	auth := NewDBAuthenticator(b.db, "sqlite")
	id, err := auth.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if id.Kind != IdentityKindAgent || id.AgentID != "trae" || id.TenantID != "dshteam" {
		t.Errorf("identity mismatch: %+v", id)
	}
	if !id.ScopeAllows("memories:write") || id.ScopeAllows("goals:write") {
		t.Errorf("scope mismatch: %v", id.Scope)
	}
	if id.Issuer != "internal" {
		t.Errorf("issuer: want internal got %q", id.Issuer)
	}

	// 错误 token → 401
	if _, err := auth.Authenticate(context.Background(), "t-wrong"); err == nil {
		t.Error("wrong token: want error")
	}
	// 空 token → 401
	if _, err := auth.Authenticate(context.Background(), ""); err == nil {
		t.Error("empty token: want error")
	}
}

func TestDBAuthenticatorAgentRevoked(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-trae-rev-001"
	id := seedAgentToken(t, b, raw, "trae", "dshteam")
	// 吊销
	if _, err := b.db.Exec(`UPDATE agent_tokens SET revoked_at = ? WHERE id = ?`,
		formatTime(timeNow()), id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	auth := NewDBAuthenticator(b.db, "sqlite")
	if _, err := auth.Authenticate(context.Background(), raw); err == nil {
		t.Error("revoked token: want error")
	}
}

func TestDBAuthenticatorAgentExpired(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-trae-exp-001"
	id := seedAgentToken(t, b, raw, "trae", "dshteam")
	past := formatTime(timeNow().Add(-time.Hour))
	if _, err := b.db.Exec(`UPDATE agent_tokens SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	auth := NewDBAuthenticator(b.db, "sqlite")
	if _, err := auth.Authenticate(context.Background(), raw); err == nil {
		t.Error("expired token: want error")
	}
}

func TestDBAuthenticatorUser(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-user-skywalk-001"
	now := formatTime(timeNow())
	// 直接插 auth_tokens（管理员 session）
	if _, err := b.db.Exec(`INSERT INTO auth_tokens
		(id, token_hash, token_hint, user_id, tenant_id, roles, scope, created_at)
		VALUES (?, ?, ?, 'skywalk', 'dshteam', 'admin', 'admin:manage', ?)`,
		"au-001", tokenHash(raw), tokenHint(raw), now); err != nil {
		t.Fatalf("seed auth token: %v", err)
	}
	auth := NewDBAuthenticator(b.db, "sqlite")
	id, err := auth.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("valid user token: %v", err)
	}
	if id.Kind != IdentityKindUser || id.UserID != "skywalk" {
		t.Errorf("identity mismatch: %+v", id)
	}
	if !id.HasRole("admin") || !id.HasRole("auditor") {
		t.Errorf("roles mismatch: %v", id.Roles)
	}
}

func TestAuthenticatorRequestBearer(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-trae-bearer-001"
	seedAgentToken(t, b, raw, "trae", "dshteam")
	auth := NewDBAuthenticator(b.db, "sqlite")

	// 正确 Bearer
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	id, err := auth.AuthenticateRequest(context.Background(), req)
	if err != nil || id.AgentID != "trae" {
		t.Errorf("bearer ok: id=%+v err=%v", id, err)
	}

	// 缺 Authorization → 401
	empty := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err := auth.AuthenticateRequest(context.Background(), empty); err == nil {
		t.Error("missing header: want error")
	}
}

// mockAuthenticator 用于测试注入覆盖。
type mockAuthenticator struct {
	wantIdentity Identity
}

func (m *mockAuthenticator) Name() string { return "mock" }
func (m *mockAuthenticator) Authenticate(_ context.Context, raw string) (Identity, error) {
	if raw == "ok" {
		return m.wantIdentity, nil
	}
	return Identity{}, ErrUnauthenticated
}
func (m *mockAuthenticator) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	if r == nil {
		return Identity{}, ErrUnauthenticated
	}
	return m.Authenticate(ctx, bearerToken(r))
}

// TestSetAuthenticatorOverride：注入外部认证器后 AuthMiddleware 优先走它（设计文档 §4）。
func TestSetAuthenticatorOverride(t *testing.T) {
	mock := &mockAuthenticator{wantIdentity: Identity{
		Kind: IdentityKindUser, UserID: "alice", TenantID: "dshteam",
		Roles: []string{"admin"}, Issuer: "mock-idp",
	}}
	SetAuthenticator(mock)
	defer SetAuthenticator(nil)

	if got := activeAuthenticator(); got == nil || got.Name() != "mock" {
		t.Fatalf("activeAuthenticator: want mock, got %v", got)
	}

	var captured Identity
	var capturedOK bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, capturedOK = IdentityFromContext(r.Context())
	})

	// mock 认的 token → 放行，身份来自 mock
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ok")
	rec := httptest.NewRecorder()
	AuthMiddleware(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("override allow: want 200 got %d", rec.Code)
	}
	if !capturedOK || captured.UserID != "alice" || captured.TenantID != "dshteam" {
		t.Errorf("override identity: got %+v ok=%v", captured, capturedOK)
	}
	if !captured.HasRole("admin") {
		t.Errorf("override roles: want admin, got %v", captured.Roles)
	}
	if captured.Issuer != "mock-idp" {
		t.Errorf("override issuer: want mock-idp, got %q", captured.Issuer)
	}

	// mock 不认的 token → 401
	req2 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer bad")
	rec2 := httptest.NewRecorder()
	AuthMiddleware(h).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("override deny: want 401 got %d", rec2.Code)
	}

	// 清除注入 → 走现有路径（透传）
	SetAuthenticator(nil)
	if got := activeAuthenticator(); got != nil {
		t.Fatalf("after clear: want nil, got %v", got)
	}
	req3 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec3 := httptest.NewRecorder()
	AuthMiddleware(h).ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("passthrough after clear: want 200 got %d", rec3.Code)
	}
}

// TestAuthMiddlewareOperationalExempt：运维端点豁免（注入认证器后仍生效）。
func TestAuthMiddlewareOperationalExempt(t *testing.T) {
	mock := &mockAuthenticator{wantIdentity: Identity{UserID: "alice"}}
	SetAuthenticator(mock)
	defer SetAuthenticator(nil)

	for _, path := range []string{"/healthz", "/health", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // 不带 token
		rec := httptest.NewRecorder()
		AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: 运维端点豁免，want 200 got %d", path, rec.Code)
		}
	}
}
