package frontend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（go.mod 已有）
)

// ErrNotFound 表示凭据/会话不存在。
var ErrNotFound = errors.New("frontend: not found")

// ErrConflict 表示用户名已存在。
var ErrConflict = errors.New("frontend: username exists")

// Credential 是前台自持的登录凭据（与后台 users 表按 username 对齐，不存后台）。
type Credential struct {
	Username     string
	PasswordHash string
	TenantID     string
	DisplayName  string
	CreatedAt    string
	DisabledAt   string // 空 = 未封禁
}

// Store 是前台自有库：只存登录凭据与会话活动日志，不存任何记忆正文。
// 记忆正文一律在后台，前台只调后台 REST API。
type Store struct {
	db *sql.DB
}

// 前台自有 DDL。只含 CREATE（红线：不动既有 SQL，这里也是全新独立库）。
const frontendDDL = `
CREATE TABLE IF NOT EXISTS credentials (
    username      TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    tenant_id     TEXT NOT NULL,
    display_name  TEXT,
    created_at    TEXT NOT NULL,
    disabled_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_cred_tenant ON credentials (tenant_id);

CREATE TABLE IF NOT EXISTS session_log (
    id           TEXT PRIMARY KEY,
    username     TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT,
    ip           TEXT,
    user_agent   TEXT,
    revoked_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_session_user ON session_log (username);
`

// OpenStore 打开（必要时创建）前台自有库并建表。
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("frontend open store: %w", err)
	}
	// SQLite 单写者，前台并发较低，限制连接数避免 "database is locked"。
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, frontendDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("frontend ensure tables: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// CreateCredential 注册一个用户凭据。用户名已存在返回 ErrConflict。
func (s *Store) CreateCredential(ctx context.Context, c Credential) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (username, password_hash, tenant_id, display_name, created_at, disabled_at)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		c.Username, c.PasswordHash, c.TenantID, c.DisplayName, nowRFC3339())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create credential: %w", err)
	}
	return nil
}

// GetCredential 按用户名取凭据。不存在返回 ErrNotFound。
func (s *Store) GetCredential(ctx context.Context, username string) (Credential, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT username, password_hash, tenant_id, COALESCE(display_name,''), created_at, COALESCE(disabled_at,'')
		 FROM credentials WHERE username = ?`, username)
	var c Credential
	err := row.Scan(&c.Username, &c.PasswordHash, &c.TenantID, &c.DisplayName, &c.CreatedAt, &c.DisabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("get credential: %w", err)
	}
	return c, nil
}

// RecordLogin 记录一次登录活动（审计/展示用，不存 token）。
func (s *Store) RecordLogin(ctx context.Context, username, ip, ua string) error {
	id := newID()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_log (id, username, created_at, ip, user_agent) VALUES (?, ?, ?, ?, ?)`,
		id, username, nowRFC3339(), ip, ua)
	if err != nil {
		return fmt.Errorf("record login: %w", err)
	}
	return nil
}

// MarkSessionSeen 更新会话最后活跃时间。
func (s *Store) MarkSessionSeen(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session_log SET last_seen_at = ? WHERE username = ? AND revoked_at IS NULL`,
		nowRFC3339(), username)
	if err != nil {
		return fmt.Errorf("mark seen: %w", err)
	}
	return nil
}

// isUniqueViolation 判定 SQLite 唯一约束冲突（"UNIQUE constraint failed"）。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}