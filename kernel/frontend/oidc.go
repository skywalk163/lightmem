package frontend

// R23.3 前台侧 OIDC 客户端（授权码 + PKCE，零第三方依赖）。
//
// 设计原则（对齐 R22.4 SSO 适配器"后台不验签，只认结果"）：本文件负责
// **发起登录 + 回调换码 + ID Token 验签**，验签成功后把 Identity 交给
// 登录流程调后台签 token（对接路径①）。所有密码学仅用 stdlib：
// crypto/rsa + crypto/sha256 + encoding/base64 + crypto/rand + net/http。
//
// 接线默认关：OIDC 配置为空时，调用方不注册 SSO 路由，登录页也不显示入口。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCConfig 描述一个 OIDC IdP。Issuer 必填；其余留空时使用最保守默认。
type OIDCConfig struct {
	Issuer       string   // IdP 的 issuer（发现文档 URL = Issuer + /.well-known/openid-configuration）
	ClientID     string   // 客户端 ID
	ClientSecret string   // 客户端密钥（confidential client）
	RedirectURI  string   // 回调地址，必须是本服务自己的 https/http URL
	Scopes       []string // 请求的 scope，默认 ["openid","profile","email"]
	ClockSkew    time.Duration
}

// SSOIdentity 是验签后的最终身份（由登录流程消费，映射为后台身份后签 token）。
type SSOIdentity struct {
	Subject    string // IdP 的 sub（稳定、唯一）
	Name       string
	Email      string
	Issuer     string
	RawIDToken string // 原始 ID Token，必要时透传给后台 SSO 适配器（路径②）
}

// oidcClient 是极简 OIDC 客户端实例。
type oidcClient struct {
	cfg   OIDCConfig
	httpc *http.Client

	mu      sync.Mutex
	meta    *oidcMeta
	jwks    map[string]*rsa.PublicKey // keyed by kid
	fetched time.Time
}

// oidcMeta 是 OpenID Connect Discovery 文档的子集。
type oidcMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// jwksDoc 是 JWKS 文档（只取 RSA keys）。
type jwksDoc struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// jwtHeader / idClaims 覆盖验签所需的最小字段集。
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type idClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	Expires   int64  `json:"exp"`
	NotBefore int64  `json:"nbf"`
	IssuedAt  int64  `json:"iat"`
	Nonce     string `json:"nonce"`
	Name      string `json:"name"`
	Email     string `json:"email"`
}

// tokenResp 是 token 端点成功响应（只取 ID Token 与必填项）。
type tokenResp struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	State       string `json:"state"`
}

// newOIDCClient 构造客户端并做一次发现（失败返回错误，让调用方可降级/告警）。
func newOIDCClient(cfg OIDCConfig, httpc *http.Client) (*oidcClient, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("oidc: issuer required")
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	c := &oidcClient{
		cfg:   cfg,
		httpc: httpc,
		jwks:  map[string]*rsa.PublicKey{},
	}
	if _, err := c.discover(context.Background()); err != nil {
		return nil, fmt.Errorf("oidc discover %s: %w", cfg.Issuer, err)
	}
	return c, nil
}

func (c *oidcClient) scopes() string {
	s := c.cfg.Scopes
	if len(s) == 0 {
		s = []string{"openid", "profile", "email"}
	}
	return strings.Join(s, " ")
}

func (c *oidcClient) discover(ctx context.Context) (*oidcMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.meta != nil && time.Since(c.fetched) < 15*time.Minute {
		return c.meta, nil
	}
	u := strings.TrimRight(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var m oidcMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("discover json: %w", err)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" {
		return nil, errors.New("oidc: discovery missing authorization_endpoint or token_endpoint")
	}
	c.meta = &m
	c.fetched = time.Now()
	return &m, nil
}

// authorizationURL 生成授权跳转 URL（state 用于 CSRF、nonce 用于防重放、PKCE 防止拦截换取）。
func (c *oidcClient) authorizationURL(state, nonce, challenge string) (string, error) {
	m, err := c.discover(context.Background())
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURI)
	q.Set("scope", c.scopes())
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// exchange 用授权码换取 token 响应（客户端密钥走 client_secret_basic）。
func (c *oidcClient) exchange(ctx context.Context, code, verifier string) (*tokenResp, error) {
	m, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURI)
	form.Set("client_id", c.cfg.ClientID)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.cfg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("token json: %w", err)
	}
	if tr.IDToken == "" {
		return nil, errors.New("oidc: id_token missing")
	}
	return &tr, nil
}

// verifyIDToken 验签 ID Token（RS256，JWKS），并校验 iss/aud/exp/nonce。
// 返回验签后的身份。
func (c *oidcClient) verifyIDToken(ctx context.Context, rawIDToken, nonce string) (*SSOIdentity, error) {
	if rawIDToken == "" {
		return nil, errors.New("oidc: empty id_token")
	}
	parts := strings.Split(rawIDToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: id_token must be a JWT with 3 segments")
	}
	hdrBytes, err := b64url(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidc header b64: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("oidc header json: %w", err)
	}
	if !strings.EqualFold(hdr.Alg, "RS256") {
		return nil, fmt.Errorf("oidc: unsupported alg %q (want RS256)", hdr.Alg)
	}
	sig, err := b64url(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidc sig b64: %w", err)
	}
	pub, err := c.resolveKey(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("oidc signature: %w", err)
	}
	payloadBytes, err := b64url(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidc payload b64: %w", err)
	}
	var cl idClaims
	if err := json.Unmarshal(payloadBytes, &cl); err != nil {
		return nil, fmt.Errorf("oidc payload json: %w", err)
	}
	if cl.Subject == "" {
		return nil, errors.New("oidc: sub missing")
	}
	// 校验非对称：iss 必须与配置 issuer 一致；aud 必须包含 client_id；exp 未过期；nonce 必须匹配。
	m, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	cfgIssuer := strings.TrimRight(c.cfg.Issuer, "/")
	claimedIssuer := strings.TrimRight(cl.Issuer, "/")
	if claimedIssuer != cfgIssuer && (m.Issuer == "" || claimedIssuer != strings.TrimRight(m.Issuer, "/")) {
		return nil, fmt.Errorf("oidc: issuer %q not trusted", cl.Issuer)
	}
	if !audienceContains(cl.Audience, c.cfg.ClientID) {
		return nil, fmt.Errorf("oidc: audience %q missing client_id %q", cl.Audience, c.cfg.ClientID)
	}
	skew := c.cfg.ClockSkew
	if skew == 0 {
		skew = 60 * time.Second
	}
	now := time.Now().Unix()
	if cl.Expires != 0 && now > cl.Expires+int64(skew.Seconds()) {
		return nil, errors.New("oidc: id_token expired")
	}
	if cl.NotBefore != 0 && now < cl.NotBefore-int64(skew.Seconds()) {
		return nil, errors.New("oidc: id_token not yet valid")
	}
	if nonce != "" && cl.Nonce != nonce {
		return nil, errors.New("oidc: nonce mismatch")
	}
	return &SSOIdentity{
		Subject:    cl.Subject,
		Name:       cl.Name,
		Email:      cl.Email,
		Issuer:     cl.Issuer,
		RawIDToken: rawIDToken,
	}, nil
}

// resolveKey 从 JWKS 取匹配 kid 的 RSA 公钥（带缓存）。
func (c *oidcClient) resolveKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	if k, ok := c.jwks[kid]; ok && time.Since(c.fetched) < 15*time.Minute {
		c.mu.Unlock()
		return k, nil
	}
	c.mu.Unlock()

	m, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks json: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Now()
	for _, k := range doc.Keys {
		if !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		nb, err := b64url(k.N)
		if err != nil {
			continue
		}
		eb, err := b64url(k.E)
		if err != nil {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		c.jwks[k.Kid] = pub
	}
	k, ok := c.jwks[kid]
	if !ok {
		return nil, fmt.Errorf("oidc: no JWKS key for kid %q", kid)
	}
	return k, nil
}

// ---- 极简辅助 ----

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func b64urlEnc(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func audienceContains(aud, clientID string) bool {
	for _, a := range strings.Split(aud, " ") {
		if strings.TrimSpace(a) == clientID {
			return true
		}
	}
	return aud == clientID
}

// newOIDCNonce 生成 16 字节随机 nonce（base64url）。
func newOIDCNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64urlEnc(b), nil
}

// newPKCE 生成 PKCE verifier + S256 challenge。
func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = b64urlEnc(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64urlEnc(sum[:]), nil
}
