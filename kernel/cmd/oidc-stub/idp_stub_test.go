package main

// oidc-stub 单测：用 httptest 起 IdP，直连 /auth 与 /token 验证登录页表单、
// 错误密码拒绝、参数缺失、授权码换令牌（含 RS256 验签）与 PKCE 拒绝路径。零外网。

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testCB        = "http://frontend.example/login/sso/callback"
	testVerifier  = "abcdefghij0123456789ABCDEFGHIJ0123456789"
	testChallenge = "qQ2YkL8n0VJ5zZ9cQ2YkL8n0VJ5zZ9cQ2YkL8n0VJ5zZ9c" // 非真实值，仅占位
)

// newTestIDP 起测试 IdP 并返回其 http server 与实例。
func newTestIDP(t *testing.T, auto bool) (*httptest.Server, *idp) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	s := &idp{key: key, kid: "testkid", clientID: "demo-client", secret: "demo-secret",
		auto: auto, ttl: time.Minute, codes: map[string]authCode{}}
	s.user.Sub, s.user.Name, s.user.Email = "sub-1", "Test User", "t@example.com"
	s.user.Login, s.user.Password = "test-user", "testpass"
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("/jwks", s.handleJWKS)
	mux.HandleFunc("/auth", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	s.issuer = ts.URL
	return ts, s
}

// authParams 构造合法的授权请求 query。
func authParams() url.Values {
	q := url.Values{}
	q.Set("client_id", "demo-client")
	q.Set("redirect_uri", testCB)
	q.Set("response_type", "code")
	q.Set("state", "st-1")
	q.Set("nonce", "n-1")
	q.Set("code_challenge", testChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", "openid profile email")
	return q
}

// testClient 不跟随重定向（回调域名在测试里不存在，需断言 302 Location 本身）。
var testClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// do 发送请求返回 (status, Location, body)。
func do(t *testing.T, method, rawURL string, form url.Values) (int, string, string) {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("do %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 8192)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, resp.Header.Get("Location"), string(b[:n])
}

// codeFromLocation 从 302 Location 提取授权码。
func codeFromLocation(t *testing.T, loc string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in location %s", loc)
	}
	return code
}

// doLogin POST 登录表单，返回 status/Location/body。
func doLogin(t *testing.T, ts *httptest.Server, q url.Values, username, password string) (int, string, string) {
	t.Helper()
	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("username", username)
	form.Set("password", password)
	return do(t, http.MethodPost, ts.URL+"/auth", form)
}

// postToken POST /token 换码。
func postToken(t *testing.T, ts *httptest.Server, form url.Values) (int, map[string]any) {
	t.Helper()
	st, _, body := do(t, http.MethodPost, ts.URL+"/token", form)
	var out map[string]any
	if body != "" {
		_ = json.Unmarshal([]byte(body), &out)
	}
	return st, out
}

// verifyRS256 验签 RS256 ID Token 并返回 claims。
func verifyRS256(t *testing.T, key *rsa.PublicKey, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token format: %s", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("RS256 verify failed: %v", err)
	}
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	claims := map[string]any{}
	_ = json.Unmarshal(pb, &claims)
	return claims
}

func TestIDPDiscoveryAndJWKS(t *testing.T) {
	ts, s := newTestIDP(t, false)
	st, _, body := do(t, http.MethodGet, ts.URL+"/.well-known/openid-configuration", nil)
	if st != http.StatusOK {
		t.Fatalf("discovery status %d", st)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(body), &d)
	if d["issuer"] != s.issuer {
		t.Errorf("issuer = %v, want %s", d["issuer"], s.issuer)
	}
	if d["id_token_signing_alg_values_supported"].([]any)[0] != "RS256" {
		t.Errorf("alg = %v", d["id_token_signing_alg_values_supported"])
	}

	st, _, body = do(t, http.MethodGet, ts.URL+"/jwks", nil)
	if st != http.StatusOK {
		t.Fatalf("jwks status %d", st)
	}
	var j map[string][]map[string]string
	_ = json.Unmarshal([]byte(body), &j)
	k := j["keys"][0]
	if k["kid"] != "testkid" || k["n"] == "" || k["e"] == "" {
		t.Errorf("jwks keys = %v", j["keys"])
	}
}

func TestAuthLoginPageRenders(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	st, _, body := do(t, http.MethodGet, ts.URL+"/auth?"+authParams().Encode(), nil)
	if st != http.StatusOK {
		t.Fatalf("login page status %d", st)
	}
	for _, want := range []string{`name="password"`, `name="username"`, `name="state"`, `name="nonce"`, `name="code_challenge"`, "登录并授权"} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
}

func TestAuthRejectsWrongPassword(t *testing.T) {
	ts, s := newTestIDP(t, false)
	q := authParams()
	st, loc, body := doLogin(t, ts, q, "test-user", "wrongpass")
	if st != http.StatusOK || loc != "" {
		t.Fatalf("wrong password: got %d loc=%q, want 200 login page", st, loc)
	}
	if !strings.Contains(body, "用户名或密码错误") {
		t.Errorf("missing error message: %s", body)
	}
	s.mu.Lock()
	n := len(s.codes)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("wrong password must not issue a code, got %d codes", n)
	}
}

func TestAuthRejectsUnknownUser(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	st, _, body := doLogin(t, ts, authParams(), "nobody", "testpass")
	if st != http.StatusOK || !strings.Contains(body, "用户名或密码错误") {
		t.Errorf("unknown user: got %d body=%s", st, body)
	}
}

func TestAuthLoginRedirectsWithCode(t *testing.T) {
	ts, s := newTestIDP(t, false)
	q := authParams()
	st, loc, _ := doLogin(t, ts, q, "test-user", "testpass")
	if st != http.StatusFound {
		t.Fatalf("login status %d, want 302", st)
	}
	u, _ := url.Parse(loc)
	if !strings.HasPrefix(u.String(), testCB+"?") {
		t.Errorf("redirect location = %q", loc)
	}
	if u.Query().Get("state") != "st-1" || u.Query().Get("code") == "" {
		t.Errorf("location missing state/code: %q", loc)
	}
	// 登录成功后应已签发授权码。
	s.mu.Lock()
	n := len(s.codes)
	s.mu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 auth code, got %d", n)
	}
}

func TestAuthMissingParamsRejected(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	cases := []struct {
		name string
		drop string
	}{
		{"missing nonce", "nonce"},
		{"missing code_challenge", "code_challenge"},
		{"missing redirect_uri", "redirect_uri"},
		{"wrong response_type", "response_type"},
		{"unknown client_id", "client_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := authParams()
			if c.drop == "unknown client_id" {
				q.Set("client_id", "other")
			} else if c.drop == "wrong response_type" {
				q.Set("response_type", "token")
			} else {
				q.Del(c.drop)
			}
			st, _, _ := do(t, http.MethodGet, ts.URL+"/auth?"+q.Encode(), nil)
			if st != http.StatusBadRequest {
				t.Errorf("%s: status %d, want 400", c.name, st)
			}
		})
	}
}

func TestAutoAuthorizeSkipsLogin(t *testing.T) {
	ts, _ := newTestIDP(t, true)
	st, loc, _ := do(t, http.MethodGet, ts.URL+"/auth?"+authParams().Encode(), nil)
	if st != http.StatusFound {
		t.Fatalf("auto authorize status %d, want 302", st)
	}
	if !strings.HasPrefix(loc, testCB+"?code=") {
		t.Errorf("location = %q", loc)
	}
}

func TestTokenExchangeAndVerify(t *testing.T) {
	ts, s := newTestIDP(t, false)
	q := authParams()
	q.Set("code_challenge", pkceChallenge(t, testVerifier))
	_, loc, _ := doLogin(t, ts, q, "test-user", "testpass")
	code := codeFromLocation(t, loc)

	st, tok := postToken(t, ts, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {"demo-client"},
	})
	if st != http.StatusOK {
		t.Fatalf("token status %d body=%v", st, tok)
	}
	idToken, _ := tok["id_token"].(string)
	if idToken == "" {
		t.Fatalf("no id_token: %v", tok)
	}
	claims := verifyRS256(t, &s.key.PublicKey, idToken)
	for k, want := range map[string]any{"iss": ts.URL, "sub": "sub-1", "aud": "demo-client", "nonce": "n-1", "name": "Test User"} {
		if claims[k] != want {
			t.Errorf("claim %s = %v, want %v", k, claims[k], want)
		}
	}
	if _, ok := claims["exp"].(float64); !ok {
		t.Errorf("missing exp claim")
	}
}

func TestTokenRejectsWrongVerifier(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	// 用「由 verifier 推导的 challenge」登录，再用错误 verifier 换码应被拒。
	q := authParams()
	q.Set("code_challenge", pkceChallenge(t, testVerifier))
	_, loc, _ := doLogin(t, ts, q, "test-user", "testpass")
	code := codeFromLocation(t, loc)

	st, tok := postToken(t, ts, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {"000000000000000000000000000000000000000000000"}, "client_id": {"demo-client"},
	})
	if st != http.StatusBadRequest {
		t.Errorf("wrong verifier status %d, want 400 (%v)", st, tok)
	}
	if tok["error"] != "invalid_grant" {
		t.Errorf("error = %v, want invalid_grant", tok["error"])
	}
}

func TestTokenCodeSingleUse(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	q := authParams()
	q.Set("code_challenge", pkceChallenge(t, testVerifier))
	_, loc, _ := doLogin(t, ts, q, "test-user", "testpass")
	code := codeFromLocation(t, loc)
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {"demo-client"},
	}
	if st, _ := postToken(t, ts, form); st != http.StatusOK {
		t.Fatalf("first exchange status %d", st)
	}
	st, tok := postToken(t, ts, form)
	if st != http.StatusBadRequest {
		t.Errorf("reuse status %d, want 400 (%v)", st, tok)
	}
}

func TestTokenExpiredCode(t *testing.T) {
	ts, s := newTestIDP(t, false)
	q := authParams()
	q.Set("code_challenge", pkceChallenge(t, testVerifier))
	_, loc, _ := doLogin(t, ts, q, "test-user", "testpass")
	code := codeFromLocation(t, loc)
	s.mu.Lock()
	ac := s.codes[code]
	ac.ExpiresAt = time.Now().Add(-time.Minute)
	s.codes[code] = ac
	s.mu.Unlock()
	st, tok := postToken(t, ts, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {"demo-client"},
	})
	if st != http.StatusBadRequest || tok["error"] != "invalid_grant" {
		t.Errorf("expired code: status %d tok=%v", st, tok)
	}
}

func TestTokenRejectsBadGrantType(t *testing.T) {
	ts, _ := newTestIDP(t, false)
	st, tok := postToken(t, ts, url.Values{
		"grant_type": {"client_credentials"}, "code": {"x"}, "code_verifier": {testVerifier},
	})
	if st != http.StatusBadRequest || tok["error"] != "invalid_request" {
		t.Errorf("bad grant: status %d tok=%v", st, tok)
	}
}

// pkceChallenge 按 S256 推导 challenge（测试用，与生产一致）。
func pkceChallenge(t *testing.T, verifier string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
