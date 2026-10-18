// R22.2 管理 API（docs/token-management-design.md §6）
//
// 目标：给正式版补上"可管理"的能力——用户 / token 的签发·吊销·轮换·审计，
// 全部**不重启即生效**（每次查库，无需重启服务，修 R21 的"改 token 必重启"缺陷）。
//
// 设计口径（R22 定稿）：
//   - 管理员自己也当 user，登录后拿 auth_tokens；管理操作校验 roles 含 admin（决策 #1）；
//   - 明文 token 只在签发那一刻返回一次，DB 只存 SHA-256 哈希（设计文档 §9）；
//   - 接线默认关：APIServer 未注入 DB 时 /admin 路由不注册（红线④零回归）；
//   - 零第三方依赖（红线⑤）：仅 stdlib（crypto/rand 生成 token，database/sql 查库）。
package kernel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// AdminStore 封装 token / 用户管理。持有 DB 连接；nil 时管理 API 不注册。
type AdminStore struct {
	db       *sql.DB
	loopback bool // 默认 true：仅本机回环可调管理 API
}

// NewAdminStore 构造 AdminStore。默认 loopback=true（仅回环可调）。
func NewAdminStore(db *sql.DB) *AdminStore {
	return &AdminStore{db: db, loopback: true}
}

// SetLoopbackOnly 开关"仅回环可调"（false 允许跨机，须配合 TLS + 强鉴权）。
func (a *AdminStore) SetLoopbackOnly(v bool) { a.loopback = v }

// newRandomToken 生成形如 `t-agent-<32位hex>` 的随机 token。
// 用 crypto/rand（不依赖 math/rand，避免可预测）。
func newRandomToken(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b), nil
}

// adminError 统一管理 API 错误响应。
func adminError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// 管理 API 路由
// ---------------------------------------------------------------------------

// RegisterAdminRoutes 把 /admin/* 端点挂到 mux。仅在 APIServer 注入了 DB 时调用。
// 每条路由先过 adminGuard（Bearer + admin role），缺/错/无权 → 401/403。
func RegisterAdminRoutes(mux *http.ServeMux, a *AdminStore) {
	mux.HandleFunc("POST /admin/users", a.guard(a.handleUserCreate))
	mux.HandleFunc("GET /admin/users", a.guard(a.handleUserList))
	mux.HandleFunc("PATCH /admin/users/{id}", a.guard(a.handleUserPatch))
	mux.HandleFunc("POST /admin/tokens/agent", a.guard(a.handleAgentTokenIssue))
	mux.HandleFunc("GET /admin/tokens/agent", a.guard(a.handleAgentTokenList))
	mux.HandleFunc("POST /admin/tokens/agent/{id}/revoke", a.guard(a.handleAgentTokenRevoke))
	mux.HandleFunc("POST /admin/tokens/agent/{id}/rotate", a.guard(a.handleAgentTokenRotate))
	mux.HandleFunc("POST /admin/tokens/session", a.guard(a.handleSessionTokenIssue))
	mux.HandleFunc("POST /admin/tokens/session/{id}/revoke", a.guard(a.handleSessionTokenRevoke))
	mux.HandleFunc("GET /admin/audit", a.guard(a.handleAuditList))
}

// guard 是管理 API 的统一鉴权中间件：回环检查 + Bearer token + admin role。
func (a *AdminStore) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 默认仅回环可调（防暴露到公网）
		if a.loopback && !a.isLoopback(r.RemoteAddr) {
			adminError(w, http.StatusForbidden, "admin api restricted to loopback")
			return
		}
		raw := bearerToken(r)
		if raw == "" {
			adminError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id, err := a.authenticateAdmin(r.Context(), raw)
		if err != nil {
			adminError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !id.HasRole("admin") {
			adminError(w, http.StatusForbidden, "forbidden: admin role required")
			return
		}
		// 审计记录（尽力而为）
		a.writeAudit(r.Context(), id, "admin_call", r.URL.Path, "")
		next(w, r.WithContext(withIdentity(r.Context(), id)))
	}
}

func (a *AdminStore) isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authenticateAdmin 用 Bearer token 换身份，仅接受 auth_tokens 表里的 user session。
func (a *AdminStore) authenticateAdmin(ctx context.Context, raw string) (Identity, error) {
	h := tokenHash(raw)
	var id, hint, userID, tenantID, roles, exp, rev string
	err := a.db.QueryRowContext(ctx,
		`SELECT id, token_hint, user_id, tenant_id, COALESCE(roles,''),
		        COALESCE(expires_at,''), COALESCE(revoked_at,'')
		   FROM auth_tokens WHERE token_hash = ?`, h,
	).Scan(&id, &hint, &userID, &tenantID, &roles, &exp, &rev)
	if err == sql.ErrNoRows {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	if rev != "" || (exp != "" && expired(exp)) {
		return Identity{}, ErrUnauthenticated
	}
	_ = hint
	return Identity{
		Kind:     IdentityKindUser,
		UserID:   userID,
		TenantID: tenantID,
		ID:       userID,
		Roles:    splitRoles(roles),
		TokenID:  id,
		Issuer:   "internal",
	}, nil
}

// ---------------------------------------------------------------------------
// 用户管理
// ---------------------------------------------------------------------------

type userCreateReq struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

func (a *AdminStore) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req userCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.TenantID) == "" {
		adminError(w, http.StatusBadRequest, "username and tenant_id required")
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if !validRole(req.Role) {
		adminError(w, http.StatusBadRequest, "role must be admin|user|auditor")
		return
	}
	id, err := newRandomToken("u")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := formatTime(timeNow())
	if _, err := a.db.Exec(`INSERT INTO users
		(id, username, email, tenant_id, role, idp, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'internal', ?, ?)`,
		id, strings.TrimSpace(req.Username), req.Email, strings.TrimSpace(req.TenantID),
		req.Role, now, now); err != nil {
		adminError(w, http.StatusConflict, "create user: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "username": req.Username, "role": req.Role})
}

func (a *AdminStore) handleUserList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, username, COALESCE(email,''), tenant_id, role,
		        COALESCE(disabled_at,''), created_at FROM users ORDER BY created_at`)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "list users: "+err.Error())
		return
	}
	defer rows.Close()
	type u struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		Email      string `json:"email"`
		TenantID   string `json:"tenant_id"`
		Role       string `json:"role"`
		Disabled   bool   `json:"disabled"`
		CreatedAt  string `json:"created_at"`
	}
	out := []u{}
	for rows.Next() {
		var x u
		var dis string
		if err := rows.Scan(&x.ID, &x.Username, &x.Email, &x.TenantID, &x.Role, &dis, &x.CreatedAt); err != nil {
			adminError(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		x.Disabled = dis != ""
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "count": len(out)})
}

type userPatchReq struct {
	Role       *string `json:"role"`
	Disabled   *bool   `json:"disabled"`
	Email      *string `json:"email"`
}

func (a *AdminStore) handleUserPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		adminError(w, http.StatusBadRequest, "id required")
		return
	}
	var req userPatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	now := formatTime(timeNow())
	if req.Role != nil {
		if !validRole(*req.Role) {
			adminError(w, http.StatusBadRequest, "role must be admin|user|auditor")
			return
		}
		if _, err := a.db.Exec(`UPDATE users SET role=?, updated_at=? WHERE id=?`, *req.Role, now, id); err != nil {
			adminError(w, http.StatusInternalServerError, "update role: "+err.Error())
			return
		}
	}
	if req.Disabled != nil {
		if *req.Disabled {
			if _, err := a.db.Exec(`UPDATE users SET disabled_at=?, updated_at=? WHERE id=?`, now, now, id); err != nil {
				adminError(w, http.StatusInternalServerError, "disable: "+err.Error())
				return
			}
		} else {
			if _, err := a.db.Exec(`UPDATE users SET disabled_at=NULL, updated_at=? WHERE id=?`, now, id); err != nil {
				adminError(w, http.StatusInternalServerError, "enable: "+err.Error())
				return
			}
		}
	}
	if req.Email != nil {
		if _, err := a.db.Exec(`UPDATE users SET email=?, updated_at=? WHERE id=?`, *req.Email, now, id); err != nil {
			adminError(w, http.StatusInternalServerError, "update email: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "updated": "true"})
}

func validRole(r string) bool {
	return r == "admin" || r == "user" || r == "auditor"
}

// ---------------------------------------------------------------------------
// Agent token 管理
// ---------------------------------------------------------------------------

type agentTokenIssueReq struct {
	AgentID          string `json:"agent_id"`
	TenantID         string `json:"tenant_id"`
	UserID           string `json:"user_id"`
	Scope            string `json:"scope"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

// agentTokenOut 是 agent token 的脱敏表示（不含 hash）。
type agentTokenOut struct {
	ID         string   `json:"id"`
	AgentID    string   `json:"agent_id"`
	TenantID   string   `json:"tenant_id"`
	UserID     string   `json:"user_id,omitempty"`
	Scope      []string `json:"scope,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Token      string   `json:"token,omitempty"` // 仅签发时返回一次
}

func (a *AdminStore) handleAgentTokenIssue(w http.ResponseWriter, r *http.Request) {
	var req agentTokenIssueReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.AgentID) == "" || strings.TrimSpace(req.TenantID) == "" {
		adminError(w, http.StatusBadRequest, "agent_id and tenant_id required")
		return
	}
	raw, err := newRandomToken("t-agent")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := newRandomToken("at")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := formatTime(timeNow())
	var exp string
	if req.ExpiresInSeconds > 0 {
		exp = formatTime(timeNow().Add(time.Duration(req.ExpiresInSeconds) * time.Second))
	}
	actor := actorOf(r.Context())
	if _, err := a.db.Exec(`INSERT INTO agent_tokens
		(id, token_hash, token_hint, agent_id, user_id, tenant_id, scope,
		 expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, tokenHash(raw), tokenHint(raw), strings.TrimSpace(req.AgentID), req.UserID,
		strings.TrimSpace(req.TenantID), req.Scope, nullIfEmpty(exp), actor, now); err != nil {
		adminError(w, http.StatusInternalServerError, "issue token: "+err.Error())
		return
	}
	out := agentTokenOut{
		ID: id, AgentID: req.AgentID, TenantID: req.TenantID, UserID: req.UserID,
		Scope: normalizeScope(req.Scope), ExpiresAt: exp, CreatedAt: now,
		Token: raw, // 明文仅此一次
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *AdminStore) handleAgentTokenList(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, token_hint, agent_id, COALESCE(user_id,''), tenant_id,
	        COALESCE(scope,''), COALESCE(expires_at,''), COALESCE(revoked_at,''),
	        created_at, COALESCE(last_used_at,'')
	       FROM agent_tokens`
	args := []any{}
	if t := r.URL.Query().Get("tenant"); t != "" {
		q += " WHERE tenant_id = ?"
		args = append(args, t)
	}
	if ag := r.URL.Query().Get("agent"); ag != "" {
		if strings.Contains(q, "WHERE") {
			q += " AND"
		} else {
			q += " WHERE"
		}
		q += " agent_id = ?"
		args = append(args, ag)
	}
	q += " ORDER BY created_at"
	rows, err := a.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	defer rows.Close()
	out := []agentTokenOut{}
	for rows.Next() {
		var x agentTokenOut
		var scope, exp, rev, created, last, hint string
		// 列序：id, token_hint, agent_id, user_id, tenant_id, scope, expires_at, revoked_at, created_at, last_used_at
		if err := rows.Scan(&x.ID, &hint, &x.AgentID, &x.UserID, &x.TenantID, &scope,
			&exp, &rev, &created, &last); err != nil {
			adminError(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		_ = hint // hint 仅内部识别用，不对外暴露
		x.Scope = normalizeScope(scope)
		x.ExpiresAt, x.RevokedAt, x.CreatedAt, x.LastUsedAt = exp, rev, created, last
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "count": len(out)})
}

func (a *AdminStore) handleAgentTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		adminError(w, http.StatusBadRequest, "id required")
		return
	}
	now := formatTime(timeNow())
	res, err := a.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "revoke: "+err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		adminError(w, http.StatusNotFound, "token not found or already revoked")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "revoked_at": now})
}

// rotateResult 是轮换的结果：旧 token 被吊销，新 token 返回一次。
type rotateResult struct {
	OldID string `json:"old_id"`
	Token string `json:"token"`
	ID    string `json:"id"`
}

func (a *AdminStore) handleAgentTokenRotate(w http.ResponseWriter, r *http.Request) {
	oldID := r.PathValue("id")
	if oldID == "" {
		adminError(w, http.StatusBadRequest, "id required")
		return
	}
	// 读取旧 token 的属性（复制一份新的）
	var agentID, userID, tenantID, scope string
	var expiresAt sql.NullString
	err := a.db.QueryRowContext(r.Context(),
		`SELECT agent_id, COALESCE(user_id,''), tenant_id, COALESCE(scope,''),
		        COALESCE(expires_at,'') FROM agent_tokens WHERE id=?`, oldID,
	).Scan(&agentID, &userID, &tenantID, &scope, &expiresAt)
	if err == sql.ErrNoRows {
		adminError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		adminError(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return
	}
	// 先吊销旧
	if _, err := a.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE id=?`, formatTime(timeNow()), oldID); err != nil {
		adminError(w, http.StatusInternalServerError, "revoke old: "+err.Error())
		return
	}
	// 签发新（继承旧属性）
	raw, err := newRandomToken("t-agent")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	newID, err := newRandomToken("at")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := formatTime(timeNow())
	exp := expiresAt.String
	if _, err := a.db.Exec(`INSERT INTO agent_tokens
		(id, token_hash, token_hint, agent_id, user_id, tenant_id, scope,
		 expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID, tokenHash(raw), tokenHint(raw), agentID, userID, tenantID, scope,
		nullIfEmpty(exp), actorOf(r.Context()), now); err != nil {
		adminError(w, http.StatusInternalServerError, "issue new: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rotateResult{OldID: oldID, ID: newID, Token: raw})
}

// ---------------------------------------------------------------------------
// Session token（人 / 前台服务代签）
// ---------------------------------------------------------------------------

type sessionTokenIssueReq struct {
	UserID           string `json:"user_id"`
	TenantID         string `json:"tenant_id"`
	Roles            string `json:"roles"`
	Scope            string `json:"scope"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

func (a *AdminStore) handleSessionTokenIssue(w http.ResponseWriter, r *http.Request) {
	var req sessionTokenIssueReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.TenantID) == "" {
		adminError(w, http.StatusBadRequest, "user_id and tenant_id required")
		return
	}
	if strings.TrimSpace(req.Roles) == "" {
		req.Roles = "user"
	}
	raw, err := newRandomToken("t-user")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := newRandomToken("au")
	if err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := formatTime(timeNow())
	var exp string
	if req.ExpiresInSeconds > 0 {
		exp = formatTime(timeNow().Add(time.Duration(req.ExpiresInSeconds) * time.Second))
	}
	if _, err := a.db.Exec(`INSERT INTO auth_tokens
		(id, token_hash, token_hint, user_id, tenant_id, roles, scope,
		 expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, tokenHash(raw), tokenHint(raw), strings.TrimSpace(req.UserID),
		strings.TrimSpace(req.TenantID), strings.TrimSpace(req.Roles), req.Scope,
		nullIfEmpty(exp), actorOf(r.Context()), now); err != nil {
		adminError(w, http.StatusInternalServerError, "issue session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "user_id": req.UserID, "tenant_id": req.TenantID,
		"roles": splitRoles(req.Roles), "token": raw, "expires_at": exp, "created_at": now,
	})
}

func (a *AdminStore) handleSessionTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		adminError(w, http.StatusBadRequest, "id required")
		return
	}
	now := formatTime(timeNow())
	res, err := a.db.Exec(`UPDATE auth_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "revoke: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		adminError(w, http.StatusNotFound, "token not found or already revoked")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "revoked_at": now})
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

func (a *AdminStore) handleAuditList(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, COALESCE(tenant_id,''), COALESCE(actor_id,''), action,
	        COALESCE(target_id,''), COALESCE(detail,''), COALESCE(ip,''), created_at
	       FROM auth_audit`
	args := []any{}
	var conds []string
	if t := r.URL.Query().Get("tenant"); t != "" {
		conds = append(conds, "tenant_id = ?")
		args = append(args, t)
	}
	if from := r.URL.Query().Get("from"); from != "" {
		conds = append(conds, "created_at >= ?")
		args = append(args, from)
	}
	if to := r.URL.Query().Get("to"); to != "" {
		conds = append(conds, "created_at <= ?")
		args = append(args, to)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT 200"
	rows, err := a.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "audit: "+err.Error())
		return
	}
	defer rows.Close()
	type rec struct {
		ID, TenantID, ActorID, Action, TargetID, Detail, IP, CreatedAt string
	}
	out := []map[string]string{}
	for rows.Next() {
		var x rec
		if err := rows.Scan(&x.ID, &x.TenantID, &x.ActorID, &x.Action, &x.TargetID,
			&x.Detail, &x.IP, &x.CreatedAt); err != nil {
			adminError(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, map[string]string{
			"id": x.ID, "tenant_id": x.TenantID, "actor_id": x.ActorID,
			"action": x.Action, "target_id": x.TargetID, "detail": x.Detail,
			"ip": x.IP, "created_at": x.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out, "count": len(out)})
}

// writeAudit 写一条审计（尽力而为，不阻断业务）。
func (a *AdminStore) writeAudit(ctx context.Context, actor Identity, action, detail, ip string) {
	id, err := newRandomToken("aa")
	if err != nil {
		return
	}
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO auth_audit (id, tenant_id, actor_id, action, detail, ip, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, actor.TenantID, actor.ID, action, detail, ip, formatTime(timeNow())); err != nil {
		// 审计失败不阻断；日志留给调用方
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// actorOf 从 context 取操作者标识（可能为空，如未认证路径）。
func actorOf(ctx context.Context) string {
	if id, ok := IdentityFromContext(ctx); ok {
		if id.ID != "" {
			return id.ID
		}
		if id.UserID != "" {
			return id.UserID
		}
		return id.AgentID
	}
	return ""
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
