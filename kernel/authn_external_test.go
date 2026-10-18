// R22.4 外部认证器接线测试（kernel/authn_external.go）
//
// 核心红线校验：接线默认关——auth 为 nil 或 authenticator 为空时**不注入任何认证器**，
// 行为逐位不变（零回归）。显式配置损坏则启动即炸（fail-closed，不静默降级）。
package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetupExternalNilAuthNoop(t *testing.T) {
	// nil auth → 不注入，activeAuthenticator 保持 nil
	if err := setupExternalAuthenticator(nil); err != nil {
		t.Fatalf("nil auth: %v", err)
	}
	if got := activeAuthenticator(); got != nil {
		t.Fatalf("nil auth should not inject, got %v", got)
	}
}

func TestSetupExternalEmptyAuthenticatorNoop(t *testing.T) {
	// authenticator 为空 → 零回归
	if err := setupExternalAuthenticator(&AuthConfig{}); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if got := activeAuthenticator(); got != nil {
		t.Fatalf("empty authenticator should not inject, got %v", got)
	}
}

func TestSetupExternalOryNoBaseURL(t *testing.T) {
	// fail-closed：配置不完整必须报错
	err := setupExternalAuthenticator(&AuthConfig{
		Authenticator: "ory_kratos",
		OryKratos:     &OryKratosConfig{}, // 缺 base_url
	})
	if err == nil {
		t.Fatal("ory without base_url must error")
	}
}

func TestSetupExternalOryOK(t *testing.T) {
	SetAuthenticator(nil)
	defer SetAuthenticator(nil)
	err := setupExternalAuthenticator(&AuthConfig{
		Authenticator: "ory_kratos",
		OryKratos:     &OryKratosConfig{BaseURL: "http://kratos:4433"},
	})
	if err != nil {
		t.Fatalf("ory ok: %v", err)
	}
	got := activeAuthenticator()
	if got == nil || got.Name() != "ory_kratos" {
		t.Fatalf("active: got %v", got)
	}
}

func TestSetupExternalSSONoVerifier(t *testing.T) {
	SetSSOVerifier(nil) // 确保未注入
	defer SetSSOVerifier(nil)
	err := setupExternalAuthenticator(&AuthConfig{
		Authenticator: "sso",
		SSO:           &SSOConfig{Name: "oidc-github", Provider: "oidc"},
	})
	if err == nil {
		t.Fatal("sso without verifier must error (fail-closed)")
	}
}

func TestSetupExternalSSOOK(t *testing.T) {
	SetAuthenticator(nil)
	defer SetAuthenticator(nil)
	SetSSOVerifier(func(ctx context.Context, raw string) (Identity, error) {
		return Identity{Kind: IdentityKindUser, ID: raw, UserID: raw, TenantID: "t"}, nil
	})
	defer SetSSOVerifier(nil)

	err := setupExternalAuthenticator(&AuthConfig{
		Authenticator: "sso",
		SSO:           &SSOConfig{Name: "oidc-github", Provider: "oidc"},
	})
	if err != nil {
		t.Fatalf("sso ok: %v", err)
	}
	got := activeAuthenticator()
	if got == nil || got.Name() != "sso:oidc-github" {
		t.Fatalf("active: got %v", got)
	}
}

func TestSetupExternalUnknown(t *testing.T) {
	err := setupExternalAuthenticator(&AuthConfig{Authenticator: "saml-magic"})
	if err == nil {
		t.Fatal("unknown authenticator must error")
	}
}

// TestExternalAuthMiddlewareInjectsIdentity 端到端：接线后 AuthMiddleware 走外部认证器。
func TestExternalAuthMiddlewareInjectsIdentity(t *testing.T) {
	SetAuthenticator(nil)
	defer SetAuthenticator(nil)
	SetSSOVerifier(func(ctx context.Context, raw string) (Identity, error) {
		return Identity{Kind: IdentityKindUser, ID: "sso-u", UserID: "sso-u", TenantID: "sso-t", Roles: []string{"user"}}, nil
	})
	defer SetSSOVerifier(nil)
	if err := setupExternalAuthenticator(&AuthConfig{Authenticator: "sso", SSO: &SSOConfig{Name: "oidc-github"}}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var captured Identity
	var capturedOK bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, capturedOK = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer sso-assert")
	rec := httptest.NewRecorder()
	AuthMiddleware(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("allow: want 200 got %d", rec.Code)
	}
	if !capturedOK || captured.UserID != "sso-u" || captured.Issuer != "sso:oidc-github" {
		t.Fatalf("identity: got %+v ok=%v", captured, capturedOK)
	}
}
