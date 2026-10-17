// R22 认证器抽象（docs/token-management-design.md §4）
//
// 目标：把"凭据 → 身份"这一步抽成接口，让内部实现可被 Ory Kratos / SSO 无缝替换，
// 业务层（记忆读写、检索）只认 Identity，零改动。
//
// 设计口径（R22 定稿）：
//   - Authenticator 是唯一鉴权入口；默认实现走内部 token 表（含 R21 静态表 fallback）；
//   - 接线默认关：未显式注入外部 Authenticator 时行为与 R21 完全一致（红线④零回归）；
//   - 零第三方依赖（红线⑤）：仅 context / crypto / net/http / strings / time；
//   - scope 采用**细粒度**（如 memories:write），词汇表见设计文档 §11。
package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IdentityKind 区分主体是 Agent（AI）还是 User（人）。
// 零值 IdentityKindUser 之外的任何值在语义上都按 Agent 处理，兼容 R21。
type IdentityKind int

const (
	IdentityKindAgent IdentityKind = iota // Agent（AI），R21 唯一形态
	IdentityKindUser                      // User（人），R22 起支持
)

// String 用于日志与审计。
func (k IdentityKind) String() string {
	switch k {
	case IdentityKindAgent:
		return "agent"
	case IdentityKindUser:
		return "user"
	default:
		return "agent"
	}
}

// Authenticator 是唯一鉴权入口。外部实现（Ory Kratos / SSO / 企业 IdP）
// 只需实现它即可无缝替换内部实现，业务层零改动。
type Authenticator interface {
	// Authenticate 用原始凭证换身份。token 或 session 由实现自行解释。
	Authenticate(ctx context.Context, raw string) (Identity, error)

	// AuthenticateRequest 从 HTTP 请求提取凭证（Bearer / Cookie / Header）。
	// 内部实现读 Authorization: Bearer；Ory 实现可读 cookie + 调 Kratos session API。
	AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error)

	// Name 返回认证器标识，用于日志与审计。
	Name() string
}

// ErrUnauthenticated 表示凭证缺失或无效（AuthMiddleware 据此返回 401）。
var ErrUnauthenticated = &authError{msg: "unauthorized"}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// 内置 scope 常量（细粒度词汇表，见设计文档 §11 决策 #4）。
const (
	ScopeMemoriesRead  = "memories:read"
	ScopeMemoriesWrite = "memories:write"
	ScopeGoalsRead     = "goals:read"
	ScopeGoalsWrite    = "goals:write"
	ScopeProcsRead     = "procedures:read"
	ScopeProcsWrite    = "procedures:write"
	ScopeEventsRead    = "events:read"
	ScopeEventsWrite   = "events:write"
	ScopeAdminManage   = "admin:manage"
)

// ScopeAllows 判断身份是否允许某动作。
// 空 scope = 全部权限（保留测试版"一条 token 干所有事"的便利，见设计文档 §11）。
// 支持两种通配：`*`（全允许）与 `域:*`（如 `memories:*` 允许该域全部动作）。
func (id Identity) ScopeAllows(need string) bool {
	if len(id.Scope) == 0 {
		return true
	}
	// 提取 need 的域（`memories:write` → `memories`）
	domain := ""
	if i := strings.IndexByte(need, ':'); i > 0 {
		domain = need[:i]
	}
	for _, s := range id.Scope {
		if s == need || s == "*" {
			return true
		}
		// 前缀通配：`memories:*` 允许 `memories:read` / `memories:write`
		if domain != "" && s == domain+":*" {
			return true
		}
	}
	return false
}

// HasRole 判断身份是否具有指定角色（RBAC）。
// 空 roles = 普通主体；admin 隐式拥有 auditor 的只读权。
func (id Identity) HasRole(role string) bool {
	for _, r := range id.Roles {
		switch role {
		case "auditor":
			if r == "auditor" || r == "admin" {
				return true
			}
		default:
			if r == role {
				return true
			}
		}
	}
	return false
}

// tokenHash 计算 token 的 SHA-256 十六进制哈希。
// 数据库只存哈希（不存明文），明文只在签发那一刻返回给调用方（设计文档 §9）。
func tokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// tokenHint 返回明文前 8 位，让人在 UI / 清单 / 日志里认出是哪条 token。
// 不泄露可用的完整 token。
func tokenHint(raw string) string {
	const n = 8
	if len(raw) <= n {
		return raw
	}
	return raw[:n]
}

// redactIdentityScope 归一化 scope 字符串：逗号分隔 → 去空白 → 去空项 → 去重。
func normalizeScope(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// timeNow 可被测试替换。
var timeNow = time.Now

// ---------------------------------------------------------------------------
// R22 接入点：外部 Authenticator 注入（缺省未注入 = 零回归，走 R21 静态表路径）
// ---------------------------------------------------------------------------

var (
	activeAuthMu sync.RWMutex
	activeAuth   Authenticator // nil = 未注入
)

// SetAuthenticator 注入外部认证器（Ory Kratos / SSO / 前台服务实现）。
// 在 main.go 启动时调用；调用后 AuthMiddleware 优先走它。
// 传 nil 可清除注入（回到 R21 静态表路径）。
func SetAuthenticator(a Authenticator) {
	activeAuthMu.Lock()
	activeAuth = a
	activeAuthMu.Unlock()
	log.Printf("[authn] SetAuthenticator: %s", authName(a))
}

// activeAuthenticator 返回当前注入的认证器；nil 表示未注入。
func activeAuthenticator() Authenticator {
	activeAuthMu.RLock()
	defer activeAuthMu.RUnlock()
	return activeAuth
}

func authName(a Authenticator) string {
	if a == nil {
		return "<none>"
	}
	return a.Name()
}

// authMiddlewareWith 用指定认证器构建 AuthMiddleware。
// 与 R21 语义一致：运维端点豁免、回环身份头优先、缺/错凭证 401。
func authMiddlewareWith(a Authenticator, next http.Handler) http.Handler {
	log.Printf("[authn] authenticator=%s 已启用（覆盖静态 token 表路径）", a.Name())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 运维端点豁免：不读记忆正文，供 supervisor / 监控探针使用。
		if isOperationalEndpoint(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// 回环来源的身份头（lightmem-mcp → lightmem-kernel 内部跳转）仍优先。
		if id, ok := identityFromLoopbackHeaders(r); ok {
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		id, err := a.AuthenticateRequest(r.Context(), r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}
