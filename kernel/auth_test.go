// R21-B：鉴权单测（任务书 §五 B 线「≥12 个 Test」）
//
// 覆盖：token 装配（env JSON / env 文件 / profile 回落 / env 优先 / 不完整条目跳过）、
// profile auth 段解析、ConfigureAuth 一键装配与 fail-fast、有效/无效/缺失 token、
// 常量时间比较（前缀拒）、Bearer 大小写、默认透传（零回归）、RequireIdentity
// 开关语义、身份注入 context、**身份覆盖 body user_id（防冒充）**、
// private owner 不匹配 403 判定、token 脱敏、并发安全。
package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ------------------------------------------------------------------
// 测试隔离助手
// ------------------------------------------------------------------

// resetAuthState 清空包级鉴权态（registry + require 开关 + 两个 env），
// 测试结束后恢复（t.Setenv 自动恢复 env；registry/flag 手动恢复）。
func resetAuthState(t *testing.T) {
	t.Helper()
	t.Setenv(EnvAgentTokens, "")
	t.Setenv(EnvRequireAuth, "")

	registry.mu.RLock()
	saved := make(map[string]AgentIdentity, len(registry.tokens))
	for k, v := range registry.tokens {
		saved[k] = v
	}
	registry.mu.RUnlock()
	savedRequire := profileRequireAuth.Load()

	InitAgentTokens(nil)
	profileRequireAuth.Store(false)

	t.Cleanup(func() {
		InitAgentTokens(saved)
		profileRequireAuth.Store(savedRequire)
	})
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp %s: %v", name, err)
	}
	return path
}

const testTokenJSON = `{
  "t-dsh-8f3a9c2e": {"agent_id":"deepseek-harness","user_id":"dsh-agent","tenant_id":"dshteam"},
  "t-trae-000001":  {"agent_id":"trae","user_id":"skywalk","tenant_id":"dshteam"}
}`

// ------------------------------------------------------------------
// Token 装配
// ------------------------------------------------------------------

// env JSON 字符串 → token 表。
func TestLoadAgentTokens_EnvJSON(t *testing.T) {
	env := func(k string) string {
		if k == EnvAgentTokens {
			return testTokenJSON
		}
		return ""
	}
	tokens := LoadAgentTokens(env, nil)
	if len(tokens) != 2 {
		t.Fatalf("want 2 tokens, got %d", len(tokens))
	}
	id, ok := tokens["t-dsh-8f3a9c2e"]
	if !ok || id.AgentID != "deepseek-harness" || id.UserID != "dsh-agent" || id.TenantID != "dshteam" {
		t.Fatalf("t-dsh mapped wrong: %+v (ok=%v)", id, ok)
	}
}

// env 可以是文件路径（避免超长 env）。
func TestLoadAgentTokens_EnvFileFallback(t *testing.T) {
	path := writeTempFile(t, "tokens.json", testTokenJSON)
	env := func(k string) string {
		if k == EnvAgentTokens {
			return path
		}
		return ""
	}
	tokens := LoadAgentTokens(env, nil)
	if len(tokens) != 2 {
		t.Fatalf("file source: want 2 tokens, got %d", len(tokens))
	}
}

// env 缺省 → 回落 profile auth 段。
func TestLoadAgentTokens_ProfileFallback(t *testing.T) {
	env := func(string) string { return "" }
	auth := &AuthConfig{AgentTokens: map[string]AgentIdentity{
		"t-profile-01": {AgentID: "raccoon", UserID: "skywalk", TenantID: "dshteam"},
	}}
	tokens := LoadAgentTokens(env, auth)
	if len(tokens) != 1 {
		t.Fatalf("profile fallback: want 1 token, got %d", len(tokens))
	}
	if _, ok := tokens["t-profile-01"]; !ok {
		t.Fatalf("profile token missing: %+v", tokens)
	}
}

// env 优先于 profile（§3.1 优先级）。
func TestLoadAgentTokens_EnvPriorityOverProfile(t *testing.T) {
	env := func(k string) string {
		if k == EnvAgentTokens {
			return testTokenJSON
		}
		return ""
	}
	auth := &AuthConfig{AgentTokens: map[string]AgentIdentity{
		"t-profile-01": {AgentID: "raccoon", UserID: "skywalk", TenantID: "dshteam"},
	}}
	tokens := LoadAgentTokens(env, auth)
	if _, ok := tokens["t-dsh-8f3a9c2e"]; !ok {
		t.Error("env token missing")
	}
	if _, ok := tokens["t-profile-01"]; ok {
		t.Error("profile token must not be effective when env is set")
	}
}

// 不完整条目（缺 tenant_id）跳过，完整条目保留。
func TestLoadAgentTokens_IncompleteEntrySkipped(t *testing.T) {
	env := func(k string) string {
		if k == EnvAgentTokens {
			return `{"t-good-0001":{"agent_id":"trae","user_id":"skywalk","tenant_id":"dshteam"},
			        "t-bad-0002":{"agent_id":"trae","user_id":"skywalk"}}`
		}
		return ""
	}
	tokens := LoadAgentTokens(env, nil)
	if len(tokens) != 1 {
		t.Fatalf("want 1 valid token, got %d: %+v", len(tokens), tokens)
	}
	if _, ok := tokens["t-good-0001"]; !ok {
		t.Error("valid entry dropped")
	}
}

// profile auth: 段独立解析（与 Profile 结构解耦，B 线文件域不含 profile.go）。
func TestLoadProfileAuth_SectionParsed(t *testing.T) {
	path := writeTempFile(t, "lims-test.profile.yaml", `profile: family
storage_backend: pgvector
auth:
  require_auth: true
  agent_tokens:
    "t-dsh-8f3a": {agent_id: deepseek-harness, user_id: dsh-agent, tenant_id: dshteam}
`)
	sec, err := LoadProfileAuth(path)
	if err != nil {
		t.Fatalf("LoadProfileAuth: %v", err)
	}
	if sec == nil {
		t.Fatal("auth section: got nil, want parsed")
	}
	if !sec.RequireAuth {
		t.Error("require_auth: got false, want true")
	}
	b, ok := sec.AgentTokens["t-dsh-8f3a"]
	if !ok || b.AgentID != "deepseek-harness" || b.UserID != "dsh-agent" || b.TenantID != "dshteam" {
		t.Fatalf("agent_tokens entry wrong: %+v (ok=%v)", b, ok)
	}
}

// 无 auth 段 → (nil, nil)，不影响 LoadProfile 既有行为（零回归）。
func TestLoadProfileAuth_AbsentSection(t *testing.T) {
	path := writeTempFile(t, "lims-plain.profile.yaml", `profile: family
storage_backend: pgvector
`)
	sec, err := LoadProfileAuth(path)
	if err != nil {
		t.Fatalf("LoadProfileAuth: %v", err)
	}
	if sec != nil {
		t.Fatalf("want nil section, got %+v", sec)
	}
}

// ------------------------------------------------------------------
// ConfigureAuth（启动一键装配）
// ------------------------------------------------------------------

// env 装配：registry 生效，source 语义正确。
func TestConfigureAuth_EnvTokens(t *testing.T) {
	resetAuthState(t)
	t.Setenv(EnvAgentTokens, testTokenJSON)
	if err := ConfigureAuth(AuthOptions{}); err != nil {
		t.Fatalf("ConfigureAuth: %v", err)
	}
	if registry.empty() {
		t.Fatal("registry empty after ConfigureAuth(env)")
	}
	id, ok := registry.lookup("t-trae-000001")
	if !ok || id.UserID != "skywalk" {
		t.Fatalf("registry lookup: %+v (ok=%v)", id, ok)
	}
}

// env 损坏 → error（main.go fail-fast，绝不带病静默透传）。
func TestConfigureAuth_MalformedEnvErrors(t *testing.T) {
	resetAuthState(t)
	t.Setenv(EnvAgentTokens, "{broken")
	if err := ConfigureAuth(AuthOptions{}); err == nil {
		t.Fatal("want error for malformed env, got nil")
	}
	// 空 JSON 对象同样拒绝（配置了却一张表都没有 = 配置事故）
	t.Setenv(EnvAgentTokens, "{}")
	if err := ConfigureAuth(AuthOptions{}); err == nil {
		t.Fatal("want error for empty token table, got nil")
	}
}

// profile auth 段接线：token 表生效 + require_auth 置位 RequireIdentity。
func TestConfigureAuth_ProfileTokensAndRequireFlag(t *testing.T) {
	resetAuthState(t)
	profilePath := writeTempFile(t, "lims-family.profile.yaml", `profile: family
storage_backend: pgvector
auth:
  require_auth: true
  agent_tokens:
    "t-dsh-8f3a9c2e": {agent_id: deepseek-harness, user_id: dsh-agent, tenant_id: dshteam}
`)
	if err := ConfigureAuth(AuthOptions{ProfilePath: profilePath}); err != nil {
		t.Fatalf("ConfigureAuth: %v", err)
	}
	if _, ok := registry.lookup("t-dsh-8f3a9c2e"); !ok {
		t.Fatal("profile token not registered")
	}
	if !requireAuthEnabled() {
		t.Fatal("require flag not set from profile auth.require_auth")
	}

	// 全链：有效 token → 200 且注入身份；无 token → 401
	var got Identity
	h := WithMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = IdentityFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
		AuthMiddleware,
		RequireIdentity,
	)
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Authorization", "Bearer t-dsh-8f3a9c2e")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || got.UserID != "dsh-agent" {
		t.Fatalf("valid token: code=%d identity=%+v", w.Code, got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/search", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("no token under require: want 401, got %d", w2.Code)
	}
}

// 未配置任何 token（默认态）→ ConfigureAuth 无 error，registry 保持空（透传）。
func TestConfigureAuth_UnconfiguredPassthrough(t *testing.T) {
	resetAuthState(t)
	if err := ConfigureAuth(AuthOptions{}); err != nil {
		t.Fatalf("ConfigureAuth(unconfigured): %v", err)
	}
	if !registry.empty() {
		t.Fatal("registry must stay empty when nothing configured")
	}
	if requireAuthEnabled() {
		t.Fatal("require flag must stay off by default (红线④)")
	}
}

// ------------------------------------------------------------------
// AuthMiddleware
// ------------------------------------------------------------------

// 正确 token → 200，Identity 注入 context 可回读（含脱敏 TokenID）。
func TestAuthMiddleware_ValidTokenInjectsIdentity(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	var got Identity
	h := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Authorization", "Bearer t-trae-000001")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got.AgentID != "trae" || got.UserID != "skywalk" || got.TenantID != "dshteam" {
		t.Fatalf("identity from context wrong: %+v", got)
	}
	if !strings.Contains(got.TokenID, "***") {
		t.Errorf("TokenID should be redacted, got %q", got.TokenID)
	}
}

// 缺 token → 401，handler 不可达。
func TestAuthMiddleware_MissingToken401(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	reached := false
	h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("want 401 unreached, got code=%d reached=%v", w.Code, reached)
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Fatalf("want error body 'unauthorized', got %q", w.Body.String())
	}
}

// 错 token → 401。
func TestAuthMiddleware_WrongToken401(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	reached := false
	h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Authorization", "Bearer t-forged-999")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("want 401 unreached, got code=%d reached=%v", w.Code, reached)
	}
}

// 默认态（registry 空）→ 透传，不注入身份（红线④，与 R20 逐位一致）。
func TestAuthMiddleware_PassthroughWhenUnconfigured(t *testing.T) {
	resetAuthState(t)
	h := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := IdentityFromContext(r.Context()); ok {
			t.Error("passthrough mode must not inject identity")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil) // 无 Authorization
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("passthrough: want 200, got %d", w.Code)
	}
}

// 前缀 token 必须拒（ConstantTimeCompare 逐条比较；前缀不是合法 token）。
func TestAuthMiddleware_TokenPrefixRejected(t *testing.T) {
	resetAuthState(t)
	full := "t-dsh-8f3a9c2e"
	InitAgentTokens(map[string]AgentIdentity{
		full: {AgentID: "deepseek-harness", UserID: "dsh-agent", TenantID: "dshteam"},
	})
	reached := false
	h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Authorization", "Bearer "+full[:len(full)-2]) // 前缀
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("prefix token: want 401 unreached, got code=%d reached=%v", w.Code, reached)
	}
}

// Bearer scheme 大小写不敏感；非 Bearer / 无 scheme → 401。
func TestAuthMiddleware_BearerSchemeCaseInsensitive(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-wb-000001": {AgentID: "workbuddy", UserID: "skywalk", TenantID: "dshteam"},
	})
	reached := false
	h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Authorization", "bearer t-wb-000001")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("lowercase bearer: want 200 reached, got code=%d reached=%v", w.Code, reached)
	}

	for _, bad := range []string{"Basic t-wb-000001", "t-wb-000001", "Bearer"} {
		hit := false
		h2 := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			hit = true
		}))
		req2 := httptest.NewRequest(http.MethodGet, "/search", nil)
		req2.Header.Set("Authorization", bad)
		w2 := httptest.NewRecorder()
		h2.ServeHTTP(w2, req2)
		if w2.Code != http.StatusUnauthorized || hit {
			t.Errorf("Authorization %q: want 401 unreached, got code=%d reached=%v", bad, w2.Code, hit)
		}
	}
}

// ------------------------------------------------------------------
// RequireIdentity
// ------------------------------------------------------------------

// LIMS_REQUIRE_AUTH=1 + 无身份 → 401；有身份 → 放行。
func TestRequireIdentity_EnforcedWhenEnabled(t *testing.T) {
	resetAuthState(t)
	t.Setenv(EnvRequireAuth, "1")
	h := RequireIdentity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tools/memory/save", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no identity: want 401, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/tools/memory/save", nil)
	req2 = req2.WithContext(withIdentity(req2.Context(), Identity{
		AgentID: "trae", UserID: "skywalk", TenantID: "dshteam",
	}))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("with identity: want 200, got %d", w2.Code)
	}
}

// 未开 LIMS_REQUIRE_AUTH → 透传（默认关，红线④）。
func TestRequireIdentity_PassthroughWhenDisabled(t *testing.T) {
	resetAuthState(t)
	h := RequireIdentity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/tools/memory/save", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled: want 200 passthrough, got %d", w.Code)
	}
}

// ------------------------------------------------------------------
// 写方身份强制（§3.1 防冒充）
// ------------------------------------------------------------------

// **冒充测试（机制级）**：用 agent-trae 的 token，body 写 user_id="agent-dsh"、
// tenant="other-tenant" → 服务端以 token 为准（user_id=skywalk, tenant=dshteam）。
// （真 e2e「落库」断言依赖 C 线 handler 消费 ResolveActor；此处锁定 API 契约。）
func TestResolveActor_IdentityWinsOverBody(t *testing.T) {
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	defer InitAgentTokens(nil)

	var got Identity
	var fallback bool
	h := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, fallback = ResolveActor(r.Context(), "agent-dsh", "other-tenant")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/tools/memory/save", nil)
	req.Header.Set("Authorization", "Bearer t-trae-000001")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if fallback {
		t.Fatal("fallback: got true, want false (identity must come from token)")
	}
	if got.UserID != "skywalk" || got.TenantID != "dshteam" || got.AgentID != "trae" {
		t.Fatalf("identity must be token's, got %+v", got)
	}
}

// 未认证（默认态）→ 回退 body 值，fallback=true（调用方记审计）。
func TestResolveActor_FallbackToBody(t *testing.T) {
	InitAgentTokens(nil) // 透传态
	id, fallback := ResolveActor(context.Background(), "agent-dsh", "other-tenant")
	if !fallback {
		t.Fatal("fallback: got false, want true (unauthenticated)")
	}
	if id.UserID != "agent-dsh" || id.TenantID != "other-tenant" {
		t.Fatalf("fallback to body wrong: %+v", id)
	}
}

// private owner 必须是 token 的 user_id（§3.1 特例）：写别人名下 → 违规 error。
func TestEnforcePrivateOwner(t *testing.T) {
	ctx := withIdentity(context.Background(), Identity{UserID: "skywalk"})

	if err := EnforcePrivateOwner(ctx, "agent-dsh", "private"); err == nil {
		t.Error("private owned by another user: want error, got nil")
	}
	if err := EnforcePrivateOwner(ctx, "skywalk", "private"); err != nil {
		t.Errorf("private owned by self: want nil, got %v", err)
	}
	if err := EnforcePrivateOwner(ctx, "", "private"); err != nil {
		t.Errorf("private with empty owner: want nil (owner falls back to token), got %v", err)
	}
	if err := EnforcePrivateOwner(ctx, "agent-dsh", "tenant"); err != nil {
		t.Errorf("tenant visibility: want nil, got %v", err)
	}
	// 未认证（透传路径）不拦截（保持 R20 行为）
	if err := EnforcePrivateOwner(context.Background(), "agent-dsh", "private"); err != nil {
		t.Errorf("unauthenticated passthrough: want nil, got %v", err)
	}
}

// IdentityFromContext / MustIdentity 语义。
func TestIdentityFromContextAndMust(t *testing.T) {
	if _, ok := IdentityFromContext(nil); ok {
		t.Error("nil ctx: want ok=false")
	}
	if _, ok := IdentityFromContext(context.Background()); ok {
		t.Error("bare ctx: want ok=false")
	}
	id := Identity{AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"}
	got, ok := IdentityFromContext(withIdentity(context.Background(), id))
	if !ok || !reflect.DeepEqual(got, id) {
		t.Fatalf("roundtrip: got %+v ok=%v", got, ok)
	}
	if !reflect.DeepEqual(MustIdentity(withIdentity(context.Background(), id)), id) {
		t.Error("MustIdentity mismatch")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("MustIdentity without identity: want panic")
			}
		}()
		MustIdentity(context.Background())
	}()
}

// ------------------------------------------------------------------
// 辅助函数
// ------------------------------------------------------------------

// 常量时间比较：相等 / 不等长 / 等长不同内容。
func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("t-abc", "t-abc") {
		t.Error("equal strings must compare equal")
	}
	if constantTimeEqual("t-abc", "t-abd") {
		t.Error("different content must not compare equal")
	}
	if constantTimeEqual("t-abc", "t-abcd") {
		t.Error("different length must not compare equal")
	}
}

// token 脱敏：不泄露完整 token。
func TestRedactTokenID(t *testing.T) {
	got := redactTokenID("t-dsh-8f3a9c2e")
	if strings.Contains(got, "8f3a9c2e") {
		t.Errorf("redact leaked full token: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("redact must contain mask: %q", got)
	}
	if got := redactTokenID("short"); strings.Contains(got, "short") {
		t.Errorf("short token leaked: %q", got)
	}
}

// 多 token 隔离：两个 token 各解出各的身份。
func TestAuthMiddleware_MultiTokenIsolation(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-dsh-000001": {AgentID: "deepseek-harness", UserID: "dsh-agent", TenantID: "dshteam"},
		"t-wb-000002":  {AgentID: "workbuddy", UserID: "skywalk", TenantID: "dshteam"},
	})
	var mu sync.Mutex
	seen := map[string]Identity{}
	h := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := IdentityFromContext(r.Context())
		mu.Lock()
		seen[id.AgentID] = id
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	for tk, wantAgent := range map[string]string{
		"t-dsh-000001": "deepseek-harness",
		"t-wb-000002":  "workbuddy",
	} {
		req := httptest.NewRequest(http.MethodGet, "/search", nil)
		req.Header.Set("Authorization", "Bearer "+tk)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token %s: want 200, got %d", tk, w.Code)
		}
		if seen[wantAgent].AgentID != wantAgent {
			t.Errorf("token %s: got agent %q", tk, seen[wantAgent].AgentID)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("want 2 distinct identities, got %d", len(seen))
	}
}

// 并发安全：50 并发请求 + 并发换表，-race 下无数据竞争。
func TestAuthMiddleware_Concurrent(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-conc-0001": {AgentID: "wb", UserID: "skywalk", TenantID: "dshteam"},
	})
	h := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/search", nil)
			if i%2 == 0 {
				req.Header.Set("Authorization", "Bearer t-conc-0001")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			want := http.StatusOK
			if i%2 == 1 {
				want = http.StatusUnauthorized
			}
			if w.Code != want {
				t.Errorf("req %d: want %d, got %d", i, want, w.Code)
			}
		}(i)
	}
	// 并发换表 + 并发读表（registry 的竞态由内部 mutex 保护）
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				InitAgentTokens(map[string]AgentIdentity{
					"t-conc-0001": {AgentID: "wb", UserID: "skywalk", TenantID: "dshteam"},
				})
			} else {
				_, _ = registry.lookup("t-conc-0001")
				_ = registry.empty()
			}
		}(i)
	}
	wg.Wait()
}

// ------------------------------------------------------------------
// R21 T3：运维端点鉴权豁免（G10 回归守护）
// ------------------------------------------------------------------

// TestAuthMiddleware_OperationalEndpointsExempt 运维端点（/healthz、/health、/metrics）
// 必须**免鉴权**：否则 rc.d supervisor 的 `fetch /healthz` 会 401，服务永远无法启动（T3 实测）。
func TestAuthMiddleware_OperationalEndpointsExempt(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	for _, path := range []string{"/healthz", "/health", "/metrics"} {
		reached := false
		h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			reached = true
		}))
		req := httptest.NewRequest(http.MethodGet, path, nil) // 故意不带 token
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !reached {
			t.Errorf("%s: want 200 且 handler 可达（免鉴权），got code=%d reached=%v", path, w.Code, reached)
		}
	}
}

// TestAuthMiddleware_BusinessEndpointsStillGuarded 豁免**只针对运维端点**；
// 业务端点（/search、/tools/*、/audit）在配置 token 后仍必须 401。
func TestAuthMiddleware_BusinessEndpointsStillGuarded(t *testing.T) {
	resetAuthState(t)
	InitAgentTokens(map[string]AgentIdentity{
		"t-trae-000001": {AgentID: "trae", UserID: "skywalk", TenantID: "dshteam"},
	})
	for _, path := range []string{
		"/search", "/tools/memory/save", "/tools/memory/recall",
		"/tools/memory/plan", "/audit", "/memories",
	} {
		reached := false
		h := AuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			reached = true
		}))
		req := httptest.NewRequest(http.MethodGet, path, nil) // 不带 token
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s: want 401 且 handler 不可达，got code=%d reached=%v", path, w.Code, reached)
		}
	}
}

// TestRequireIdentity_OperationalEndpointsExempt RequireIdentity 同口径豁免，
// 否则 LIMS_REQUIRE_AUTH=1 时健康检查同样会被挡住（服务起不来）。
func TestRequireIdentity_OperationalEndpointsExempt(t *testing.T) {
	resetAuthState(t)
	t.Setenv(EnvRequireAuth, "1")
	for _, path := range []string{"/healthz", "/metrics"} {
		reached := false
		h := RequireIdentity(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			reached = true
		}))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !reached {
			t.Errorf("%s under RequireIdentity: want 200 reached, got code=%d reached=%v", path, w.Code, reached)
		}
	}
	// 业务端点仍应 401
	reached := false
	h := RequireIdentity(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || reached {
		t.Errorf("/search under RequireIdentity: want 401 unreached, got code=%d reached=%v", w.Code, reached)
	}
}

// TestSingleIdentity 校验 stdio 单身份解析（R21 T3 解 D-1）：
// 恰好一条 → 返回该 token 与身份；0 条或多条 → 不返回（不猜测，避免身份错配）。
func TestSingleIdentity(t *testing.T) {
	resetAuthState(t)
	if _, _, ok := SingleIdentity(); ok {
		t.Error("空表不应返回单身份")
	}
	InitAgentTokens(map[string]AgentIdentity{
		"t-codearts-000001": {AgentID: "codearts", UserID: "skywalk", TenantID: "dshteam"},
	})
	tok, id, ok := SingleIdentity()
	if !ok || tok != "t-codearts-000001" || id.AgentID != "codearts" ||
		id.UserID != "skywalk" || id.TenantID != "dshteam" {
		t.Fatalf("单条表解析错误: tok=%q id=%+v ok=%v", tok, id, ok)
	}
	// 多条 → 必须拒绝（stdio 无法区分身份，宁可无身份也不冒充）
	InitAgentTokens(map[string]AgentIdentity{
		"t-a-000001": {AgentID: "a", UserID: "u", TenantID: "t"},
		"t-b-000002": {AgentID: "b", UserID: "u", TenantID: "t"},
	})
	if _, _, ok := SingleIdentity(); ok {
		t.Error("多条 token 时不应返回单身份（否则身份可能错配）")
	}
}

// TestInjectIdentityArgsWithFallback stdio 无 context 身份时，
// 回退到进程级单身份注入 tenant_id/user_id/agent_id（否则 REST 端 400 tenant_id required）。
func TestInjectIdentityArgsWithFallback(t *testing.T) {
	resetAuthState(t)
	fb := Identity{AgentID: "codearts", UserID: "skywalk", TenantID: "dshteam"}
	args := injectIdentityArgsWith(context.Background(), map[string]any{}, &fb)
	if args["tenant_id"] != "dshteam" || args["user_id"] != "skywalk" || args["agent_id"] != "codearts" {
		t.Fatalf("回退身份未注入: %+v", args)
	}
	// 无回退且无 context → 原样返回（零回归）
	args2 := injectIdentityArgsWith(context.Background(), map[string]any{}, nil)
	if _, exists := args2["tenant_id"]; exists {
		t.Fatalf("无身份时不应注入 tenant_id: %+v", args2)
	}
}
