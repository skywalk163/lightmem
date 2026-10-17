// R21-B 鉴权与写方身份强制（§3.1 冻结 API）
//
// 目标：给共享记忆大脑装上可信边界。每个 Agent 一份 token，服务端据此定身份，
// 杜绝冒充（G4：旧实现用 body 里的 user_id 当 actor）。
//
// 设计口径（任务书 §3.1 / B 线）：
//   - Identity 由 token 解析出的可信身份，服务端唯一真源；
//   - 默认透传：未配置 LIMS_AGENT_TOKENS 时 AuthMiddleware 直接放行（红线④，零回归）；
//   - 已配置时：缺/错 token → 401 {"error":"unauthorized"}；
//   - LIMS_REQUIRE_AUTH=1 时 RequireIdentity 强制 401；
//   - 常量时间比较（crypto/subtle）防时序侧信道；
//   - 零第三方依赖（红线⑦）：仅 net/http / context / crypto / encoding/json / os。
package kernel

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// 环境变量名（任务书 §3.1 冻结）。
const (
	// EnvAgentTokens：token 表，JSON 字符串或指向 JSON 文件的路径（优先于 profile）。
	EnvAgentTokens = "LIMS_AGENT_TOKENS"
	// EnvRequireAuth："1" 时强制要求已认证身份（RequireIdentity 由透传翻为强制）。
	EnvRequireAuth = "LIMS_REQUIRE_AUTH"
)

// profileRequireAuth 记录 profile auth.require_auth（与 env LIMS_REQUIRE_AUTH 等价，
// 由 ConfigureAuth 置位；RequireIdentity 构造时读取）。
var profileRequireAuth atomic.Bool

// Identity 由 token 解析出的可信身份；服务端唯一真源。
// TokenID 是匹配到的 token 键（脱敏展示用，不应出现在业务字段里）。
type Identity struct {
	AgentID  string // 哪个 Agent 在操作
	UserID   string // 以哪个用户身份（决定 private 可见性）
	TenantID string // 租户边界
	TokenID  string // 命中的 token 标识（脱敏展示）
}

// AgentIdentity token 映射目标（profile auth.agent_tokens 值形态）
type AgentIdentity struct {
	AgentID  string `json:"agent_id" yaml:"agent_id"`
	UserID   string `json:"user_id" yaml:"user_id"`
	TenantID string `json:"tenant_id" yaml:"tenant_id"`
}

// AuthConfig profile 新增 auth 段（B 线仅新增，不动既有键）
type AuthConfig struct {
	RequireAuth bool                     `yaml:"require_auth"`
	AgentTokens map[string]AgentIdentity `yaml:"agent_tokens"`
}

// ------------------------------------------------------------------
// Token 表（进程级，读写加锁）
// ------------------------------------------------------------------

type tokenRegistry struct {
	mu     sync.RWMutex
	tokens map[string]AgentIdentity
}

var registry = &tokenRegistry{}

func (r *tokenRegistry) load(tokens map[string]AgentIdentity) {
	r.mu.Lock()
	r.tokens = tokens
	r.mu.Unlock()
}

func (r *tokenRegistry) lookup(token string) (AgentIdentity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.tokens[token]
	return id, ok
}

func (r *tokenRegistry) empty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tokens) == 0
}

// InitAgentTokens 初始化 token 表（幂等；并发安全）。
// 调用方（cmd/lightmem-mcp 或 server 装配）在启动阶段调用一次即可；
// 重复调用以最后一次为准（支持热更新）。
func InitAgentTokens(tokens map[string]AgentIdentity) {
	registry.load(tokens)
}

// LoadAgentTokens 从环境变量 LIMS_AGENT_TOKENS 或 profile auth 段装配 token 表。
// 环境变量优先；值为 JSON 字符串或指向 JSON 文件路径时均支持。
// 返回空表表示未配置（AuthMiddleware 走透传路径）。
func LoadAgentTokens(env func(string) string, auth *AuthConfig) map[string]AgentIdentity {
	out := map[string]AgentIdentity{}
	raw := ""
	if env != nil {
		raw = strings.TrimSpace(env(EnvAgentTokens))
	}
	if raw != "" {
		parsed := map[string]AgentIdentity{}
		// 先按 JSON 对象解析；失败则按文件路径读取
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			data, rerr := os.ReadFile(raw)
			if rerr != nil {
				log.Printf("[auth] %s 既非 JSON 也非可读文件: %v (fallback: profile auth)", EnvAgentTokens, err)
				goto profile
			}
			if jerr := json.Unmarshal(data, &parsed); jerr != nil {
				log.Printf("[auth] %s 文件解析失败: %v (fallback: profile auth)", EnvAgentTokens, jerr)
				goto profile
			}
		}
		for k, v := range parsed {
			if v.AgentID == "" || v.UserID == "" || v.TenantID == "" {
				log.Printf("[auth] 跳过不完整 token 条目 %s (agent_id/user_id/tenant_id 必填)", k)
				continue
			}
			out[k] = v
		}
		if len(out) > 0 {
			log.Printf("[auth] 从 LIMS_AGENT_TOKENS 装配 token 表: %d 个", len(out))
			return out
		}
	}
profile:
	if auth != nil && len(auth.AgentTokens) > 0 {
		for k, v := range auth.AgentTokens {
			if v.AgentID == "" || v.UserID == "" || v.TenantID == "" {
				log.Printf("[auth] 跳过不完整 profile token 条目 %s", k)
				continue
			}
			out[k] = v
		}
		log.Printf("[auth] 从 profile auth 段装配 token 表: %d 个", len(out))
	}
	return out
}

// ------------------------------------------------------------------
// Context 注入
// ------------------------------------------------------------------

type ctxKeyIdentity int

const ctxKeyIdentityValue ctxKeyIdentity = iota

// IdentityFromContext 从请求 context 取身份；未认证返回 ok=false。
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	v := ctx.Value(ctxKeyIdentityValue)
	if v == nil {
		return Identity{}, false
	}
	id, ok := v.(Identity)
	return id, ok
}

func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKeyIdentityValue, id)
}

// ------------------------------------------------------------------
// HTTP 中间件
// ------------------------------------------------------------------

// bearerToken 从请求头取 Bearer token（不区分大小写）。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	scheme, rest, found := strings.Cut(h, " ")
	if !found {
		return ""
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AuthMiddleware 解析 Authorization: Bearer <token> 并注入 Identity。
//   - 未配置 token 表时：透传（向后兼容，红线④），并打一行 warn 日志；
//   - 已配置时：缺/错 token → 401 {"error":"unauthorized"}。
//
// 中间件语义与 LoggingMiddleware / RecoverMiddleware / TraceIDMiddleware 互不干扰；
// 未通过鉴权的请求不会进入业务 handler。
// T3 部署（R21）：运维端点豁免。
//
// 理由：这些端点不读写任何记忆数据，却必须在「未认证」状态下可访问，否则
//  1. rc.d 的 supervisor 用 `fetch http://127.0.0.1:<port>/healthz` 做启动健康检查，
//     鉴权一旦覆盖 /healthz 就会 401 → **服务永远无法启动**（T3 实测重现）；
//  2. 进程外监控/告警探针通常不带 agent token。
//
// 注意：只豁免**无数据语义**的运维端点。业务端点（/search、/tools/*、/audit …）
// 一律仍需鉴权；/healthz 只回后端连通性与 profile 名，不含记忆正文。
func isOperationalEndpoint(path string) bool {
	switch path {
	case "/healthz", "/health", "/metrics":
		return true
	}
	return false
}

// AuthMiddleware 校验 agent token 并注入 Identity。
//
//	未配置 token → 透传（红线④，零回归）；已配置 → 缺/错 token 401。
//	运维端点（/healthz、/health、/metrics）豁免（见 isOperationalEndpoint）。
func AuthMiddleware(next http.Handler) http.Handler {
	if registry.empty() {
		log.Printf("[auth] LIMS_AGENT_TOKENS / profile auth 未配置，AuthMiddleware 透传（R21-B 默认关，红线④）")
		// 即便未启用 token 鉴权，仍接受**回环来源**的身份头，使 lightmem-mcp(:18444)
		// 转发过来的身份能沿链路传到 REST handler（否则 C 线 handler 拿不到身份）。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, ok := identityFromLoopbackHeaders(r); ok {
				next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	registry.mu.RLock()
	n := len(registry.tokens)
	registry.mu.RUnlock()
	log.Printf("[auth] agent token 鉴权已启用（tokens=%d）", n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 运维端点豁免：不读记忆正文，供 supervisor / 监控探针使用。
		if isOperationalEndpoint(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// 优先：回环来源的身份头（lightmem-mcp → lightmem-kernel 内部跳转）。
		// 该路径下 MCP 侧已按 Bearer token 认证完成，转发只用身份头承载，不带原始 token。
		if id, ok := identityFromLoopbackHeaders(r); ok {
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		var matched string
		var identity AgentIdentity
		registry.mu.RLock()
		for k, v := range registry.tokens {
			if constantTimeEqual(k, token) {
				matched = k
				identity = v
				break
			}
		}
		registry.mu.RUnlock()
		if matched == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		id := Identity{
			AgentID:  identity.AgentID,
			UserID:   identity.UserID,
			TenantID: identity.TenantID,
			TokenID:  redactTokenID(matched),
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// identityFromLoopbackHeaders 从 X-Lightmem-{User,Tenant,Agent} 头取身份，
// **仅当请求来自回环地址**时成立。
//
// 用途：lightmem-mcp(:18444) 已按 Bearer token 完成认证并解析出 Identity，
// 它再以 loopback HTTP 调用 lightmem-kernel(:18443) 的 REST 端点（A/C 解耦约定），
// 此时用这三个头承载身份，避免 REST 侧二次解析 token。
//
// 安全前提（**关键**）：只信任 127.0.0.1 / ::1。若放任任意来源，则任何远端
// 调用方只需伪造一个头即可冒充任意 Agent（等同 G4）。非回环一律忽略。
func identityFromLoopbackHeaders(r *http.Request) (Identity, bool) {
	if r == nil || !isLoopbackRequest(r) {
		return Identity{}, false
	}
	user := r.Header.Get("X-Lightmem-User")
	tenant := r.Header.Get("X-Lightmem-Tenant")
	agent := r.Header.Get("X-Lightmem-Agent")
	if user == "" && tenant == "" && agent == "" {
		return Identity{}, false
	}
	return Identity{UserID: user, TenantID: tenant, AgentID: agent}, true
}

// isLoopbackRequest 判断请求来源是否为回环地址（127.0.0.0/8 或 ::1）。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// RequireIdentity 强制要求已认证身份。
//   - LIMS_REQUIRE_AUTH=1 或 profile auth.require_auth → 未认证一律 401；
//   - 否则透传（默认关，红线④；任务书 §3.1「LIMS_REQUIRE_AUTH=1 时由部署层挂载」）。
func RequireIdentity(next http.Handler) http.Handler {
	if !requireAuthEnabled() {
		return next
	}
	log.Printf("[auth] RequireIdentity 已启用（%s=1 或 profile auth.require_auth）：未认证请求一律 401", EnvRequireAuth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 运维端点豁免（与 AuthMiddleware 同口径）：否则 supervisor 健康检查会 401，
		// 服务无法启动（T3 实测）。
		if isOperationalEndpoint(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := IdentityFromContext(r.Context()); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuthEnabled 判断是否强制已认证身份（env 优先，profile 段回落）。
func requireAuthEnabled() bool {
	if strings.TrimSpace(os.Getenv(EnvRequireAuth)) == "1" {
		return true
	}
	return profileRequireAuth.Load()
}

// MustIdentity 取身份，未认证时 panic（仅在已确认挂载 AuthMiddleware 的路径使用）。
func MustIdentity(ctx context.Context) Identity {
	id, ok := IdentityFromContext(ctx)
	if !ok {
		panic("kernel: authenticated identity required")
	}
	return id
}

// ------------------------------------------------------------------
// 写方身份强制辅助（B 线提供，供 C 线 handler 消费）
// ------------------------------------------------------------------

// ResolveActor 决定写入 actor：
//   - 已认证：以 token 身份为准（忽略 body 里的 user_id/tenant_id，防冒充）；
//   - 未认证：回退 body 值（默认透传路径），并返回 fallback=true 供审计记录。
func ResolveActor(ctx context.Context, bodyUser, bodyTenant string) (Identity, bool) {
	if id, ok := IdentityFromContext(ctx); ok {
		return id, false
	}
	return Identity{UserID: bodyUser, TenantID: bodyTenant}, true
}

// EnforcePrivateOwner 校验 private 可见性的 owner 是否为 token 身份。
// 返回 nil 表示放行；否则返回可写进 403 的 error。
func EnforcePrivateOwner(ctx context.Context, owner, visibility string) error {
	if !strings.EqualFold(visibility, string(VisPrivate)) {
		return nil
	}
	id, ok := IdentityFromContext(ctx)
	if !ok {
		// 未开启鉴权：透传路径下不拦截（保持 R20 行为）
		return nil
	}
	if owner != "" && owner != id.UserID {
		return fmt.Errorf("private memory owner %q must match token identity %q", owner, id.UserID)
	}
	return nil
}

func redactTokenID(token string) string {
	if len(token) <= 8 {
		return token[:1] + "***"
	}
	return token[:4] + "***" + token[len(token)-2:]
}

// ------------------------------------------------------------------
// 启动装配：profile auth 段 + 一键 ConfigureAuth（R21-B）
// ------------------------------------------------------------------

// LoadProfileAuth 从 profile YAML 中独立解析 auth: 段（R21-B）。
//
// 与 Profile 结构解耦：B 线文件域不含 kernel/profile.go，故由本文件独立解析
// YAML 的 auth 子树；yaml.v3 默认忽略未知键，Profile 新增该段不影响
// LoadProfile 既有校验（零回归）。无 auth 段 → (nil, nil)。
func LoadProfileAuth(path string) (*AuthConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load profile auth: %w", err)
	}
	var doc struct {
		Auth *AuthConfig `yaml:"auth"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse profile auth %s: %w", path, err)
	}
	return doc.Auth, nil
}

// AuthOptions 是 ConfigureAuth 的入参。
type AuthOptions struct {
	ProfilePath string // lims-*.profile.yaml 路径（读 auth: 段；可空）
}

// ConfigureAuth 由 main.go 启动时调用，一键装配鉴权（任务书 §3.1 优先级）：
//
//		env LIMS_AGENT_TOKENS（JSON 或文件路径）> profile auth.agent_tokens
//
//	  - env 已配置但解析失败 → 返回 error（调用方 fail-fast，绝不带病静默透传）；
//	  - 两者皆无 → 未配置态（AuthMiddleware 透传，红线④ 零回归）；
//	  - profile auth.require_auth 置位 RequireIdentity 强制开关（与 env 等价）。
func ConfigureAuth(opts AuthOptions) error {
	var auth *AuthConfig
	if opts.ProfilePath != "" {
		sec, err := LoadProfileAuth(opts.ProfilePath)
		if err != nil {
			return err
		}
		auth = sec
	}
	if auth != nil && auth.RequireAuth {
		profileRequireAuth.Store(true)
		log.Printf("[auth] profile auth.require_auth=true → RequireIdentity 强制已认证")
	}
	// env 严格校验：配置损坏必须在启动时炸，而不是运行时静默降级为透传。
	if raw := strings.TrimSpace(os.Getenv(EnvAgentTokens)); raw != "" {
		parsed, err := parseAgentTokensSource(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAgentTokens, err)
		}
		InitAgentTokens(parsed)
		log.Printf("[auth] token 表已装配（source=env, tokens=%d）", len(parsed))
		return nil
	}
	InitAgentTokens(LoadAgentTokens(os.Getenv, auth))
	if !registry.empty() {
		registry.mu.RLock()
		n := len(registry.tokens)
		registry.mu.RUnlock()
		log.Printf("[auth] token 表已装配（source=profile, tokens=%d）", n)
	}
	return nil
}

// parseAgentTokensSource 解析 env 值：JSON 对象字符串，或指向 JSON 文件的路径。
// 既不是 JSON 也不是可读文件 → error（fail-closed，不静默透传）。
func parseAgentTokensSource(raw string) (map[string]AgentIdentity, error) {
	data := []byte(raw)
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		b, err := os.ReadFile(raw)
		if err != nil {
			return nil, fmt.Errorf("value is neither a JSON object nor a readable file path: %q", raw)
		}
		data = b
	}
	parsed := map[string]AgentIdentity{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse agent tokens json: %w", err)
	}
	out := make(map[string]AgentIdentity, len(parsed))
	for k, v := range parsed {
		if v.AgentID == "" || v.UserID == "" || v.TenantID == "" {
			return nil, fmt.Errorf("token %.4s…: agent_id/user_id/tenant_id required", k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("token table is empty")
	}
	return out, nil
}
