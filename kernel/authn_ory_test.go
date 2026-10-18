// R22.4 Ory Kratos 适配器测试（docs §4 / kernel/authn_ory.go）
//
// 用 httptest 模拟 Kratos `GET /sessions/whoami`，验证：
// cookie / bearer 都能触发 whoami；成功映射 Identity；401 → 401；
// 默认 Mapper 的 traits 缺省；自定义 Mapper 可注入 tenant/roles 策略。
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// kratosCapture 捕获模拟 server 收到的请求（用指针容器，避免闭包变量返回值拷贝问题）。
type kratosCapture struct {
	req *http.Request
}

// newKratosWhoamiServer 起一个模拟 Kratos whoami 的 httptest server。
// 返回 (server, capture)。capture.req 记录收到的请求便于断言 cookie / bearer。
func newKratosWhoamiServer(t *testing.T, status int, sess KratosSession) (*httptest.Server, *kratosCapture) {
	t.Helper()
	cap := &kratosCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.req = r.Clone(r.Context())
		if r.URL.Path != "/sessions/whoami" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(sess)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestOryKratosWhoamiViaCookie(t *testing.T) {
	srv, cap := newKratosWhoamiServer(t, http.StatusOK, KratosSession{
		Active: true, ID: "sess-1",
		Identity: KratosIdentity{ID: "user-42", Traits: map[string]any{
			"tenant_id": "dshteam", "roles": "admin,user",
		}},
		ExpiresAt: "2030-01-01T00:00:00Z",
	})
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: srv.URL})
	defer SetAuthenticator(nil)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.AddCookie(&http.Cookie{Name: OryKratosCookie, Value: "sess-token"})
	req.Header.Set("Authorization", "Bearer sess-token")

	id, err := auth.AuthenticateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("AuthenticateRequest: %v", err)
	}
	if id.Kind != IdentityKindUser || id.UserID != "user-42" || id.TenantID != "dshteam" {
		t.Fatalf("identity: %+v", id)
	}
	if !id.HasRole("admin") || !id.HasRole("user") {
		t.Fatalf("roles: %v", id.Roles)
	}
	if id.Issuer != "ory_kratos" || id.TokenID != "sess-1" {
		t.Fatalf("issuer/token: %+v", id)
	}
	// 断言 cookie 与 bearer 都传给了 whoami
	if cap.req == nil {
		t.Fatal("no request captured")
	}
	if c, err := cap.req.Cookie(OryKratosCookie); err != nil || c.Value != "sess-token" {
		t.Fatalf("cookie not forwarded: %+v %v", c, err)
	}
	if got := cap.req.Header.Get("Authorization"); got != "Bearer sess-token" {
		t.Fatalf("authorization: got %q", got)
	}
}

func TestOryKratosWhoamiViaRaw(t *testing.T) {
	srv, _ := newKratosWhoamiServer(t, http.StatusOK, KratosSession{
		Active: true, ID: "sess-2",
		Identity: KratosIdentity{ID: "user-7"},
	})
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: srv.URL})
	defer SetAuthenticator(nil)

	id, err := auth.Authenticate(context.Background(), "sess-raw")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// 缺 traits → 默认 tenant=default、roles=[user]
	if id.TenantID != "default" {
		t.Fatalf("tenant: got %q want default", id.TenantID)
	}
	if len(id.Roles) != 1 || id.Roles[0] != "user" {
		t.Fatalf("roles: %v", id.Roles)
	}
	if id.UserID != "user-7" {
		t.Fatalf("user: %v", id.UserID)
	}
}

func TestOryKratosUnauthenticated(t *testing.T) {
	srv, _ := newKratosWhoamiServer(t, http.StatusUnauthorized, KratosSession{})
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: srv.URL})
	defer SetAuthenticator(nil)

	if _, err := auth.Authenticate(context.Background(), "expired-sess"); err != ErrUnauthenticated {
		t.Fatalf("want ErrUnauthenticated got %v", err)
	}
}

func TestOryKratosInactiveSession(t *testing.T) {
	// active=false → 视为未认证
	srv, _ := newKratosWhoamiServer(t, http.StatusOK, KratosSession{
		Active: false, ID: "sess-dead", Identity: KratosIdentity{ID: "user-x"},
	})
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: srv.URL})
	defer SetAuthenticator(nil)

	if _, err := auth.Authenticate(context.Background(), "sess"); err != ErrUnauthenticated {
		t.Fatalf("want ErrUnauthenticated got %v", err)
	}
}

func TestOryKratosEmptyRaw(t *testing.T) {
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: "http://127.0.0.1:1"})
	defer SetAuthenticator(nil)
	if _, err := auth.Authenticate(context.Background(), ""); err != ErrUnauthenticated {
		t.Fatalf("empty raw: want ErrUnauthenticated got %v", err)
	}
	if _, err := auth.AuthenticateRequest(context.Background(), nil); err != ErrUnauthenticated {
		t.Fatalf("nil req: want ErrUnauthenticated got %v", err)
	}
}

func TestOryKratosNoBaseURL(t *testing.T) {
	auth := NewOryKratosAuthenticator(OryKratosConfig{})
	defer SetAuthenticator(nil)
	if _, err := auth.Authenticate(context.Background(), "sess"); err == nil {
		t.Fatal("empty base_url must error")
	}
}

func TestOryKratosCustomMapper(t *testing.T) {
	srv, _ := newKratosWhoamiServer(t, http.StatusOK, KratosSession{
		Active: true, ID: "sess-9",
		Identity: KratosIdentity{ID: "ext-1"},
	})
	auth := NewOryKratosAuthenticator(OryKratosConfig{
		BaseURL: srv.URL,
		Mapper: func(s KratosSession) Identity {
			return Identity{
				Kind: IdentityKindUser, ID: s.Identity.ID, UserID: s.Identity.ID,
				TenantID: "custom-tenant", Roles: []string{"user"},
				Issuer: "ory_kratos", TokenID: s.ID,
			}
		},
	})
	defer SetAuthenticator(nil)

	id, err := auth.Authenticate(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.TenantID != "custom-tenant" {
		t.Fatalf("custom mapper tenant: got %q", id.TenantID)
	}
}

func TestOryKratosName(t *testing.T) {
	auth := NewOryKratosAuthenticator(OryKratosConfig{BaseURL: "http://x"})
	defer SetAuthenticator(nil)
	if auth.Name() != "ory_kratos" {
		t.Fatalf("name: %v", auth.Name())
	}
}
