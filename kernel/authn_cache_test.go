// R22.3 缓存测试（docs/token-management-design.md §6 / kernel/authn_cache.go）
//
// 核心命题：60s TTL 缓存让高频 token 校验零 DB 查询（兜底降负载），
// 同时吊销/轮换主动 Invalidate 保证"吊销即时生效"——两全，不牺牲时效。
package kernel

import (
	"context"
	"testing"
	"time"
)

func TestAuthCachePutGet(t *testing.T) {
	c := newAuthCache(time.Minute)
	c.Put("h1", Identity{UserID: "alice", TenantID: "dshteam"})
	id, ok := c.Get("h1")
	if !ok || id.UserID != "alice" || id.TenantID != "dshteam" {
		t.Fatalf("cache get: got %+v ok=%v", id, ok)
	}
	// miss
	if _, ok := c.Get("nope"); ok {
		t.Fatal("unknown hash should miss")
	}
}

func TestAuthCacheExpire(t *testing.T) {
	c := newAuthCache(2 * time.Millisecond)
	c.Put("h", Identity{UserID: "bob"})
	time.Sleep(10 * time.Millisecond)
	if _, ok := c.Get("h"); ok {
		t.Fatal("expired entry should miss")
	}
}

func TestAuthCacheInvalidateAndReset(t *testing.T) {
	c := newAuthCache(time.Minute)
	c.Put("h", Identity{UserID: "bob"})
	c.Invalidate("h")
	if _, ok := c.Get("h"); ok {
		t.Fatal("after invalidate should miss")
	}
	c.Put("h", Identity{UserID: "bob"})
	c.Reset()
	if _, ok := c.Get("h"); ok {
		t.Fatal("after reset should miss")
	}
	// nil 缓存（NoCache 路径）不 panic
	var nilCache *authCache
	nilCache.Put("h", Identity{})
	if _, ok := nilCache.Get("h"); ok {
		t.Fatal("nil cache should miss")
	}
	nilCache.Invalidate("h")
	nilCache.Reset()
}

// TestDBAuthenticatorCacheServesWithinTTL 证明"缓存兜底降负载"：
// token 缓存后，即使 DB 里被直接吊销（未经 AdminStore 主动失效），
// 60s TTL 内缓存仍提供服务（这正是 60s 接受窗口，设计文档决策 #2）。
func TestDBAuthenticatorCacheServesWithinTTL(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-cache-ttl-x"
	seedAgentToken(t, b, raw, "trae", "dshteam")

	c := newAuthCache(time.Minute) // 独立缓存，避免污染包级 defaultAuthCache
	auth := &dbAuthenticator{db: b.db, driver: "sqlite", cache: c}

	// 首次：查库成功 + 写缓存
	if id, err := auth.Authenticate(context.Background(), raw); err != nil || id.AgentID != "trae" {
		t.Fatalf("fresh authenticate: id=%+v err=%v", id, err)
	}
	// 绕过 AdminStore，直接在 DB 吊销（模拟外部吊销但未主动失效缓存）
	if _, err := b.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE token_hash=?`,
		formatTime(timeNow()), tokenHash(raw)); err != nil {
		t.Fatal(err)
	}
	// 缓存 TTL 内仍生效（接受窗口）
	if id, err := auth.Authenticate(context.Background(), raw); err != nil || id.AgentID != "trae" {
		t.Fatalf("cache should serve within TTL: id=%+v err=%v", id, err)
	}
}

// TestDBAuthenticatorCacheInvalidateIsImmediate 证明"主动失效→吊销即时生效"：
// 吊销/轮换时 AdminStore 调 InvalidateTokenHash，缓存立即清除，
// 下一次校验打 DB，看到 revoked_at → 401（不重启、不等 60s）。
func TestDBAuthenticatorCacheInvalidateIsImmediate(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-cache-inv-x"
	seedAgentToken(t, b, raw, "trae", "dshteam")

	c := newAuthCache(time.Minute)
	auth := &dbAuthenticator{db: b.db, driver: "sqlite", cache: c}

	// 先缓存成功
	if _, err := auth.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	// 吊销 + 主动失效（模拟 AdminStore.handleAgentTokenRevoke 的路径）
	if _, err := b.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE token_hash=?`,
		formatTime(timeNow()), tokenHash(raw)); err != nil {
		t.Fatal(err)
	}
	c.Invalidate(tokenHash(raw))

	// 下一次校验立即看到吊销
	if _, err := auth.Authenticate(context.Background(), raw); err == nil {
		t.Fatal("after invalidate, revoked token must fail immediately")
	}
}

// TestDBProviderExposesDB 验证 sqliteBackend 满足 DBProvider 接口（main.go 依赖）。
func TestDBProviderExposesDB(t *testing.T) {
	b := newTestSQLiteBackend(t)
	var dp DBProvider = b
	if dp.DB() == nil {
		t.Fatal("DBProvider.DB() returned nil")
	}
}

// TestNewDBAuthenticatorNoCache 验证 NoCache 构造：吊销即时可见（无缓存）。
func TestNewDBAuthenticatorNoCache(t *testing.T) {
	b := newAuthSQLite(t)
	raw := "t-nocache-x"
	seedAgentToken(t, b, raw, "trae", "dshteam")
	auth := NewDBAuthenticatorNoCache(b.db, "sqlite")

	if _, err := auth.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("nocache fresh: %v", err)
	}
	if _, err := b.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE token_hash=?`,
		formatTime(timeNow()), tokenHash(raw)); err != nil {
		t.Fatal(err)
	}
	// 无缓存 → 吊销即时可见（无需失效）
	if _, err := auth.Authenticate(context.Background(), raw); err == nil {
		t.Fatal("nocache: revoked token must fail")
	}
}
