// R22.3 token 校验缓存（docs/token-management-design.md §6）
//
// 目的：dbAuthenticator 每次查库正确但高并发下打 DB。加 60s TTL 缓存后：
//   - 命中缓存 → 零 DB 查询（高频 token 校验降负载）；
//   - **吊销/轮换时主动失效**该 token 的缓存 → 仍然"吊销即生效"（修掉
//     "缓存导致吊销延迟"的副作用），缓存只是兜底降负载，不牺牲吊销时效。
//
// 设计口径（R22 定稿决策 #2）：缓存 TTL 60s；缺省共享一个包级缓存，
// 吊销（AdminStore / revoke API）会主动 Invalidate，无需重启即生效。
//
// 零第三方依赖（红线⑤）：仅 stdlib sync + time。
package kernel

import (
	"sync"
	"time"
)

// authCacheTTL 是 token 校验缓存 TTL（设计文档决策 #2，用户确认 60s）。
const authCacheTTL = 60 * time.Second

// 包级共享缓存：所有 dbAuthenticator 与 AdminStore 共用同一份，
// 使"签发→缓存""吊销→失效"跨组件一致（避免各组件各缓一份导致吊销不彻底）。
var defaultAuthCache = newAuthCache(authCacheTTL)

// cacheEntry 缓存一条校验成功的 Identity + 写入时刻。
type cacheEntry struct {
	identity Identity
	at       time.Time
}

// authCache 是带 TTL 的 token→Identity 缓存。仅缓存"校验成功"的结果；
// 失败（吊销/过期/不存在）从不缓存，因此这些状态天然即时生效。
type authCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
}

func newAuthCache(ttl time.Duration) *authCache {
	if ttl <= 0 {
		ttl = authCacheTTL
	}
	return &authCache{entries: map[string]cacheEntry{}, ttl: ttl}
}

// Get 命中且未过期 → (id, true)；过期条目删除并返回 miss。
func (c *authCache) Get(hash string) (Identity, bool) {
	if c == nil {
		return Identity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[hash]
	if !ok {
		return Identity{}, false
	}
	if timeNow().Sub(e.at) > c.ttl {
		delete(c.entries, hash)
		return Identity{}, false
	}
	return e.identity, true
}

// Put 写入一条校验成功的 Identity。
func (c *authCache) Put(hash string, id Identity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hash] = cacheEntry{identity: id, at: timeNow()}
}

// Invalidate 主动失效某 token 的缓存（吊销/轮换时调用，保证吊销即时生效）。
func (c *authCache) Invalidate(hash string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
}

// Reset 清空缓存（测试隔离用）。
func (c *authCache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cacheEntry{}
}

// InvalidateTokenHash 是包级入口：用 token 明文哈希主动失效缓存。
// 供 AdminStore 吊销 / 轮换时调用（保证吊销即时生效，不被缓存延迟）。
func InvalidateTokenHash(hash string) {
	defaultAuthCache.Invalidate(hash)
}
