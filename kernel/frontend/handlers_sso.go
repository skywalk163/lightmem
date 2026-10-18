package frontend

// R23.3 SSO/OIDC 登录 handler。职责：
//   - GET /login/sso            生成 state/nonce/PKCE 并跳转 IdP 授权端点
//   - GET /login/sso/callback   用 code 换 ID Token → 验签 → 映射身份 → 后台签 token → 建会话
//
// 与后台的对接遵循 R22.4"后台不验签，只认结果"：验签在本进程完成，
// 之后的身份直接走已有 `IssueSession`（对接路径①）签 `auth_tokens`。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ssoEntry 是一次授权码流所需的服务端状态（一次性、短 TTL）。
type ssoEntry struct {
	Nonce    string
	Verifier string
	Redirect string
	Created  time.Time
}

// ssoStateStore 用内存保存 state→entry；state 是回调的 CSRF/防重放锚点。
type ssoStateStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]ssoEntry
}

func newSSOStateStore(ttl time.Duration) *ssoStateStore {
	return &ssoStateStore{ttl: ttl, m: map[string]ssoEntry{}}
}

func (st *ssoStateStore) put(state string, e ssoEntry) {
	if e.Created.IsZero() {
		e.Created = time.Now()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.gcLocked(time.Now())
	st.m[state] = e
}

// take 取用即删除（一次性），并顺带清理过期项。
func (st *ssoStateStore) take(state string) (ssoEntry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	st.gcLocked(now)
	e, ok := st.m[state]
	if !ok {
		return ssoEntry{}, false
	}
	delete(st.m, state)
	return e, true
}

func (st *ssoStateStore) gcLocked(now time.Time) {
	for k, v := range st.m {
		if now.Sub(v.Created) > st.ttl {
			delete(st.m, k)
		}
	}
}

// handleSSOStart 发起 SSO 登录。
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "SSO 未配置（接线默认关）", http.StatusServiceUnavailable)
		return
	}
	if s.hasSession(r) {
		http.Redirect(w, r, "/workspace", http.StatusSeeOther)
		return
	}
	state, err := ssoRandomToken(16)
	if err != nil {
		http.Error(w, "SSO 初始化失败", http.StatusInternalServerError)
		return
	}
	nonce, err := newOIDCNonce()
	if err != nil {
		http.Error(w, "SSO 初始化失败", http.StatusInternalServerError)
		return
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		http.Error(w, "SSO 初始化失败", http.StatusInternalServerError)
		return
	}
	redirect := strings.TrimSpace(r.FormValue("next"))
	if redirect == "" || !strings.HasPrefix(redirect, "/") {
		redirect = "/workspace"
	}
	s.sso.put(state, ssoEntry{Nonce: nonce, Verifier: verifier, Redirect: redirect, Created: time.Now()})
	authURL, err := s.oidc.authorizationURL(state, nonce, challenge)
	if err != nil {
		log.Printf("[frontend] sso start: %v", err)
		http.Error(w, "SSO 授权地址不可用", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleSSOCallback 处理 IdP 回调：换码、验签、映射身份、后台签 token、建会话。
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "SSO 未配置（接线默认关）", http.StatusServiceUnavailable)
		return
	}
	fail := func(msg string) {
		log.Printf("[frontend] sso callback failed: %s", msg)
		d := s.newPageData(w, r)
		d.Error = "SSO 登录失败：" + msg
		d.User = nil
		s.tpl.render(w, "login.html", d)
	}
	q := r.URL.Query()
	state, code := q.Get("state"), q.Get("code")
	if e := q.Get("error"); e != "" {
		fail("IdP 拒绝授权：" + e)
		return
	}
	if state == "" || code == "" {
		fail("回调参数缺失")
		return
	}
	entry, ok := s.sso.take(state)
	if !ok {
		fail("state 无效或已过期（请重新发起登录）")
		return
	}
	ctx, cancel := s.bgctx(r)
	defer cancel()

	tr, err := s.oidc.exchange(ctx, code, entry.Verifier)
	if err != nil {
		fail("换取令牌失败")
		return
	}
	id, err := s.oidc.verifyIDToken(ctx, tr.IDToken, entry.Nonce)
	if err != nil {
		fail("令牌校验失败")
		return
	}
	b := s.backendReady()
	if b == nil {
		fail("服务暂不可用")
		return
	}
	userID := ssoUserID(id.Issuer, id.Subject)
	display := firstNonEmpty(id.Name, id.Email, userID)
	token, sid, err := b.IssueSession(ctx, userID, s.cfg.DefaultTenant, s.cfg.DefaultRoles, "")
	if err != nil {
		log.Printf("[frontend] sso issue session for %q: %v", userID, err)
		fail("建立会话失败，请稍后重试")
		return
	}
	s.setSessionCookie(w, r, token, int(s.cfg.SessionTTL.Seconds()))
	s.setSessionIDCookie(w, sid)
	s.setUserCookie(w, userID, display, s.cfg.DefaultTenant)
	if err := s.store.RecordLogin(r.Context(), userID, clientIP(r), r.UserAgent()); err != nil {
		logErr("record sso login", err)
	}
	http.Redirect(w, r, entry.Redirect, http.StatusSeeOther)
}

// ssoUserID 把 IdP 的 sub 映射为后台稳定 user_id：
// 加 issuer 前缀哈希，避免不同 IdP 的 sub 撞号（如 GitHub 数字 sub vs 其它）。
func ssoUserID(issuer, sub string) string {
	h := sha256.Sum256([]byte(issuer + "|" + sub))
	return "sso-" + hex.EncodeToString(h[:])[:20]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ssoRandomToken 生成 n 字节随机 token 的 base64url 形式（用于 state）。
func ssoRandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64urlEnc(b), nil
}
