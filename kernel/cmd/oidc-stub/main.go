// lightmem-oidc-stub 是 R23.3 SSO 的本地测试 OIDC Provider。
//
// 用途：让前台 lightmem-frontend 的 SSO 全链路能在本地/CI 里真机跑通
// （发现 → 授权 → 换码 → ID Token 验签 → 建会话），无需真实 IdP 与公网。
//
// ⚠️ 仅限测试/演示，严禁用于生产：
//   - 内置固定测试账号，登录页表单校验明文密码（仅测试）；
//   - RSA 密钥默认启动时临时生成（不落盘），-key 指定则从 PEM 加载以便重启复用；
//   - 不支持真实用户注册/登录/撤销令牌等能力；
//   - 授权端点默认走登录页；CI/无头场景可用 -auto-authorize 直接放行。
//
// 零第三方依赖（红线⑤），仅 stdlib。
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"html"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	listen        = flag.String("listen", "127.0.0.1:18091", "IdP 监听地址")
	issuer        = flag.String("issuer", "", "issuer；空则按监听地址推断")
	keyPath       = flag.String("key", "", "RSA 私钥 PEM 路径；空则启动时临时生成（不落盘）")
	clientID      = flag.String("client-id", "demo-client", "允许的客户端 ID")
	clientSecret  = flag.String("client-secret", "demo-secret", "允许的客户端密钥（confidential client）")
	kid           = flag.String("kid", "idp-stub-1", "JWKS key id")
	userName      = flag.String("user-name", "Test User", "测试账号显示名")
	userEmail     = flag.String("user-email", "test@example.com", "测试账号邮箱")
	userSub       = flag.String("user-sub", "user-test-001", "测试账号 sub（稳定唯一）")
	userLogin     = flag.String("user-login", "test-user", "测试账号登录名（登录页输入）")
	userPassword  = flag.String("user-password", "testpass", "测试账号密码（登录页输入，明文，仅测试）")
	autoAuthorize = flag.Bool("auto-authorize", false, "授权端点直接放行（不显示登录页）；默认 false=走登录页")
	ttl           = flag.Duration("token-ttl", 10*time.Minute, "授权码/令牌有效期")
)

// authCode 是授权码对应的服务端状态（一次性、短 TTL）。
type authCode struct {
	Nonce         string
	CodeChallenge string
	ClientID      string
	RedirectURI   string
	Sub           string
	ExpiresAt     time.Time
}

type idp struct {
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	clientID string
	secret   string
	auto     bool
	user     struct{ Sub, Name, Email, Login, Password string }
	ttl      time.Duration

	mu    sync.Mutex
	codes map[string]authCode
}

func main() {
	flag.Parse()
	key, err := loadOrGenKey(*keyPath)
	if err != nil {
		log.Fatalf("[boot] key: %v", err)
	}
	iss := strings.TrimRight(*issuer, "/")
	if iss == "" {
		iss = "http://" + *listen
	}
	s := &idp{
		key: key, kid: *kid, issuer: iss,
		clientID: *clientID, secret: *clientSecret, ttl: *ttl, auto: *autoAuthorize,
		codes: map[string]authCode{},
	}
	s.user.Sub, s.user.Name, s.user.Email = *userSub, *userName, *userEmail
	s.user.Login, s.user.Password = *userLogin, *userPassword

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("/jwks", s.handleJWKS)
	mux.HandleFunc("/auth", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)

	srv := &http.Server{Addr: *listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	log.Printf("[boot] lightmem-oidc-stub (TEST ONLY) on %s", iss)
	log.Printf("[boot] discovery=%s/.well-known/openid-configuration auth=%s/auth token=%s/token jwks=%s/jwks",
		iss, iss, iss, iss)
	log.Printf("[boot] test account sub=%s name=%q email=%q login=%q", s.user.Sub, s.user.Name, s.user.Email, s.user.Login)
	log.Fatalf("serve: %v", srv.ListenAndServe())
}

func (s *idp) handleHome(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>lightmem OIDC stub (TEST ONLY)</title>
<h1>lightmem OIDC Provider Stub</h1>
<p class="warn"><b>测试/演示专用，严禁用于生产。</b>默认走登录页表单（固定测试账号），
授权端点直接放行需显式 <code>-auto-authorize</code>；密钥默认临时生成。</p>
<ul>
<li>Issuer: ` + s.issuer + `</li>
<li>Discovery: ` + s.issuer + `/.well-known/openid-configuration</li>
<li>Authorization: ` + s.issuer + `/auth</li>
<li>Token: ` + s.issuer + `/token</li>
<li>JWKS: ` + s.issuer + `/jwks</li>
<li>Client: <code>` + s.clientID + `</code></li>
<li>Login: <code>` + s.user.Login + `</code> / <code>` + s.user.Password + `</code></li>
<li>Test account sub=<code>` + s.user.Sub + `</code> name=<code>` + s.user.Name + `</code></li>
</ul>
<p>前端启动示例：<code>-oidc-issuer ` + s.issuer + ` -oidc-client-id ` + s.clientID +
		` -oidc-client-secret ` + s.secret + `</code></p>`))
}

func (s *idp) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/auth",
		"token_endpoint":                        s.issuer + "/token",
		"jwks_uri":                              s.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	})
}

func (s *idp) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "kid": s.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(eBytes(pub.E)),
		}},
	})
}

// handleAuthorize 是授权端点：
//   - auto-authorize=true 时直接发码 302 回回调（CI/无头场景）；
//   - 默认走 IdP 登录页：GET 渲染表单，POST 校验账号密码后发码 302 回回调。
func (s *idp) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	// r.Form 合并 URL query 与表单 body（POST 登录页提交时参数在 body）。
	_ = r.ParseForm()
	q := r.Form
	client, redirectURI, responseType := q.Get("client_id"), q.Get("redirect_uri"), q.Get("response_type")
	state, nonce, challenge := q.Get("state"), q.Get("nonce"), q.Get("code_challenge")
	bad := func(msg string) {
		http.Error(w, msg+" (state="+state+")", http.StatusBadRequest)
	}
	if responseType != "code" {
		bad("response_type must be code")
		return
	}
	if client != s.clientID {
		bad("unknown client_id")
		return
	}
	if !strings.HasPrefix(redirectURI, "http") || nonce == "" || challenge == "" {
		bad("missing redirect_uri/nonce/code_challenge")
		return
	}
	if s.auto {
		s.redirectWithCode(w, r, client, redirectURI, state, nonce, challenge)
		return
	}
	if r.Method == http.MethodPost {
		s.authorizeLogin(w, r, client, redirectURI, state, nonce, challenge)
		return
	}
	s.renderLoginPage(w, "", client, redirectURI, state, nonce, challenge, r.Form.Get("scope"))
}

// redirectWithCode 生成一次性授权码并 302 回回调。
func (s *idp) redirectWithCode(w http.ResponseWriter, r *http.Request, client, redirectURI, state, nonce, challenge string) {
	code, err := randomToken(24)
	if err != nil {
		http.Error(w, "rng failure", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.codes[code] = authCode{
		Nonce: nonce, CodeChallenge: challenge, ClientID: client,
		RedirectURI: redirectURI, Sub: s.user.Sub, ExpiresAt: time.Now().Add(s.ttl),
	}
	s.mu.Unlock()
	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	http.Redirect(w, r, redirectURI+sep+"code="+code+"&state="+state, http.StatusFound)
}

// authorizeLogin 处理登录表单提交：常数时间比较校验账号密码，通过后发码。
func (s *idp) authorizeLogin(w http.ResponseWriter, r *http.Request, client, redirectURI, state, nonce, challenge string) {
	_ = r.ParseForm()
	login, password := r.FormValue("username"), r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(login), []byte(s.user.Login)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(s.user.Password)) != 1 {
		s.renderLoginPage(w, "用户名或密码错误", client, redirectURI, state, nonce, challenge, r.FormValue("scope"))
		return
	}
	s.redirectWithCode(w, r, client, redirectURI, state, nonce, challenge)
}

// renderLoginPage 渲染 IdP 登录页，隐藏字段带回原授权参数。
func (s *idp) renderLoginPage(w http.ResponseWriter, errMsg, client, redirectURI, state, nonce, challenge, scope string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	hidden := ""
	for k, v := range map[string]string{
		"client_id": client, "redirect_uri": redirectURI, "state": state,
		"nonce": nonce, "code_challenge": challenge, "scope": scope, "response_type": "code",
	} {
		hidden += `<input type="hidden" name="` + k + `" value="` + html.EscapeString(v) + `">`
	}
	errBlock := ""
	if errMsg != "" {
		errBlock = `<p class="err">` + html.EscapeString(errMsg) + `</p>`
	}
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>登录 · lightmem OIDC stub (TEST ONLY)</title>
<style>body{font-family:system-ui,sans-serif;max-width:400px;margin:3rem auto;padding:0 1rem}
label{display:block;margin:.5rem 0 .2rem;font-size:.9rem}input[type=text],input[type=password]{width:100%;box-sizing:border-box;padding:.45rem;border:1px solid #ccc;border-radius:4px}
button{margin-top:1rem;padding:.5rem 1.2rem;border:0;border-radius:4px;background:#2563eb;color:#fff;cursor:pointer}
.err{color:#b91c1c}.warn{color:#92400e;font-size:.85rem;border:1px solid #fcd34d;padding:.5rem;border-radius:4px}</style>
<h2>登录 lightmem</h2>
<p class="warn">测试/演示专用 IdP（` + html.EscapeString(s.issuer) + `）</p>
` + errBlock + `
<form method="post" action="` + html.EscapeString(s.issuer) + `/auth">
` + hidden + `
<label for="u">用户名</label><input id="u" name="username" autocomplete="username" value="` + html.EscapeString(s.user.Login) + `">
<label for="p">密码</label><input id="p" name="password" type="password" autocomplete="current-password">
<button type="submit">登录并授权</button>
</form>
<p class="warn">测试账号：<code>` + html.EscapeString(s.user.Login) + `</code> / <code>` + html.EscapeString(s.user.Password) + `</code></p>`))
}

// handleToken 是令牌端点：校验授权码 + PKCE verifier，签发 ID Token（RS256）。
func (s *idp) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	grant, code, verifier := r.FormValue("grant_type"), r.FormValue("code"), r.FormValue("code_verifier")
	client := r.FormValue("client_id")
	// confidential client 支持 client_secret_basic（user 为 URL-escaped client_id）。
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		client = u
	}
	if grant != "authorization_code" || code == "" || verifier == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	ac, ok := s.codes[code]
	delete(s.codes, code) // 一次性
	s.mu.Unlock()
	if !ok || time.Now().After(ac.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	if ac.ClientID != client && client != s.clientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != ac.CodeChallenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "pkce verifier mismatch"})
		return
	}
	now := time.Now()
	idToken, err := s.signRS256(map[string]any{
		"iss": s.issuer, "sub": ac.Sub, "aud": ac.ClientID,
		"exp": now.Add(s.ttl).Unix(), "iat": now.Unix(),
		"nonce": ac.Nonce, "name": s.user.Name, "email": s.user.Email,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	at, _ := randomToken(24)
	writeJSON(w, http.StatusOK, map[string]any{
		"id_token": idToken, "access_token": at, "token_type": "Bearer",
		"expires_in": int(s.ttl.Seconds()), "scope": "openid profile email",
	})
}

func (s *idp) signRS256(claims map[string]any) (string, error) {
	hb, err := json.Marshal(map[string]string{"alg": "RS256", "kid": s.kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(raw))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return raw + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func eBytes(e int) []byte {
	return new(big.Int).SetInt64(int64(e)).Bytes()
}

func loadOrGenKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		log.Printf("[boot] generated ephemeral RSA key (in-memory, not persisted)")
		return k, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, os.ErrInvalid
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, os.ErrInvalid
	}
	log.Printf("[boot] loaded RSA key from %s", path)
	return rk, nil
}
