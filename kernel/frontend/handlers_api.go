package frontend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// proxyBackend 以当前用户的 session token 调后台业务端点，并把响应透传给前台客户端。
// 后台未配置、用户无 token、后台返回非 2xx 时都如实透传状态码（不吞错误）。
//
// R21 收口 G4：后台仍要求显式 tenant_id（校验存在性），前台自动注入当前用户所属租户，
// 避免把租户当参数暴露给 UI。
func (s *Server) proxyBackend(w http.ResponseWriter, r *http.Request, method, path string, body map[string]any) {
	b := s.backendReady()
	if b == nil {
		proxy(w, http.StatusServiceUnavailable, []byte(`{"error":"backend not configured"}`))
		return
	}
	token := s.sessionToken(r)
	if token == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	tenant := s.viewUserTenant(r)
	if method == http.MethodGet {
		path = withQuery(path, url.Values{"tenant_id": {tenant}})
	} else if body == nil {
		body = map[string]any{"tenant_id": tenant}
	} else if body["tenant_id"] == nil || body["tenant_id"] == "" {
		body["tenant_id"] = tenant
	}

	ctx, cancel := s.bgctx(r)
	defer cancel()
	status, raw, err := b.CallUser(ctx, method, path, token, body)
	if err != nil {
		logErr("backend call "+method+" "+path, err)
		proxy(w, http.StatusBadGateway, []byte(`{"error":"backend unreachable"}`))
		return
	}
	if status == http.StatusUnauthorized {
		// 后台 token 已失效（吊销/过期）：清会话回登录。
		s.clearSession(w, r)
	}
	if raw == nil {
		raw = []byte{}
	}
	proxy(w, status, raw)
}

// viewUserTenant 返回当前用户所属租户（cookie 还原，兜底默认租户）。
func (s *Server) viewUserTenant(r *http.Request) string {
	if u := s.viewUser(r); u != nil && u.TenantID != "" {
		return u.TenantID
	}
	return s.cfg.DefaultTenant
}

// withQuery 给 path 追加 query。
func withQuery(path string, q url.Values) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// readJSONBody 解析请求 JSON body；空 body 返回 nil（不报错）。
func readJSONBody(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- API handlers ----

func (s *Server) handleAPIRecall(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.ContentLength > 0 {
		_ = readJSONBody(r, &body)
	}
	s.proxyBackend(w, r, http.MethodPost, "/tools/memory/recall", body)
}

func (s *Server) handleAPISave(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	_ = readJSONBody(r, &body)
	s.injectOwner(r, body)
	s.proxyBackend(w, r, http.MethodPost, "/tools/memory/save", body)
}

// injectOwner 确保写路径带 permission.owner=登录用户（后台 writepolicy 强制）。
func (s *Server) injectOwner(r *http.Request, body map[string]any) {
	u := s.viewUser(r)
	if u == nil || u.Username == "" {
		return
	}
	pm, _ := body["permission"].(map[string]any)
	if pm == nil {
		pm = map[string]any{}
		body["permission"] = pm
	}
	if pm["owner"] == nil || pm["owner"] == "" {
		pm["owner"] = u.Username
	}
}

func (s *Server) handleAPIGoals(w http.ResponseWriter, r *http.Request) {
	s.proxyBackend(w, r, http.MethodGet, "/tools/memory/plan", nil)
}

func (s *Server) handleAPIGoalCreate(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	_ = readJSONBody(r, &body)
	s.injectOwner(r, body)
	// 后台 handleGoalCreate 用 body 的 user_id 当 owner/actor（不走 ResolveActor），须补登录用户。
	if body["user_id"] == nil || body["user_id"] == "" {
		if u := s.viewUser(r); u != nil && u.Username != "" {
			body["user_id"] = u.Username
		}
	}
	s.proxyBackend(w, r, http.MethodPost, "/tools/memory/goal", body)
}

func (s *Server) handleAPIGoalProgress(w http.ResponseWriter, r *http.Request) {
	// 后台 progress 用 body；路由携带的 id 仅用于展示。
	id := r.PathValue("id")
	body := map[string]any{"memory_id": id}
	_ = readJSONBody(r, &body)
	if _, ok := body["memory_id"]; !ok || body["memory_id"] == "" {
		body["memory_id"] = id
	}
	s.proxyBackend(w, r, http.MethodPost, "/tools/memory/goal/progress", body)
}

func (s *Server) handleAPIGoalState(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"memory_id": r.PathValue("id")}
	_ = readJSONBody(r, &body)
	if _, ok := body["memory_id"]; !ok || body["memory_id"] == "" {
		body["memory_id"] = r.PathValue("id")
	}
	s.proxyBackend(w, r, http.MethodPost, "/tools/memory/goal/state", body)
}

// handleAPIDiscover 社区公开浏览：调后台 GET /search，以当前用户为 viewer。
// R23.2 后台 canView 已放行 public（跨租户读），viewer 非空是读 public 的前提。
func (s *Server) handleAPIDiscover(w http.ResponseWriter, r *http.Request) {
	viewer := ""
	if u := s.viewUser(r); u != nil {
		viewer = u.Username
	}
	path := withQuery("/search", url.Values{
		"q":      {r.URL.Query().Get("q")},
		"viewer": {viewer},
	})
	s.proxyBackend(w, r, http.MethodGet, path, nil)
}
