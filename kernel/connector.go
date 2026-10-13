// 外部连接器（P5 完整实现）
// 真源：lightmem.md §5.4 存储后端可插拔、§4.6 外部连接器
// 光明语言：用结构化配置表达同步策略
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Connector 外部连接器接口
type Connector interface {
	Name() string
	Sync(ctx context.Context, cfg json.RawMessage) error
	Health(ctx context.Context) error
	Close() error
}

// ConnectorRegistry 连接器注册表
type ConnectorRegistry struct {
	connectors map[string]Connector
}

func NewConnectorRegistry() *ConnectorRegistry {
	return &ConnectorRegistry{connectors: map[string]Connector{}}
}

func (r *ConnectorRegistry) Register(c Connector) {
	if c != nil {
		r.connectors[c.Name()] = c
	}
}

func (r *ConnectorRegistry) Get(name string) Connector {
	return r.connectors[name]
}

func (r *ConnectorRegistry) All() []Connector {
	var out []Connector
	for _, c := range r.connectors {
		out = append(out, c)
	}
	return out
}

// DBConnector 数据库连接器（完整实现）
type DBConnector struct {
	name    string
	db      *sql.DB
	cfg     *DBConfig
	backend StorageBackend
}

type DBConfig struct {
	Driver     string `json:"driver"`
	DSN        string `json:"dsn"`
	Table      string `json:"table"`
	KeyCol     string `json:"key_col"`
	ContentCol string `json:"content_col"`
	UpdatedCol string `json:"updated_col"`
	PollSec    int    `json:"poll_sec"`
	TenantID   string `json:"tenant_id"`
	UserID     string `json:"user_id"`
}

func NewDBConnector(name string, backend StorageBackend) *DBConnector {
	return &DBConnector{
		name:    name,
		backend: backend,
		cfg:     &DBConfig{PollSec: 30, TenantID: "connector", UserID: "system"},
	}
}

func (c *DBConnector) Name() string { return c.name }

func (c *DBConnector) Health(ctx context.Context) error {
	if c.db == nil {
		return fmt.Errorf("db not connected")
	}
	return c.db.PingContext(ctx)
}

func (c *DBConnector) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// Sync 增量同步（WAL/时间戳）
func (c *DBConnector) Sync(ctx context.Context, cfg json.RawMessage) error {
	if err := json.Unmarshal(cfg, c.cfg); err != nil {
		return err
	}
	db, err := sql.Open(c.cfg.Driver, c.cfg.DSN)
	if err != nil {
		return err
	}
	c.db = db

	lastSync := time.Now().UTC().Add(-time.Hour)
	ticker := time.NewTicker(time.Duration(c.cfg.PollSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			rows, err := c.db.QueryContext(ctx,
				fmt.Sprintf("SELECT %s, %s, %s FROM %s WHERE %s > $1 ORDER BY %s ASC",
					c.cfg.KeyCol, c.cfg.ContentCol, c.cfg.UpdatedCol, c.cfg.Table, c.cfg.UpdatedCol, c.cfg.UpdatedCol),
				lastSync)
			if err != nil {
				log.Printf("[connector] %s query: %v", c.name, err)
				continue
			}
			count := 0
			for rows.Next() {
				var key, content string
				var updated time.Time
				if err := rows.Scan(&key, &content, &updated); err != nil {
					continue
				}
				m := &Memory{
					MemoryID:       newMemoryID(),
					IdempotencyKey: c.name + ":" + key,
					TenantID:       c.cfg.TenantID,
					UserID:         c.cfg.UserID,
					MemoryKind:     KindFact,
					Fact:           content,
					Source:         c.name,
					RecordedAt:     time.Now().UTC(),
					Confidence:     0.7,
					Version:        1,
					Layer:          LayerL1,
				}
				if err := c.backend.Upsert(ctx, m); err != nil {
					log.Printf("[connector] %s upsert %s: %v", c.name, key, err)
					continue
				}
				lastSync = updated
				count++
			}
			rows.Close()
			if count > 0 {
				log.Printf("[connector] %s synced %d rows", c.name, count)
			}
		}
	}
}

// DocConnector 文档连接器（P5 完整实现）
type DocConnector struct {
	name    string
	cfg     *DocConfig
	backend StorageBackend
}

type DocConfig struct {
	Path       string   `json:"path"`
	Extensions []string `json:"extensions"`
	PollSec    int      `json:"poll_sec"`
	TenantID   string   `json:"tenant_id"`
	UserID     string   `json:"user_id"`
}

func NewDocConnector(name string, backend StorageBackend) *DocConnector {
	return &DocConnector{
		name:    name,
		backend: backend,
		cfg:     &DocConfig{PollSec: 60, TenantID: "connector", UserID: "system"},
	}
}

func (c *DocConnector) Name() string { return c.name }

func (c *DocConnector) Health(ctx context.Context) error {
	if c.cfg.Path == "" {
		return fmt.Errorf("path not configured")
	}
	_, err := os.Stat(c.cfg.Path)
	return err
}

func (c *DocConnector) Close() error { return nil }

func (c *DocConnector) Sync(ctx context.Context, cfg json.RawMessage) error {
	if err := json.Unmarshal(cfg, c.cfg); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Duration(c.cfg.PollSec) * time.Second)
	defer ticker.Stop()
	seen := map[string]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			files, err := filepath.Glob(filepath.Join(c.cfg.Path, "*"))
			if err != nil {
				log.Printf("[doc] %s glob: %v", c.name, err)
				continue
			}
			count := 0
			for _, f := range files {
				ext := filepath.Ext(f)
				if len(ext) > 0 {
					ext = ext[1:]
				}
				extOk := false
				for _, e := range c.cfg.Extensions {
					if e == ext {
						extOk = true
						break
					}
				}
				if !extOk {
					continue
				}
				info, err := os.Stat(f)
				if err != nil {
					continue
				}
				mod := info.ModTime()
				if seen[f].After(mod) || seen[f] == mod {
					continue
				}
				data, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				m := &Memory{
					MemoryID:       newMemoryID(),
					IdempotencyKey: c.name + ":" + filepath.Base(f),
					TenantID:       c.cfg.TenantID,
					UserID:         c.cfg.UserID,
					MemoryKind:     KindFact,
					Fact:           string(data),
					Source:         c.name,
					RecordedAt:     time.Now().UTC(),
					Confidence:     0.6,
					Version:        1,
					Layer:          LayerL1,
				}
				if err := c.backend.Upsert(ctx, m); err != nil {
					log.Printf("[doc] %s upsert %s: %v", c.name, f, err)
					continue
				}
				seen[f] = mod
				count++
			}
			if count > 0 {
				log.Printf("[doc] %s synced %d files", c.name, count)
			}
		}
	}
}

// RefSource 引用式记忆来源
type RefSource struct {
	Connector string `json:"connector"`
	Key       string `json:"key"`
	URL       string `json:"url,omitempty"`
}

// ResolveRef 解析引用式记忆
func ResolveRef(ctx context.Context, backend StorageBackend, ref *RefSource) (*Memory, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil ref")
	}
	// 按 idempotency_key 查找
	lk, ok := backend.(IdempotencyLookup)
	if !ok {
		return nil, fmt.Errorf("backend does not support idempotency lookup")
	}
	return lk.FindByIdempotencyKey(ctx, "", "", ref.Connector+":"+ref.Key)
}
