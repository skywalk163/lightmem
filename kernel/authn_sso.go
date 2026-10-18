// R22.4 SSO 适配器（docs/token-management-design.md §4）
//
// 目的：实现 `Authenticator` 接口的 SSO（OIDC/SAML）版本。设计原则：
// **后台不验签，只认结果**——验签（OIDC JWT / SAML 断言）由前台服务完成，
// 通过 `SSOVerifier` 回调注入，后台把已验签的身份映射进业务层。
//
// 接线默认关（红线④）：需 profile `auth.authenticator: sso` 显式开启，
// 且调用方注入 `SSOVerifier`。未配置 = 走内部实现，零回归。
//
// 零第三方依赖（红线⑤）：仅 stdlib。JWT/SAML 库由前台（或外部 IdP SDK）负责，
// 本包不引第三方依赖。
package kernel

import (
	"context"
	"net/http"
	"strings"
)

// SSOVerifier 校验已验签的 token/断言并返回身份。
// 验签由注入方（前台服务）完成；返回的 Identity 的 Issuer 会被强制标为 "sso:<name>"。
type SSOVerifier func(ctx context.Context, raw string) (Identity, error)

// SSOConfig 描述 SSO 提供方（只做登记与标识，后台不存敏感凭据）。
type SSOConfig struct {
	Name     string `yaml:"name"`     // 如 "oidc-github"、"saml-okta"
	Provider string `yaml:"provider"` // "oidc" | "saml"，仅用于日志与审计
}

// ssoIdentityHeader 是自定义头名，供前台把已验签的断言透传给后台。
// 设计文档 §4："从 Authorization 或自定义头取已验签的 token/断言"。
const ssoIdentityHeader = "X-Lightmem-Identity"

// ssoAuthenticator 实现 Authenticator 的 SSO 版本。
type ssoAuthenticator struct {
	name     string
	provider string
	verifier SSOVerifier
}

// NewSSOAuthenticator 构造 SSO 适配器。name 如 "oidc-github"；verifier 必填。
func NewSSOAuthenticator(name string, verifier SSOVerifier) Authenticator {
	return &ssoAuthenticator{name: name, verifier: verifier}
}

// NewSSOAuthenticatorWithProvider 带 provider 标识构造（用于审计区分 oidc/saml）。
func NewSSOAuthenticatorWithProvider(name, provider string, verifier SSOVerifier) Authenticator {
	return &ssoAuthenticator{name: name, provider: provider, verifier: verifier}
}

func (s *ssoAuthenticator) Name() string { return "sso:" + s.name }

// Authenticate 用已验签的原始断言换身份。
func (s *ssoAuthenticator) Authenticate(ctx context.Context, raw string) (Identity, error) {
	if s.verifier == nil {
		return Identity{}, ErrUnauthenticated
	}
	if strings.TrimSpace(raw) == "" {
		return Identity{}, ErrUnauthenticated
	}
	id, err := s.verifier(ctx, raw)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	// 强制标记签发源，便于审计（不信任 verifier 提供的 Issuer）
	id.Issuer = "sso:" + s.name
	if id.Kind != IdentityKindUser {
		id.Kind = IdentityKindUser
	}
	return id, nil
}

// AuthenticateRequest 从请求提取已验签断言。
// 优先 `Authorization: Bearer <assertion>`；回退到 `X-Lightmem-Identity` 头。
func (s *ssoAuthenticator) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	if r == nil {
		return Identity{}, ErrUnauthenticated
	}
	if b := strings.TrimSpace(bearerToken(r)); b != "" {
		return s.Authenticate(ctx, b)
	}
	if h := strings.TrimSpace(r.Header.Get(ssoIdentityHeader)); h != "" {
		return s.Authenticate(ctx, h)
	}
	return Identity{}, ErrUnauthenticated
}
