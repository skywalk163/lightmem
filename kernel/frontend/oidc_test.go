package frontend

// R23.3 OIDC 客户端单测：用 httptest 起一个"本地 IdP stub"（RS256 自签 JWT + JWKS），
// 全流程在进程内验证 discover → authorizationURL → exchange → verifyIDToken，
// 以及各类验签失败路径（错 nonce / 过期 / 错 issuer / 篡改签名）。零外网依赖。

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// idpStub 是极简 OIDC Provider：只提供发现文档、JWKS、token 端点三个端点。
type idpStub struct {
	srv          *httptest.Server
	key          *rsa.PrivateKey
	kid          string
	mu           sync.Mutex
	lastNonce    string
	lastCode     string
	lastVerifier string
	// 覆盖用：强制签发过期 / 错 issuer 的 token。
	forceExpired bool
	forceIssuer  string
}

func newIDPStub(t *testing.T) *idpStub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa key: %v", err)
	}
	s := &idpStub{key: key, kid: "testkey-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("/jwks", s.handleJWKS)
	mux.HandleFunc("/token", s.handleToken)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *idpStub) issuer() string { return s.srv.URL }

func (s *idpStub) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"issuer":                 s.issuer(),
		"authorization_endpoint": s.srv.URL + "/auth",
		"token_endpoint":         s.srv.URL + "/token",
		"jwks_uri":               s.srv.URL + "/jwks",
	})
}

func (s *idpStub) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "kid": s.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func (s *idpStub) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")
	clientID := r.FormValue("client_id")
	if code == "" || verifier == "" || clientID != "client-a" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	nonce := s.lastNonce
	expired := s.forceExpired
	iss := s.forceIssuer
	if iss == "" {
		iss = s.issuer()
	}
	s.mu.Unlock()

	exp := time.Now().Add(5 * time.Minute).Unix()
	if expired {
		exp = time.Now().Add(-time.Hour).Unix()
	}
	idToken := s.sign(map[string]any{
		"iss": iss, "sub": "user-42", "aud": "client-a", "exp": exp,
		"iat": time.Now().Unix(), "nonce": nonce, "name": "SSO User", "email": "u@example.com",
	})
	writeJSON(w, http.StatusOK, map[string]any{"id_token": idToken, "access_token": "at-1", "token_type": "Bearer", "expires_in": 300})
}

func (s *idpStub) setNonce(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastNonce = nonce
}

func (s *idpStub) setForce(expired bool, issuer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceExpired = expired
	s.forceIssuer = issuer
}

func (s *idpStub) sign(claims map[string]any) string {
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": s.kid, "typ": "JWT"})
	pb, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	raw := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(raw))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return raw + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func testOIDCConfig(issuer string) OIDCConfig {
	return OIDCConfig{
		Issuer: issuer, ClientID: "client-a", ClientSecret: "secret",
		RedirectURI: "http://frontend.test/login/sso/callback", Scopes: []string{"openid", "profile", "email"},
	}
}

func TestOIDCFullFlow(t *testing.T) {
	stub := newIDPStub(t)
	client, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatalf("newOIDCClient: %v", err)
	}

	nonce, err := newOIDCNonce()
	if err != nil {
		t.Fatal(err)
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	state, err := ssoRandomToken(16)
	if err != nil {
		t.Fatal(err)
	}

	authURL, err := client.authorizationURL(state, nonce, challenge)
	if err != nil {
		t.Fatalf("authorizationURL: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method"} {
		if u.Query().Get(want) == "" {
			t.Errorf("authURL missing %q", want)
		}
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("want S256, got %q", u.Query().Get("code_challenge_method"))
	}
	if u.Query().Get("nonce") != nonce {
		t.Errorf("nonce not propagated")
	}
	stub.setNonce(nonce)

	tr, err := client.exchange(t.Context(), "code-1", verifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tr.IDToken == "" {
		t.Fatal("empty id_token")
	}
	id, err := client.verifyIDToken(t.Context(), tr.IDToken, nonce)
	if err != nil {
		t.Fatalf("verifyIDToken: %v", err)
	}
	if id.Subject != "user-42" {
		t.Errorf("sub = %q, want user-42", id.Subject)
	}
	if id.Name != "SSO User" || id.Email != "u@example.com" {
		t.Errorf("claims = %+v", id)
	}
}

func TestOIDCVerifyRejectsWrongNonce(t *testing.T) {
	stub := newIDPStub(t)
	client, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatal(err)
	}
	stub.setNonce("expected-nonce")
	token := stub.sign(map[string]any{
		"iss": stub.issuer(), "sub": "user-42", "aud": "client-a",
		"exp": time.Now().Add(time.Minute).Unix(), "nonce": "expected-nonce",
	})
	if _, err := client.verifyIDToken(t.Context(), token, "wrong-nonce"); err == nil {
		t.Error("want nonce mismatch error, got nil")
	}
}

func TestOIDCVerifyRejectsExpired(t *testing.T) {
	stub := newIDPStub(t)
	client, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatal(err)
	}
	stub.setNonce("n")
	token := stub.sign(map[string]any{
		"iss": stub.issuer(), "sub": "user-42", "aud": "client-a",
		"exp": time.Now().Add(-time.Hour).Unix(), "nonce": "n",
	})
	if _, err := client.verifyIDToken(t.Context(), token, "n"); err == nil {
		t.Error("want expired error, got nil")
	}
}

func TestOIDCVerifyRejectsWrongIssuer(t *testing.T) {
	stub := newIDPStub(t)
	client, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatal(err)
	}
	stub.setNonce("n")
	token := stub.sign(map[string]any{
		"iss": "https://evil.example.com", "sub": "user-42", "aud": "client-a",
		"exp": time.Now().Add(time.Minute).Unix(), "nonce": "n",
	})
	if _, err := client.verifyIDToken(t.Context(), token, "n"); err == nil {
		t.Error("want wrong issuer error, got nil")
	}
}

func TestOIDCVerifyRejectsTamperedSignature(t *testing.T) {
	stub := newIDPStub(t)
	client, err := newOIDCClient(testOIDCConfig(stub.issuer()), nil)
	if err != nil {
		t.Fatal(err)
	}
	stub.setNonce("n")
	token := stub.sign(map[string]any{
		"iss": stub.issuer(), "sub": "user-42", "aud": "client-a",
		"exp": time.Now().Add(time.Minute).Unix(), "nonce": "n",
	})
	parts := strings.Split(token, ".")
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte("forged"))
	if _, err := client.verifyIDToken(t.Context(), strings.Join(parts, "."), "n"); err == nil {
		t.Error("want signature error, got nil")
	}
}

func TestOIDCRequiresIssuer(t *testing.T) {
	if _, err := newOIDCClient(OIDCConfig{Issuer: ""}, nil); err == nil {
		t.Error("want error for empty issuer")
	}
}

func TestSSOUserIDStableAndScoped(t *testing.T) {
	a := ssoUserID("https://idp.example.com", "sub-1")
	if a != ssoUserID("https://idp.example.com", "sub-1") {
		t.Error("ssoUserID not deterministic")
	}
	if len(a) != len("sso-")+20 {
		t.Errorf("unexpected length: %q", a)
	}
	if a == ssoUserID("https://other.example.com", "sub-1") {
		t.Error("same sub on different issuer must differ")
	}
	if a == ssoUserID("https://idp.example.com", "sub-2") {
		t.Error("different subs must differ")
	}
}

func TestSSOStateStoreIsSingleUse(t *testing.T) {
	st := newSSOStateStore(time.Minute)
	st.put("s1", ssoEntry{Nonce: "n", Verifier: "v"})
	if _, ok := st.take("s1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := st.take("s1"); ok {
		t.Error("second take should fail (single-use)")
	}
}

func TestSSOStateStoreExpires(t *testing.T) {
	st := newSSOStateStore(time.Millisecond)
	st.put("s1", ssoEntry{Nonce: "n", Verifier: "v", Created: time.Now().Add(-time.Hour)})
	if _, ok := st.take("s1"); ok {
		t.Error("expired entry should not be taken")
	}
}
