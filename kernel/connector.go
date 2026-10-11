// 外部连接器（P5）
// 真源：lightmem.md §5.4 存储后端可插拔、§4.6 外部连接器
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
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

// DBConnector 数据库连接器
type DBConnector struct {
	name   string
	db     *sql.DB
	cfg    *DBConfig
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
}

func NewDBConnector(name string, backend StorageBackend) *DBConnector {
	return &DBConnector{name: name, backend: backend, cfg: &DBConfig{PollSec: 30}}
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
				fmt.Sprintf("SELECT %s, %s, %s FROM %s WHERE %s > $1",
					c.cfg.KeyCol, c.cfg.ContentCol, c.cfg.UpdatedCol, c.cfg.Table, c.cfg.UpdatedCol),
				lastSync)
			if err != nil {
				log.Printf("[connector] %s query: %v", c.name, err)
				continue
			}
			for rows.Next() {
				var key, content string
				var updated time.Time
				if err := rows.Scan(&key, &content, &updated); err != nil {
					continue
				}
				m := &Memory{
					MemoryID:       newMemoryID(),
					IdempotencyKey: c.name + ":" + key,
					TenantID:       "connector",
					UserID:         "system",
					MemoryKind:     KindFact,
					Fact:           content,
					Source:         c.name,
					RecordedAt:     time.Now().UTC(),
					Confidence:     0.7,
					Version:        1,
					Layer:          LayerL1,
				}
				if _, err := c.backend.Upsert(ctx, m); err != nil {
					log.Printf("[connector] %s upsert %s: %v", c.name, key, err)
				}
				lastSync = updated
			}
			rows.Close()
		}
	}
}

// RefSource 引用式记忆来源
type RefSource struct {
	Connector string `json:"connector"`
	Key       string `json:"key"`
	URL       string `json:"url,omitempty"`
}
