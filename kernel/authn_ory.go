// R22.4 Ory Kratos 适配器（docs/token-management-design.md §4）
//
// 目的：实现 `Authenticator` 接口的 Ory Kratos 版本——读 session cookie /
// bearer → 调 Kratos `GET /sessions/whoami` → 把返回的 identity 映射成
// `Identity{Kind:User}`。业务层（记忆读写、检索）完全不变。
//
// 接线默认关（红线④）：适配器随代码发布，但**不自动接入路由**。
// 需在 profile `auth.authenticator: ory_kratos` 显式开启，main.go 的
// ConfigureAuth 才会 SetAuthenticator(它)。未配置 = 走内部实现，零回归。
//
// 零第三方依赖（红线⑤）：仅 stdlib（net/http 调 whoami，encoding/json 解析）。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OryKratosCookie 是 Kratos session cookie 名（前端常见默认值）。
const OryKratosCookie = "ory_kratos_session"

// OryKratosConfig 配置 Kratos 适配器。
type OryKratosConfig struct {
	// BaseURL 是 Kratos public API 地址，如 "http://kratos:4433"。末尾斜杠可带可不带。
	BaseURL string `yaml:"base_url"`
	// Timeout 单次 whoami 请求超时，默认 5s。
	Timeout time.Duration `yaml:"timeout"`
	// HTTPClient 可注入（测试用 httptest server 或 mock）。nil 时用默认 client。
	HTTPClient *http.Client `yaml:"-"`
	// Mapper 可注入身份映射策略（不同部署 tenant/roles 来源不同）。
	// nil 时用默认映射：从 identity.traits 读 tenant_id / roles。
	Mapper func(KratosSession) Identity `yaml:"-"`
}

// KratosIdentity 是 whoami 响应里 identity 的解析结构（只取需要的字段）。
type KratosIdentity struct {
	ID       string                 `json:"id"`
	SchemaID string                 `json:"schema_id"`
	Traits   map[string]any         `json:"traits"`
}

// KratosSession 是 `GET /sessions/whoami` 的响应。
type KratosSession struct {
	Active   bool            `json:"active"`
	ID       string          `json:"id"`
	Identity KratosIdentity  `json:"identity"`
	ExpiresAt string         `json:"expires_at"`
}

// oryKratosAuthenticator 实现 Authenticator 的 Kratos 版本。
type oryKratosAuthenticator struct {
	cfg    OryKratosConfig
	client *http.Client
	mapper func(KratosSession) Identity
}

// NewOryKratosAuthenticator 构造 Kratos 适配器。
func NewOryKratosAuthenticator(cfg OryKratosConfig) Authenticator {
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	mapper := cfg.Mapper
	if mapper == nil {
		mapper = defaultKratosMapper
	}
	return &oryKratosAuthenticator{
		cfg:    cfg,
		client: client,
		mapper: mapper,
	}
}

func (o *oryKratosAuthenticator) Name() string { return "ory_kratos" }

// Authenticate 用原始凭证（session token 明文）调 Kratos whoami。
func (o *oryKratosAuthenticator) Authenticate(ctx context.Context, raw string) (Identity, error) {
	if strings.TrimSpace(raw) == "" {
		return Identity{}, ErrUnauthenticated
	}
	return o.whoami(ctx, raw, "")
}

// AuthenticateRequest 从请求提取 Kratos session cookie（或 Bearer）调 whoami。
// 兼容两种来源：`ory_kratos_session` cookie，或 `Authorization: Bearer <session>`。
func (o *oryKratosAuthenticator) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	if r == nil {
		return Identity{}, ErrUnauthenticated
	}
	// 先 cookie，再 bearer
	if c, err := r.Cookie(OryKratosCookie); err == nil && c.Value != "" {
		return o.whoami(ctx, c.Value, "")
	}
	if b := strings.TrimSpace(bearerToken(r)); b != "" {
		return o.whoami(ctx, b, "")
	}
	return Identity{}, ErrUnauthenticated
}

// whoami 调 Kratos public `GET /sessions/whoami`。
// sessionToken 以 cookie 值或 bearer 传入（Kratos public whoami 两者都认）。
func (o *oryKratosAuthenticator) whoami(ctx context.Context, sessionToken, _ string) (Identity, error) {
	base := strings.TrimRight(o.cfg.BaseURL, "/")
	if base == "" {
		return Identity{}, fmt.Errorf("ory_kratos: base_url not configured")
	}
	url := base + "/sessions/whoami"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("ory_kratos: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// 同时带 cookie 与 bearer，兼容 Kratos 两种识别方式
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.AddCookie(&http.Cookie{Name: OryKratosCookie, Value: sessionToken})

	resp, err := o.client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("ory_kratos: whoami: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fallthrough to parse
	case http.StatusUnauthorized, http.StatusForbidden:
		return Identity{}, ErrUnauthenticated
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Identity{}, fmt.Errorf("ory_kratos: whoami status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var sess KratosSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return Identity{}, fmt.Errorf("ory_kratos: decode session: %w", err)
	}
	if !sess.Active || sess.Identity.ID == "" {
		return Identity{}, ErrUnauthenticated
	}
	return o.mapper(sess), nil
}

// defaultKratosMapper 把 Kratos session 映射成 Identity。
// 默认策略：UserID/ID 取 identity.id；tenant_id、roles 从 traits 读（缺省 "default"/"user"）。
// 不同部署可通过 OryKratosConfig.Mapper 注入自定义策略。
func defaultKratosMapper(sess KratosSession) Identity {
	id := sess.Identity.ID
	traits := sess.Identity.Traits
	return Identity{
		Kind:      IdentityKindUser,
		ID:        id,
		UserID:    id,
		TenantID:  traitString(traits, "tenant_id", "default"),
		Roles:     splitRoles(traitString(traits, "roles", "user")),
		Issuer:    "ory_kratos",
		TokenID:   sess.ID,
		ExpiresAt: parseTime(sess.ExpiresAt),
	}
}

// traitString 从 traits map 取字符串字段；缺失返回 def。
func traitString(traits map[string]any, key, def string) string {
	if traits == nil {
		return def
	}
	v, ok := traits[key]
	if !ok {
		return def
	}
	switch s := v.(type) {
	case string:
		if s == "" {
			return def
		}
		return s
	default:
		return def
	}
}
