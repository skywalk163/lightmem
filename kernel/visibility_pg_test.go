// R24-T0 · L-225：PG 可见性谓词**真机**测试（判据 3 + 判据 4）。
//
// 背景：R23.2 交付的「社区公开 VisPublic」在 SQLite 侧有 `TestCanViewPublic` 覆盖，
// 但 PG 后端的 `pgVisibilityPredicate` **完全没有 public 分支**——R23.2 验收脚本
// `scripts/accept_r23.py` 起的是 solo/sqlite，所以这个 P1 缺陷整整一轮没人发现
// （登记为 **L-225**）。本文件即为堵这个洞的 PG 真机证据。
//
// 硬要求（任务书 §3.4 判据 3）：
//   - **必须真 PG**，不许 SQLite 顶替；
//   - public 记忆能被**另一租户**的 viewer 检索到；
//   - 同一次运行里逐条断言既有四值零回归（判据 4）。
//
// 隔离策略：
//   - 用**独立测试库**（默认 `lightmem_r24_t0`），绝不动生产库 `lims_dsh` 的 60 行数据；
//   - 库不存在时自动创建（postgres 具 createdb 权限）；
//   - 表结构由 `migrations/001_pgvector.up.sql` 幂等引导（只读该文件，不修改）；
//   - 每条用例用唯一 memory_id 前缀，Cleanup 删除自己的数据。
//
// 环境（沿用 kernel/edge_persist_pg_test.go 口径，凭据全走环境变量、不硬编码）：
//
//	LIGHTMEM_PG_HOST      （必填，未设则整文件 t.Skip）
//	LIGHTMEM_PG_PORT      （默认 5432）
//	LIGHTMEM_PG_USER      （默认 postgres）
//	LIGHTMEM_PG_PASSWORD  （可空）
//	LIGHTMEM_PG_TEST_DB   （默认 lightmem_r24_t0）
package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgVisTestDB 独立测试库名（默认 lightmem_r24_t0，可用环境变量覆盖）。
const pgVisTestDB = "lightmem_r24_t0"

// pgVisConfig PG 真机连接配置。
type pgVisConfig struct {
	host     string
	port     int
	user     string
	password string
	dbname   string
}

// loadPgVisConfig 读环境变量；未设 LIGHTMEM_PG_HOST 返回 nil（调用方 t.Skip）。
func loadPgVisConfig() *pgVisConfig {
	host := os.Getenv("LIGHTMEM_PG_HOST")
	if host == "" {
		return nil
	}
	port := 5432
	if p := os.Getenv("LIGHTMEM_PG_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	user := os.Getenv("LIGHTMEM_PG_USER")
	if user == "" {
		user = "postgres"
	}
	dbname := os.Getenv("LIGHTMEM_PG_TEST_DB")
	if dbname == "" {
		dbname = pgVisTestDB
	}
	return &pgVisConfig{
		host:     host,
		port:     port,
		user:     user,
		password: os.Getenv("LIGHTMEM_PG_PASSWORD"),
		dbname:   dbname,
	}
}

// pgVisDSN 拼连接串。
func (c *pgVisConfig) pgVisDSN(db string) string {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
		c.host, c.port, c.user, db)
	if c.password != "" {
		dsn += " password=" + c.password
	}
	return dsn
}

// openPgVis 打开连接并 Ping。
func openPgVis(t *testing.T, c *pgVisConfig, db string) *sql.DB {
	t.Helper()
	d, err := sql.Open("pgx", c.pgVisDSN(db))
	if err != nil {
		t.Fatalf("pg open: %v", err)
	}
	d.SetMaxOpenConns(5)
	d.SetMaxIdleConns(2)
	d.SetConnMaxLifetime(30 * time.Minute)
	if err := d.Ping(); err != nil {
		d.Close()
		t.Fatalf("pg ping (%s): %v", db, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// ensurePgVisDatabase 幂等创建独立测试库（不动生产库）。
func ensurePgVisDatabase(t *testing.T, c *pgVisConfig) {
	t.Helper()
	adm := openPgVis(t, c, "postgres")
	var n int
	if err := adm.QueryRow("SELECT count(*) FROM pg_database WHERE datname=$1", c.dbname).Scan(&n); err != nil {
		t.Fatalf("check database: %v", err)
	}
	if n == 0 {
		// datname 不能参数化，但库名来自常量/环境变量且此处仅测试库，先做标识符校验。
		if !pgIdentOK(c.dbname) {
			t.Fatalf("unsafe database name: %q", c.dbname)
		}
		if _, err := adm.Exec(fmt.Sprintf("CREATE DATABASE %q", c.dbname)); err != nil {
			t.Fatalf("create database %s: %v", c.dbname, err)
		}
		t.Logf("created isolated test database %s", c.dbname)
	} else {
		t.Logf("reusing isolated test database %s", c.dbname)
	}
}

// pgIdentOK 保守的标识符白名单校验（只允许 [a-z0-9_]，长度 ≤ 63）。
func pgIdentOK(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

// bootstrapPgVisSchema 用生产迁移脚本幂等建表（**只读该文件，不修改**）。
func bootstrapPgVisSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	raw, err := os.ReadFile("migrations/001_pgvector.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
}

// newPgVisBackend 构造**真实** pgBackend（走 newPgBackend 生产装配路径）。
func newPgVisBackend(t *testing.T, c *pgVisConfig) StorageBackend {
	t.Helper()
	ensurePgVisDatabase(t, c)
	db := openPgVis(t, c, c.dbname)
	bootstrapPgVisSchema(t, db)

	b, err := newPgBackend(context.Background(), &StorageConfig{
		PG: &PGConfig{
			Host:     c.host,
			Port:     c.port,
			Database: c.dbname,
			User:     c.user,
			Password: c.password,
			SSLMode:  "disable",
			PoolSize: 5,
		},
	})
	if err != nil {
		t.Fatalf("newPgBackend: %v", err)
	}
	return b
}

// pgVisSeed 写入一条指定可见性的记忆；返回 memory_id。
func pgVisSeed(t *testing.T, b StorageBackend, id string, vis Visibility,
	tenant, user string, readers []string, sharedTenants []string) string {
	t.Helper()
	m := &Memory{
		MemoryID:          id,
		IdempotencyKey:    id,
		TenantID:          tenant,
		UserID:            user,
		Visibility:        vis,
		MemoryKind:        KindFact,
		Fact:              "L225 visibility probe " + id,
		Confidence:        0.9,
		SharedWithTenants: sharedTenants,
		Permission:        Permission{Owner: user, Readers: readers},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = b.(*pgBackend).db.Exec(`DELETE FROM memories WHERE memory_id=$1`, id)
	})
	return id
}

// pgVisQueryIDs 用真实 pgBackend.Query 取回可见的 memory_id 集合。
func pgVisQueryIDs(t *testing.T, b StorageBackend, viewer, tenant string) map[string]bool {
	t.Helper()
	res, err := b.Query(context.Background(), QueryParams{
		Text:     "L225",
		Viewer:   viewer,
		TenantID: tenant,
		TopK:     50,
	})
	if err != nil {
		t.Fatalf("query(viewer=%s,tenant=%s): %v", viewer, tenant, err)
	}
	out := make(map[string]bool)
	for _, r := range res {
		if strings.HasPrefix(r.MemoryID, "l225-") {
			out[r.MemoryID] = true
		}
	}
	return out
}

// TestPGVisibilityPublicCrossTenant 判据 3：**PG 真机**下 visibility=public 的记忆
// 能被**另一租户**的 viewer 检索到。
//
// 修复前该断言必失败——这正是 L-225。
func TestPGVisibilityPublicCrossTenant(t *testing.T) {
	cfg := loadPgVisConfig()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST not set; skipping real-PG visibility test")
	}
	b := newPgVisBackend(t, cfg)

	// tenant-a / alice 发布的一条 public 记忆
	pub := pgVisSeed(t, b, "l225-pub", VisPublic, "tenant-a", "alice", nil, nil)

	// 另一租户 tenant-b 的 viewer bob
	seen := pgVisQueryIDs(t, b, "bob", "tenant-b")
	if !seen[pub] {
		t.Fatalf("L-225: public memory %s must be visible to viewer 'bob' of tenant-b on real PG; got %v",
			pub, seen)
	}
	// 仅有 viewer 身份、无租户也应可见（对齐 canView 的 viewer != ""）
	seenNoTenant := pgVisQueryIDs(t, b, "bob", "")
	if !seenNoTenant[pub] {
		t.Fatalf("L-225: public memory %s must be visible to viewer without tenant; got %v", pub, seenNoTenant)
	}
	t.Logf("L-225 OK: public memory %s visible cross-tenant on real PG (%s)", pub, cfg.dbname)
}

// TestPGVisibilityFourValuesNoRegression 判据 4：既有四值零回归，逐条断言。
// private 仍只 owner 可见、tenant 不跨租户、shared_user 只看 readers、
// cross_tenant 看 shared_with_tenants；匿名 viewer（空身份）不得见任何一条。
func TestPGVisibilityFourValuesNoRegression(t *testing.T) {
	cfg := loadPgVisConfig()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST not set; skipping real-PG visibility test")
	}
	b := newPgVisBackend(t, cfg)

	priv := pgVisSeed(t, b, "l225-private", VisPrivate, "tenant-a", "alice", nil, nil)
	shared := pgVisSeed(t, b, "l225-shared", VisSharedUser, "tenant-a", "alice", []string{"carol"}, nil)
	ten := pgVisSeed(t, b, "l225-tenant", VisTenant, "tenant-a", "alice", nil, nil)
	cross := pgVisSeed(t, b, "l225-cross", VisCrossTenant, "tenant-a", "alice", nil, []string{"tenant-b"})
	pub := pgVisSeed(t, b, "l225-pub2", VisPublic, "tenant-a", "alice", nil, nil)

	cases := []struct {
		name    string
		viewer  string
		tenant  string
		wantYes []string
		wantNo  []string
	}{
		{
			name: "private 仅 owner", viewer: "alice", tenant: "tenant-a",
			wantYes: []string{priv, shared, ten, pub}, wantNo: []string{cross},
		},
		{
			name: "private 对同租户他人不可见", viewer: "bob", tenant: "tenant-a",
			wantYes: []string{ten, pub}, wantNo: []string{priv, shared, cross},
		},
		{
			name: "tenant 不跨租户", viewer: "bob", tenant: "tenant-b",
			wantYes: []string{cross, pub}, wantNo: []string{priv, shared, ten},
		},
		{
			name: "shared_user 只看 readers", viewer: "carol", tenant: "tenant-a",
			wantYes: []string{shared, ten, pub}, wantNo: []string{priv, cross},
		},
		{
			name: "cross_tenant 看 shared_with_tenants", viewer: "dave", tenant: "tenant-b",
			wantYes: []string{cross, pub}, wantNo: []string{priv, shared, ten},
		},
		{
			// 既有语义（R21-B 修 G8 时定稿，canView 的 case VisTenant 同样不看 viewer）：
			// tenant 记忆的判据只有 tenant_id 匹配且 tenant 非空，viewer 身份由服务层鉴权保证。
			// 因此空 viewer + 有 tenant 时可见 tenant 属**预期行为**，不是泄漏。
			// 本用例真正要卡的是：匿名 viewer 不得见到 private / shared_user /
			// cross_tenant / public（这四者都要求 viewer 非空）。
			name: "匿名 viewer 不得见 private/shared/cross/public", viewer: "", tenant: "tenant-a",
			wantYes: []string{ten}, wantNo: []string{priv, shared, cross, pub},
		},
	}

	for _, c := range cases {
		seen := pgVisQueryIDs(t, b, c.viewer, c.tenant)
		for _, id := range c.wantYes {
			if !seen[id] {
				t.Errorf("case %q: want %s visible to (%s,%s), not found", c.name, id, c.viewer, c.tenant)
			}
		}
		for _, id := range c.wantNo {
			if seen[id] {
				t.Errorf("case %q: %s must NOT be visible to (%s,%s) — 越权泄漏", c.name, id, c.viewer, c.tenant)
			}
		}
	}
}

// TestPGVisibilityPublicGet 补充：单条 Get 路径（backend.Get）也放行 public。
// Query 与 Get 是两条独立 SQL，谓词共用但调用点不同，都要覆盖。
func TestPGVisibilityPublicGet(t *testing.T) {
	cfg := loadPgVisConfig()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST not set; skipping real-PG visibility test")
	}
	b := newPgVisBackend(t, cfg)
	pub := pgVisSeed(t, b, "l225-pub-get", VisPublic, "tenant-a", "alice", nil, nil)

	m, err := b.Get(context.Background(), pub, "bob", "tenant-b")
	if err != nil {
		t.Fatalf("get public cross-tenant: %v", err)
	}
	if m == nil || m.MemoryID != pub {
		t.Fatalf("L-225: Get must return public memory cross-tenant, got %v", m)
	}

	// 零回归：private 单条 Get 对他人仍应拒绝（nil 或 not found）。
	priv := pgVisSeed(t, b, "l225-priv-get", VisPrivate, "tenant-a", "alice", nil, nil)
	if got, _ := b.Get(context.Background(), priv, "bob", "tenant-a"); got != nil {
		t.Errorf("private memory %s leaked via Get to non-owner", priv)
	}
}
