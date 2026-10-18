package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BackendConfig 是后台对接配置。
type BackendConfig struct {
	BaseURL      string        // 后台业务端点，如 http://127.0.0.1:18444
	AdminURL     string        // 后台管理 API，如 http://127.0.0.1:18090
	ServiceToken string        // 前台自身 token，调 /admin/tokens/session 用
	Timeout      time.Duration // 单次请求超时；0 用默认 10s
}

type backendClient struct {
	cfg  BackendConfig
	http *http.Client
}

// NewBackendClient 构造后台客户端。配置为空时 IsConfigured() 返回 false。
func NewBackendClient(cfg BackendConfig) *backendClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &backendClient{cfg: cfg, http: &http.Client{Timeout: timeout}}
}

// IsConfigured 判断是否具备调后台的最低配置（base URL + service token）。
func (b *backendClient) IsConfigured() bool {
	return b != nil && b.cfg.BaseURL != "" && b.cfg.AdminURL != "" && b.cfg.ServiceToken != ""
}

// Health 探测后台主服务是否可达。
func (b *backendClient) Health(ctx context.Context) error {
	if !b.IsConfigured() {
		return fmt.Errorf("backend not configured")
	}
	st, _, err := b.do(ctx, http.MethodGet, b.cfg.BaseURL+"/health", b.cfg.ServiceToken, nil)
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return fmt.Errorf("backend health = %d", st)
	}
	return nil
}

// IssueSession 以服务身份调后台管理 API，为 userID 签发一条 session token（auth_tokens）。
// 返回 token 明文与 token 记录 id（登出 revoke 用）。roles/scope 传空时后台按其缺省处理。
func (b *backendClient) IssueSession(ctx context.Context, userID, tenantID, roles, scope string) (token, id string, err error) {
	body := map[string]string{"user_id": userID, "tenant_id": tenantID, "roles": roles, "scope": scope}
	st, raw, err := b.do(ctx, http.MethodPost, b.cfg.AdminURL+"/admin/tokens/session", b.cfg.ServiceToken, body)
	if err != nil {
		return "", "", fmt.Errorf("issue session: %w", err)
	}
	if st != http.StatusCreated && st != http.StatusOK {
		return "", "", fmt.Errorf("issue session: backend %d: %s", st, truncate(string(raw), 160))
	}
	var resp struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("issue session: parse: %w", err)
	}
	if resp.Token == "" {
		return "", "", fmt.Errorf("issue session: empty token")
	}
	return resp.Token, resp.ID, nil
}

// RevokeSession 调后台管理 API 吊销一条 session token（登出）。
func (b *backendClient) RevokeSession(ctx context.Context, tokenID string) error {
	st, raw, err := b.do(ctx, http.MethodPost, b.cfg.AdminURL+"/admin/tokens/session/"+tokenID+"/revoke", b.cfg.ServiceToken, nil)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if st != http.StatusOK {
		return fmt.Errorf("revoke session: backend %d: %s", st, truncate(string(raw), 120))
	}
	return nil
}

// CallUser 以某个已登录用户的 session token 调后台业务端点。
// 返回 HTTP 状态码、响应体（可为 nil）与网络错误。
func (b *backendClient) CallUser(ctx context.Context, method, path, userToken string, body any) (int, []byte, error) {
	if userToken == "" {
		return http.StatusUnauthorized, nil, fmt.Errorf("missing user token")
	}
	return b.do(ctx, method, b.cfg.BaseURL+path, userToken, body)
}

// do 发送一次带 Bearer 的 JSON 请求。
func (b *backendClient) do(ctx context.Context, method, url, token string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}