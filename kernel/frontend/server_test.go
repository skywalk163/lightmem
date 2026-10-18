package frontend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mockBackend 模拟后台：签 session + 业务端点，记录收到的 Authorization。
type mockBackend struct {
	*httptest.Server
	lastAuth string // 最后一次业务端点请求的 Bearer token
}

func newMockBackend(t *testing.T) *mockBackend {
	t.Helper()
	m := &mockBackend{}
	issue := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"id": "tok-abc", "token": "backend-session-token-xyz"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/tokens/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		issue(w, r)
	})
	mux.HandleFunc("POST /admin/tokens/session/{id}/revoke", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	business := func(w http.ResponseWriter, r *http.Request) {
		m.lastAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{"results": []map[string]string{{"kind": "fact", "fact": "hello"}}})
	}
	mux.HandleFunc("POST /tools/memory/recall", business)
	mux.HandleFunc("POST /search", business)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /tools/memory/save", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"memory_id": "m-1"})
	})
	mux.HandleFunc("GET /tools/memory/plan", business)
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

// newTestServer 起完整前台服务（内存库 + mock 后台）。
func newTestServer(t *testing.T, m *mockBackend) (*httptest.Server, *Server) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	bc := NewBackendClient(BackendConfig{
		BaseURL: m.URL, AdminURL: m.URL, ServiceToken: "svc-token",
	})
	tpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	srv, err := NewServer(Config{DefaultTenant: "dshteam", DefaultRoles: "user"}, store, bc, tpl)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // 不跟随重定向，便于断言 302
	}}
}

func getCSRF(t *testing.T, c *http.Client, rawURL string) string {
	// 先 GET 触发服务端 set-cookie（若尚无），再从 jar 读（服务端已有则不重发 set-cookie）。
	if resp, err := c.Get(rawURL); err == nil {
		resp.Body.Close()
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "lightmem_csrf" {
			return ck.Value
		}
	}
	t.Fatalf("no csrf cookie from %s", rawURL)
	return ""
}

func postForm(t *testing.T, c *http.Client, url string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(url, form)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestRegisterLoginLogoutFlow(t *testing.T) {
	m := newMockBackend(t)
	base, _ := newTestServer(t, m)
	c := newClient(t)

	// 未登录访问工作台 → 302 到 /login
	resp, err := c.Get(base.URL + "/workspace")
	if err != nil {
		t.Fatalf("GET /workspace: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /workspace = %d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// 注册
	csrf := getCSRF(t, c, base.URL+"/register")
	r := postForm(t, c, base.URL+"/register", url.Values{
		"_csrf": {csrf}, "username": {"alice"}, "password": {"s3cret-pass"},
		"password2": {"s3cret-pass"}, "display_name": {"Alice"},
	})
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/workspace" {
		t.Fatalf("register = %d loc=%q", r.StatusCode, r.Header.Get("Location"))
	}

	// 已登录访问工作台 → 200
	resp2, err := c.Get(base.URL + "/workspace")
	if err != nil {
		t.Fatalf("GET /workspace (authed): %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authed /workspace = %d", resp2.StatusCode)
	}
	if !strings.Contains(string(body), "记忆工作台") {
		t.Fatalf("workspace body missing title: %s", truncate(string(body), 120))
	}

	// 调 /api/memory/recall → mock 收到用户 token
	resp3 := postForm(t, c, base.URL+"/api/memory/recall", url.Values{})
	b3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("/api/memory/recall = %d %s", resp3.StatusCode, string(b3))
	}
	var rr struct {
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal(b3, &rr); err != nil || len(rr.Results) == 0 {
		t.Fatalf("recall response = %s", string(b3))
	}
	if m.lastAuth != "Bearer backend-session-token-xyz" {
		t.Fatalf("backend saw auth = %q, want user session token", m.lastAuth)
	}

	// 登出 → 302 / ，再访问 /workspace → 302 /login
	csrf2 := getCSRF(t, c, base.URL+"/workspace")
	lo := postForm(t, c, base.URL+"/logout", url.Values{"_csrf": {csrf2}})
	io.Copy(io.Discard, lo.Body)
	lo.Body.Close()
	if lo.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d", lo.StatusCode)
	}
	resp4, _ := c.Get(base.URL + "/workspace")
	io.Copy(io.Discard, resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusSeeOther {
		t.Fatalf("post-logout /workspace = %d, want 302", resp4.StatusCode)
	}
}

func TestLoginBadPassword(t *testing.T) {
	m := newMockBackend(t)
	base, srv := newTestServer(t, m)
	old := passwordHashIter
	passwordHashIter = 1000
	defer func() { passwordHashIter = old }()

	// 直接插一个凭据
	err := srv.store.CreateCredential(t.Context(), Credential{
		Username: "alice", PasswordHash: mustHash(t, "good-pass"), TenantID: "dshteam",
	})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	c := newClient(t)
	csrf := getCSRF(t, c, base.URL+"/login")
	r := postForm(t, c, base.URL+"/login", url.Values{
		"_csrf": {csrf}, "username": {"alice"}, "password": {"wrong-pass"},
	})
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("bad login = %d", r.StatusCode)
	}
	if !strings.Contains(string(body), "用户名或密码错误") {
		t.Fatalf("bad login body = %s", truncate(string(body), 160))
	}
}

func TestLoginUnknownUserSameError(t *testing.T) {
	m := newMockBackend(t)
	base, _ := newTestServer(t, m)
	c := newClient(t)
	csrf := getCSRF(t, c, base.URL+"/login")
	r := postForm(t, c, base.URL+"/login", url.Values{
		"_csrf": {csrf}, "username": {"ghost"}, "password": {"whatever"},
	})
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "用户名或密码错误") {
		t.Fatalf("unknown-user body = %s", truncate(string(body), 160))
	}
}

func TestCSRFRejectsMissingToken(t *testing.T) {
	m := newMockBackend(t)
	base, _ := newTestServer(t, m)
	c := newClient(t)
	r := postForm(t, c, base.URL+"/register", url.Values{
		"username": {"alice"}, "password": {"s3cret-pass"}, "password2": {"s3cret-pass"},
	})
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("register without csrf = %d, want 403", r.StatusCode)
	}
}

func TestHealthEndpoint(t *testing.T) {
	m := newMockBackend(t)
	base, _ := newTestServer(t, m)
	resp, err := http.Get(base.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d", resp.StatusCode)
	}
}

func TestStaticServed(t *testing.T) {
	m := newMockBackend(t)
	base, _ := newTestServer(t, m)
	resp, err := http.Get(base.URL + "/static/style.css")
	if err != nil {
		t.Fatalf("GET /static/style.css: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "--brand") {
		t.Fatalf("style.css = %d len=%d", resp.StatusCode, len(body))
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := hashPassword(pw, 1000)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	return h
}