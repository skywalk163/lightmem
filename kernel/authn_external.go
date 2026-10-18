// R22.4 外部认证器接线（docs/token-management-design.md §4）
//
// 由 ConfigureAuth 调用。规则：
//   - auth 为 nil 或 auth.Authenticator 为空 → 不做任何事（零回归，红线④）；
//   - auth.Authenticator == "ory_kratos" → 构造 OryKratos 适配器并 SetAuthenticator；
//   - auth.Authenticator == "sso"       → 需调用方（前台）已通过
//     SetSSOVerifier 注入验签回调，否则启动即炸（fail-closed，不静默降级）。
//
// SSO 的 verifier 由进程内注入（前台服务与 kernel 同进程部署时），
// 因此提供 SetSSOVerifier 全局注册点。
package kernel

import (
	"fmt"
	"log"
	"strings"
)

// ssoVerifierStore 保存 SSO 验签回调（进程内单例）。
// 前台服务在 ConfigureAuth 之前注入；未注入时 sso 接线会 fail-closed。
var ssoVerifierStore SSOVerifier

// SetSSOVerifier 注入 SSO 验签回调（R22.4，仅 sso 模式需要）。
// 验签由前台/IdP SDK 完成，后台只认结果。
func SetSSOVerifier(v SSOVerifier) { ssoVerifierStore = v }

// setupExternalAuthenticator 根据 auth 配置接线外部认证器（默认关）。
func setupExternalAuthenticator(auth *AuthConfig) error {
	if auth == nil || strings.TrimSpace(auth.Authenticator) == "" {
		return nil // 默认：内部实现，零回归
	}
	switch strings.TrimSpace(auth.Authenticator) {
	case "ory_kratos":
		if auth.OryKratos == nil || strings.TrimSpace(auth.OryKratos.BaseURL) == "" {
			return fmt.Errorf("auth.authenticator=ory_kratos 但 auth.ory_kratos.base_url 未配置")
		}
		SetAuthenticator(NewOryKratosAuthenticator(*auth.OryKratos))
		log.Printf("[auth] 外部认证器已接线：ory_kratos → %s", auth.OryKratos.BaseURL)
		return nil
	case "sso":
		if ssoVerifierStore == nil {
			return fmt.Errorf("auth.authenticator=sso 但未调用 kernel.SetSSOVerifier 注入验签回调")
		}
		name := "sso"
		if auth.SSO != nil && strings.TrimSpace(auth.SSO.Name) != "" {
			name = auth.SSO.Name
		}
		provider := ""
		if auth.SSO != nil {
			provider = auth.SSO.Provider
		}
		SetAuthenticator(NewSSOAuthenticatorWithProvider(name, provider, ssoVerifierStore))
		log.Printf("[auth] 外部认证器已接线：sso:%s (provider=%s)", name, provider)
		return nil
	default:
		return fmt.Errorf("auth.authenticator=%q 未知（支持: ory_kratos / sso）", auth.Authenticator)
	}
}
