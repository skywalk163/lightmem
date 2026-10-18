// R22.2 管理 API 测试（docs/token-management-design.md §6）
//
// 覆盖：未注入 DB 时 /admin 不注册（零回归）、回环限制、admin role 鉴权、
// 用户 CRUD、agent token 签发·吊销·轮换（不重启即生效）、session token 代签、
// 审计查询。全部用内存 SQLite。
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newAdminFixture 装配内存 SQLite + auth 表 + APIServer（注入 auth DB）。
// 返回 APIServer 与底层 sqlite backend（可访问 b.db）。
func newAdminFixture(t testing.TB) *APIServer {
	b := newAuthSQLite(t)
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	srv.SetAuthDB(b.db)
	return srv
}

// seedAdminTokenDirect 直接在 AdminStore 的 DB 上插入 admin token，返回明文。
func seedAdminTokenDirect(t testing.TB, a *AdminStore) string {
	t.Helper()
	raw := "t-admin-direct-x"
	now := formatTime(timeNow())
	if _, err := a.db.Exec(`INSERT INTO users
		(id, username, email, tenant_id, role, idp, created_at, updated_at)
		VALUES ('u-admin', 'admin', '', 'dshteam', 'admin', 'internal', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := a.db.Exec(`INSERT INTO auth_tokens
		(id, token_hash, token_hint, user_id, tenant_id, roles, scope, created_at)
		VALUES ('au-admin', ?, ?, 'admin', 'dshteam', 'admin', '', ?)`,
		tokenHash(raw), tokenHint(raw), now); err != nil {
		t.Fatalf("seed auth token: %v", err)
	}
	return raw
}

// adminReq 构造一个已带 admin Bearer token 的请求。
func adminReq(t testing.TB, method, path, token string, body any) *http.Request {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, strings.NewReader(string(b)))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = "127.0.0.1:53000"
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestAdminNotRegisteredWithoutDB(t *testing.T) {
	// 未注入 DB：/admin 路由不注册 → 404（零回归）
	b := newTestSQLiteBackend(t)
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未注入 DB：/admin 应 404，got %d", rec.Code)
	}
}

func TestAdminGuardRequiresToken(t *testing.T) {
	srv := newAdminFixture(t)
	// 无 token → 401
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	r.RemoteAddr = "127.0.0.1:53000"
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: want 401 got %d", rec.Code)
	}
	// 错 token → 401
	rec2 := httptest.NewRecorder()
	r2 := adminReq(t, http.MethodGet, "/admin/users", "t-wrong", nil)
	srv.Handler().ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("bad token: want 401 got %d", rec2.Code)
	}
}

func TestAdminGuardLoopbackRestricted(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)
	// 非回环来源 → 403
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	r.RemoteAddr = "203.0.113.5:54321"
	r.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: want 403 got %d", rec.Code)
	}
}

func TestAdminUserCreateAndList(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)

	// 建用户
	rec := httptest.NewRecorder()
	r := adminReq(t, http.MethodPost, "/admin/users", token, map[string]any{
		"username": "alice", "email": "a@x.com", "tenant_id": "dshteam", "role": "user",
	})
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: want 201 got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created["username"] != "alice" {
		t.Errorf("created username: %v", created)
	}

	// 列用户
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, adminReq(t, http.MethodGet, "/admin/users", token, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("list users: want 200 got %d", rec2.Code)
	}
	var listed struct {
		Users []struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"users"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &listed)
	if listed.Count < 2 { // admin + alice
		t.Errorf("list count: want >=2 got %d", listed.Count)
	}

	// 改角色为 auditor
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, adminReq(t, http.MethodPatch,
		"/admin/users/"+created["id"], token, map[string]any{"role": "auditor"}))
	if rec3.Code != http.StatusOK {
		t.Fatalf("patch role: want 200 got %d body=%s", rec3.Code, rec3.Body.String())
	}
}

func TestAdminAgentTokenLifecycle(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)

	// 签发 agent token（带 scope 和过期）
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodPost, "/admin/tokens/agent", token, map[string]any{
		"agent_id": "trae", "tenant_id": "dshteam", "user_id": "skywalk",
		"scope": "memories:write,goals:read", "expires_in_seconds": 86400,
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue agent token: want 201 got %d body=%s", rec.Code, rec.Body.String())
	}
	var issued struct {
		ID      string `json:"id"`
		Token   string `json:"token"`
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)
	if issued.Token == "" || issued.ID == "" {
		t.Fatal("issued token/id empty")
	}

	// 新 token 立即生效（不重启即生效）
	auth := NewDBAuthenticator(srv.admin.db, "sqlite")
	if id, err := auth.Authenticate(context.Background(), issued.Token); err != nil || id.AgentID != "trae" {
		t.Fatalf("fresh token should work: id=%+v err=%v", id, err)
	}

	// 吊销
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, adminReq(t, http.MethodPost,
		"/admin/tokens/agent/"+issued.ID+"/revoke", token, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("revoke: want 200 got %d body=%s", rec2.Code, rec2.Body.String())
	}
	// 吊销后立即失效（不重启即生效）
	if _, err := auth.Authenticate(context.Background(), issued.Token); err == nil {
		t.Error("revoked token should fail")
	}
}

func TestAdminAgentTokenRotate(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)
	auth := NewDBAuthenticator(srv.admin.db, "sqlite")

	// 签发
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodPost, "/admin/tokens/agent", token, map[string]any{
		"agent_id": "dsh", "tenant_id": "dshteam", "user_id": "skywalk", "scope": "memories:read",
	}))
	var issued struct{ ID, Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)

	// 轮换
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, adminReq(t, http.MethodPost,
		"/admin/tokens/agent/"+issued.ID+"/rotate", token, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rotate: want 200 got %d body=%s", rec2.Code, rec2.Body.String())
	}
	var rotated struct {
		OldID string `json:"old_id"`
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &rotated)
	if rotated.Token == "" || rotated.ID == "" {
		t.Fatal("rotated token/id empty")
	}
	// 旧失效、新生效
	if _, err := auth.Authenticate(context.Background(), issued.Token); err == nil {
		t.Error("old token after rotate should fail")
	}
	if id, err := auth.Authenticate(context.Background(), rotated.Token); err != nil || id.AgentID != "dsh" {
		t.Errorf("new token after rotate should work: id=%+v err=%v", id, err)
	}
}

func TestAdminSessionTokenIssue(t *testing.T) {
	srv := newAdminFixture(t)
	adminTok := seedAdminTokenDirect(t, srv.admin)

	// 前台服务代签用户 session token
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodPost, "/admin/tokens/session", adminTok, map[string]any{
		"user_id": "bob", "tenant_id": "dshteam", "roles": "user", "scope": "memories:read",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue session: want 201 got %d body=%s", rec.Code, rec.Body.String())
	}
	var issued struct {
		Token  string   `json:"token"`
		ID     string   `json:"id"`
		Roles  []string `json:"roles"`
		UserID string   `json:"user_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)
	if issued.Token == "" || issued.UserID != "bob" {
		t.Fatalf("session issued mismatch: %+v", issued)
	}
	if len(issued.Roles) != 1 || issued.Roles[0] != "user" {
		t.Errorf("session roles: %v", issued.Roles)
	}

	// user 用 session token 调管理 API → 403（非 admin）
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, adminReq(t, http.MethodGet, "/admin/users", issued.Token, nil))
	if rec2.Code != http.StatusForbidden {
		t.Errorf("non-admin session on admin api: want 403 got %d", rec2.Code)
	}

	// 吊销 session
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, adminReq(t, http.MethodPost,
		"/admin/tokens/session/"+issued.ID+"/revoke", adminTok, nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("revoke session: want 200 got %d body=%s", rec3.Code, rec3.Body.String())
	}
}

func TestAdminAuditRecorded(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)

	// 触发一次管理调用（guard 里会写审计）
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodGet, "/admin/users", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d", rec.Code)
	}

	// 查审计
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, adminReq(t, http.MethodGet, "/admin/audit", token, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("audit list: want 200 got %d body=%s", rec2.Code, rec2.Body.String())
	}
	var audit struct {
		Records []struct {
			Action string `json:"action"`
			Actor  string `json:"actor_id"`
		} `json:"records"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &audit)
	if audit.Count < 1 {
		t.Errorf("audit: want >=1 record, got %d", audit.Count)
	}
}

func TestAdminListAgentTokens(t *testing.T) {
	srv := newAdminFixture(t)
	token := seedAdminTokenDirect(t, srv.admin)

	// 签两条
	for _, agent := range []string{"trae", "dsh"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodPost, "/admin/tokens/agent", token, map[string]any{
			"agent_id": agent, "tenant_id": "dshteam",
		}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("issue %s: %d", agent, rec.Code)
		}
	}
	// 列表（按 agent 过滤）
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminReq(t, http.MethodGet, "/admin/tokens/agent?agent=trae", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Tokens []struct {
			AgentID string `json:"agent_id"`
		} `json:"tokens"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Count != 1 || listed.Tokens[0].AgentID != "trae" {
		t.Errorf("filtered list: count=%d tokens=%+v", listed.Count, listed.Tokens)
	}
}
