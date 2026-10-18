// R22.4 SSO 适配器测试（docs §4 / kernel/authn_sso.go）
//
// 验证：验签回调成功 → 身份 + Issuer 强制 sso:<name>；回调拒绝 → 401；
// 从 Bearer / 自定义头取断言。后台不验签只认结果。
package kernel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSSOAuthenticateSuccess(t *testing.T) {
	auth := NewSSOAuthenticator("oidc-github", func(ctx context.Context, raw string) (Identity, error) {
		if raw != "assertion-1" {
			t.Fatalf("raw: got %q", raw)
		}
		return Identity{Kind: IdentityKindUser, ID: "gh-1", UserID: "gh-1", TenantID: "dshteam"}, nil
	})
	defer SetAuthenticator(nil)

	id, err := auth.Authenticate(context.Background(), "assertion-1")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.UserID != "gh-1" || id.TenantID != "dshteam" {
		t.Fatalf("identity: %+v", id)
	}
	// 强制签发源：不信任 verifier 提供的 Issuer
	if id.Issuer != "sso:oidc-github" {
		t.Fatalf("issuer: got %q want sso:oidc-github", id.Issuer)
	}
	if id.Kind != IdentityKindUser {
		t.Fatalf("kind: %v", id.Kind)
	}
}

func TestSSOVerifierRejected(t *testing.T) {
	auth := NewSSOAuthenticator("saml-okta", func(ctx context.Context, raw string) (Identity, error) {
		return Identity{}, errors.New("invalid signature")
	})
	defer SetAuthenticator(nil)

	if _, err := auth.Authenticate(context.Background(), "bad-assertion"); err != ErrUnauthenticated {
		t.Fatalf("want ErrUnauthenticated got %v", err)
	}
}

func TestSSOEmptyAndNilVerifier(t *testing.T) {
	auth := NewSSOAuthenticator("oidc", func(ctx context.Context, raw string) (Identity, error) {
		return Identity{Kind: IdentityKindUser, UserID: "u"}, nil
	})
	defer SetAuthenticator(nil)
	if _, err := auth.Authenticate(context.Background(), ""); err != ErrUnauthenticated {
		t.Fatalf("empty: want 401 got %v", err)
	}
	noVerifier := NewSSOAuthenticator("oidc", nil)
	if _, err := noVerifier.Authenticate(context.Background(), "x"); err != ErrUnauthenticated {
		t.Fatalf("nil verifier: want 401 got %v", err)
	}
}

func TestSSORequestBearerAndHeader(t *testing.T) {
	auth := NewSSOAuthenticator("oidc-gitlab", func(ctx context.Context, raw string) (Identity, error) {
		return Identity{Kind: IdentityKindUser, ID: raw, UserID: raw, TenantID: "t"}, nil
	})
	defer SetAuthenticator(nil)

	// Bearer 优先
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer assert-b")
	id, err := auth.AuthenticateRequest(context.Background(), req)
	if err != nil || id.UserID != "assert-b" {
		t.Fatalf("bearer: id=%+v err=%v", id, err)
	}
	// 无 Bearer → 回退自定义头
	req2 := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req2.Header.Set(ssoIdentityHeader, "assert-h")
	id2, err := auth.AuthenticateRequest(context.Background(), req2)
	if err != nil || id2.UserID != "assert-h" {
		t.Fatalf("header: id=%+v err=%v", id2, err)
	}
	// 都缺 → 401
	req3 := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if _, err := auth.AuthenticateRequest(context.Background(), req3); err != ErrUnauthenticated {
		t.Fatalf("missing: want 401 got %v", err)
	}
}

func TestSSOName(t *testing.T) {
	a := NewSSOAuthenticatorWithProvider("oidc-github", "oidc", func(ctx context.Context, raw string) (Identity, error) {
		return Identity{Kind: IdentityKindUser, UserID: "u"}, nil
	})
	defer SetAuthenticator(nil)
	if a.Name() != "sso:oidc-github" {
		t.Fatalf("name: %v", a.Name())
	}
}
