package frontend

// R23.3 SSO 端到端 handler 测试：起完整前台服务（内存库 + mock 后台）+ 本地 IdP stub，
// 用真实 HTTP 走 /login/sso → 回调 → 建会话 → 进工作台。零外网、零外部 IdP。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ssoTestServer 起前台服务并装配指向 IdP stub 的 OIDC 客户端。
func ssoTestServer(t *testing.T, stub *idpStub) (*httptest.Server, *Server) {
	t.Helper()
	m := newMockBackend(t)
	ts_, srv_ := newTestServer(t, m)
	oc, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatalf("oidc client: %v", err)
	}
	srv_.oidc = oc
	return ts_, srv_
}

// readBody 读取并关闭响应体。
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestSSOLoginEndToEnd(t *testing.T) {
	stub := newIDPStub(t)
	ts, _ := ssoTestServer(t, stub)

	c := newClient(t)

	// 1) 发起：/login/sso → 302 到 IdP 授权端点，带 state/nonce。
	resp, err := c.Get(ts.URL + "/login/sso")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: want 302, got %d", resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	state, nonce := u.Query().Get("state"), u.Query().Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("missing state/nonce in %s", loc)
	}
	stub.setNonce(nonce)

	// 2) 回调：state + code → 302 /workspace 且下发会话 cookie。
	cb := ts.URL + "/login/sso/callback?state=" + url.QueryEscape(state) + "&code=good-code"
	resp, err = c.Get(cb)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	gotLoc := resp.Header.Get("Location")
	hasSess := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "lightmem_session" && ck.Value != "" {
			hasSess = true
		}
	}
	resp.Body.Close()
	// redirect 状态：handler 用 303（SeeOther）落到 /workspace。
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: want 302/303, got %d", resp.StatusCode)
	}
	if gotLoc != "/workspace" {
		t.Errorf("callback location = %q, want /workspace", gotLoc)
	}
	if !hasSess {
		t.Error("callback did not set lightmem_session cookie")
	}

	// 3) 用会话 cookie 访问工作台：应 200 且看到 SSO 用户。
	resp, err = c.Get(ts.URL + "/workspace")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workspace: want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "sso-") {
		t.Errorf("workspace body missing sso user identity: %s", snippet(body))
	}
}

func TestSSODisabledByDefault(t *testing.T) {
	// 未配置 OIDC（oidc == nil）：发起与回调都应 503，登录页不显示 SSO 入口。
	m := newMockBackend(t)
	ts, _ := newTestServer(t, m)
	c := newClient(t)

	for _, path := range []string{"/login/sso", "/login/sso/callback?state=x&code=y"} {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: want 503, got %d", path, resp.StatusCode)
		}
	}

	resp, err := c.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if strings.Contains(body, "SSO") {
		t.Error("login page must not show SSO entry when disabled")
	}
}

func TestSSORejectsUnknownState(t *testing.T) {
	stub := newIDPStub(t)
	ts, _ := ssoTestServer(t, stub)
	c := newClient(t)

	resp, err := c.Get(ts.URL + "/login/sso/callback?state=forged-state&code=x")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 (login page), got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "state 无效") {
		t.Errorf("want state error on login page, got: %s", snippet(body))
	}
}

func TestSSORejectsReusedState(t *testing.T) {
	stub := newIDPStub(t)
	ts, _ := ssoTestServer(t, stub)
	c := newClient(t)

	resp, _ := c.Get(ts.URL + "/login/sso")
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	u, _ := url.Parse(loc)
	state, nonce := u.Query().Get("state"), u.Query().Get("nonce")
	stub.setNonce(nonce)

	cb := ts.URL + "/login/sso/callback?state=" + url.QueryEscape(state) + "&code=good-code"
	r1, err := c.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusFound && r1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first callback want 302/303, got %d", r1.StatusCode)
	}
	// state 一次性：重用应失败。
	r2, err := c.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, r2)
	if r2.StatusCode != http.StatusOK || !strings.Contains(body, "state 无效") {
		t.Errorf("reused state must fail: status=%d body=%s", r2.StatusCode, snippet(body))
	}
}

// snippet 截断 body 便于日志输出。
func snippet(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
