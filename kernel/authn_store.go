// R22 token 存储（docs/token-management-design.md §5）
//
// 内部认证器的 DB 路径：4 张表（users / agent_tokens / auth_tokens / auth_audit），
// token 只存 SHA-256 哈希 + 前 8 位 hint。solo 档落 SQLite，family/enterprise 落 PG，
// 同一套 DDL 双写（沿用 backend_sqlite.go / backend_pg.go 的双后端模式）。
//
// 红线：DDL 只含 CREATE TABLE / CREATE INDEX，绝不含 ALTER/DROP/TRUNCATE（R14 红线）。
// 零第三方依赖（红线⑤）：仅 context / database/sql / fmt / log / strings / time。
package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authDDL 4 张表的幂等建表语句。只 CREATE，绝不 ALTER 既有表。
// 时间列用 TEXT + RFC3339 存储，SQLite / PostgreSQL 通用（不依赖方言）。
const authDDL = `
-- users：人（含未来前台服务注册的用户）
CREATE TABLE IF NOT EXISTS users (
    id           TEXT PRIMARY KEY,
    username     TEXT UNIQUE NOT NULL,
    email        TEXT,
    tenant_id    TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'user',
    external_id  TEXT,
    idp          TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    disabled_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_users_tenant ON users (tenant_id);
CREATE INDEX IF NOT EXISTS idx_users_external ON users (idp, external_id);

-- agent_tokens：Agent（AI）的身份 token
CREATE TABLE IF NOT EXISTS agent_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT UNIQUE NOT NULL,
    token_hint   TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    user_id      TEXT,
    tenant_id    TEXT NOT NULL,
    scope        TEXT,
    expires_at   TEXT,
    revoked_at   TEXT,
    created_by   TEXT,
    created_at   TEXT NOT NULL,
    last_used_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_at_tenant ON agent_tokens (tenant_id);
CREATE INDEX IF NOT EXISTS idx_at_agent ON agent_tokens (agent_id);
CREATE INDEX IF NOT EXISTS idx_at_active ON agent_tokens (tenant_id, revoked_at, expires_at);

-- auth_tokens：人（前台服务代签）的 session token
CREATE TABLE IF NOT EXISTS auth_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT UNIQUE NOT NULL,
    token_hint   TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    roles        TEXT NOT NULL DEFAULT 'user',
    scope        TEXT,
    expires_at   TEXT,
    revoked_at   TEXT,
    created_by   TEXT,
    created_at   TEXT NOT NULL,
    last_used_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_au_user ON auth_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_au_active ON auth_tokens (user_id, revoked_at, expires_at);

-- auth_audit：鉴权与 token 管理审计
CREATE TABLE IF NOT EXISTS auth_audit (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT,
    actor_id     TEXT,
    action       TEXT NOT NULL,
    target_id    TEXT,
    detail       TEXT,
    ip           TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_tenant ON auth_audit (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON auth_audit (actor_id, created_at);
`

// EnsureAuthTables 幂等建 auth 相关表。DDL 只含 CREATE（红线校验）。
// SQLite / PostgreSQL 通用（driver 由调用方注入）。
func EnsureAuthTables(ctx context.Context, db *sql.DB) error {
	upper := strings.ToUpper(authDDL)
	for _, banned := range []string{"ALTER TABLE", "ALTER INDEX", "DROP TABLE", "DROP INDEX", "TRUNCATE"} {
		if strings.Contains(upper, banned) {
			return fmt.Errorf("auth DDL 违反红线：含 %s（只允许 CREATE TABLE/INDEX）", banned)
		}
	}
	for _, required := range []string{"USERS", "AGENT_TOKENS", "AUTH_TOKENS", "AUTH_AUDIT"} {
		if !strings.Contains(upper, "CREATE TABLE IF NOT EXISTS "+required) {
			return fmt.Errorf("auth DDL 缺少表 %s", required)
		}
	}
	if _, err := db.ExecContext(ctx, authDDL); err != nil {
		return fmt.Errorf("ensure auth tables: %w", err)
	}
	return nil
}

// AuthDBConfig 描述内部 DB 认证器需要的最小连接信息。
// 注：kernel 包已有 DBConfig（connector.go，外部数据连接器），此处避免重名。
type AuthDBConfig struct {
	DB     *sql.DB // 已打开的 database/sql 连接（sqlite 或 pg）
	Driver string  // "sqlite" | "pg"，用于日志
}

// dbAuthenticator 内部认证器的 DB 路径实现。
// 校验流：token → SHA-256 → 查 agent_tokens / auth_tokens → 检查 revoked_at / expires_at
// → 组 Identity。每次查库（配 60s 缓存即"吊销不重启生效"，见设计文档 §6）。
type dbAuthenticator struct {
	db     *sql.DB
	driver string
}

// NewDBAuthenticator 构造内部 DB 认证器。
func NewDBAuthenticator(db *sql.DB, driver string) Authenticator {
	return &dbAuthenticator{db: db, driver: driver}
}

func (a *dbAuthenticator) Name() string { return "internal" }

func (a *dbAuthenticator) Authenticate(ctx context.Context, raw string) (Identity, error) {
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}
	h := tokenHash(raw)

	// 先查 agent token
	if id, found, err := a.lookupAgent(ctx, h); err != nil {
		return Identity{}, err
	} else if found {
		return id, nil
	}

	// 再查人 session token
	if id, found, err := a.lookupUser(ctx, h); err != nil {
		return Identity{}, err
	} else if found {
		return id, nil
	}

	return Identity{}, ErrUnauthenticated
}

func (a *dbAuthenticator) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	if r == nil {
		return Identity{}, ErrUnauthenticated
	}
	raw := strings.TrimSpace(bearerToken(r))
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}
	return a.Authenticate(ctx, raw)
}

func (a *dbAuthenticator) lookupAgent(ctx context.Context, hash string) (Identity, bool, error) {
	var (
		id, hint, agentID, userID, tenantID, scope, exp, rev, created string
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT id, token_hint, agent_id, COALESCE(user_id,''), tenant_id,
		        COALESCE(scope,''), COALESCE(expires_at,''), COALESCE(revoked_at,''),
		        COALESCE(created_at,'')
		   FROM agent_tokens WHERE token_hash = ?`, hash,
	).Scan(&id, &hint, &agentID, &userID, &tenantID, &scope, &exp, &rev, &created)
	if err == sql.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("lookup agent token: %w", err)
	}
	if rev != "" {
		return Identity{}, false, nil // 已吊销
	}
	if exp != "" && expired(exp) {
		return Identity{}, false, nil // 已过期
	}
	_ = hint
	_ = created
	// touch last_used_at（尽力而为，不因失败阻断鉴权）
	_, _ = a.db.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE id = ?`,
		formatTime(timeNow()), id)
	return Identity{
		Kind:      IdentityKindAgent,
		AgentID:   agentID,
		UserID:    userID,
		TenantID:  tenantID,
		ID:        agentID,
		TokenID:   id,
		Issuer:    "internal",
		Scope:     normalizeScope(scope),
		ExpiresAt: parseTime(exp),
	}, true, nil
}

func (a *dbAuthenticator) lookupUser(ctx context.Context, hash string) (Identity, bool, error) {
	var (
		id, hint, userID, tenantID, roles, scope, exp, rev string
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT id, token_hint, user_id, tenant_id, COALESCE(roles,''), COALESCE(scope,''),
		        COALESCE(expires_at,''), COALESCE(revoked_at,'')
		   FROM auth_tokens WHERE token_hash = ?`, hash,
	).Scan(&id, &hint, &userID, &tenantID, &roles, &scope, &exp, &rev)
	if err == sql.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("lookup user token: %w", err)
	}
	if rev != "" {
		return Identity{}, false, nil
	}
	if exp != "" && expired(exp) {
		return Identity{}, false, nil
	}
	_ = hint
	return Identity{
		Kind:      IdentityKindUser,
		UserID:    userID,
		TenantID:  tenantID,
		ID:        userID,
		Roles:     splitRoles(roles),
		TokenID:   id,
		Issuer:    "internal",
		Scope:     normalizeScope(scope),
		ExpiresAt: parseTime(exp),
	}, true, nil
}

// expired 判断 RFC3339 时间串是否已过期。
func expired(t string) bool {
	tt, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return false
	}
	return timeNow().After(tt)
}

// parseTime 解析时间串；失败返回零值。
func parseTime(t string) time.Time {
	if t == "" {
		return time.Time{}
	}
	tt, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return time.Time{}
	}
	return tt
}

// formatTime 格式化时间为 RFC3339 串。
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// splitRoles 逗号分隔的角色串 → slice。
func splitRoles(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
