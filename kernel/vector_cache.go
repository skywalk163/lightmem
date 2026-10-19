// R28-C2（业主裁定一）· /search/vector 结果缓存（LRU + TTL + 命中率埋点）。
//
// 业主裁定原文：
//   - 「应用层缓存：对 /search/vector 的查询结果做 LRU + TTL 缓存，
//     key 用查询向量的哈希 + topK + 过滤条件。」
//   - 「**关键是要埋点统计缓存命中率**。如果命中率低于阈值（比如 60%），说明查询
//     是长尾分布，缓存兜不住，那冗余列/索引就是下一轮的必选项。」
//   - 「不要用缓存把问题盖住就宣布闭环。」
//
// 埋点口径：每次查询在响应头 `X-Vector-Cache: hit|miss|off` 上标记，
// bench（eval/scripts/bench_p95.py）据此统计命中率。另提供 stats() 供日志/诊断。
//
// 语义红线：缓存**不得改变响应语义**——同样的输入必须返回同样的结果集；
// 故 key 覆盖全部影响结果的入参（向量 + topK + tenant + viewer），
// 且结果以副本返回（调用方无法污染缓存）。陈旧由 TTL 兜底（默认 10s）。
package kernel

import (
	"container/list"
	"hash/fnv"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// vectorCacheEntry 是缓存条目。
type vectorCacheEntry struct {
	key      string
	result   []QueryResult
	insertAt time.Time
}

// vectorCache 是 LRU + TTL 缓存。
type vectorCache struct {
	mu     sync.Mutex
	cap    int
	ttl    time.Duration
	items  map[string]*list.Element
	order  *list.List //  front = 最近使用
	hits   int64
	misses int64
}

func newVectorCache(capacity int, ttl time.Duration) *vectorCache {
	if capacity <= 0 {
		capacity = 512
	}
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &vectorCache{
		cap:   capacity,
		ttl:   ttl,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}
}

// get 取缓存；命中则移到队首。过期条目就地淘汰。
func (c *vectorCache) get(key string, now time.Time) ([]QueryResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	e := el.Value.(*vectorCacheEntry)
	if now.Sub(e.insertAt) > c.ttl {
		// 过期：淘汰并计为未命中。
		c.order.Remove(el)
		delete(c.items, key)
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(el)
	c.hits++
	// 副本返回：调用方对切片的修改不会污染缓存。
	out := make([]QueryResult, len(e.result))
	copy(out, e.result)
	return out, true
}

// put 写入缓存，超出容量淘汰最久未用。
func (c *vectorCache) put(key string, res []QueryResult, now time.Time) {
	if len(res) == 0 {
		return // 空结果不入缓存（避免把「暂时为空」固化）
	}
	cp := make([]QueryResult, len(res))
	copy(cp, res)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*vectorCacheEntry).result = cp
		el.Value.(*vectorCacheEntry).insertAt = now
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&vectorCacheEntry{key: key, result: cp, insertAt: now})
	c.items[key] = el
	for c.order.Len() > c.cap {
		last := c.order.Back()
		if last == nil {
			break
		}
		c.order.Remove(last)
		delete(c.items, last.Value.(*vectorCacheEntry).key)
	}
}

// stats 返回累计命中/未命中与命中率（无查询时返回 0）。
func (c *vectorCache) stats() (hits, misses int64, rate float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.hits + c.misses
	if total == 0 {
		return c.hits, c.misses, 0
	}
	return c.hits, c.misses, float64(c.hits) / float64(total)
}

// ---- 进程级实例与配置 ----

var globalVectorCache = newVectorCache(vectorCacheSizeFromEnv(), vectorCacheTTLFromEnv())

func vectorCacheSizeFromEnv() int {
	if v := os.Getenv("LIMS_VECTOR_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return 512
}

func vectorCacheTTLFromEnv() time.Duration {
	if v := os.Getenv("LIMS_VECTOR_CACHE_TTL"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 10 * time.Second
}

// vectorCacheEnabled 报告是否启用结果缓存（默认开；0/off/false 关闭）。
func vectorCacheEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LIMS_VECTOR_CACHE"))) {
	case "0", "off", "false", "disable", "disabled":
		return false
	default:
		return true
	}
}

// vectorCacheKey 生成缓存 key：向量哈希 + topK + tenant + viewer。
//
// 向量用 FNV-1a 64 位哈希（1024 维 float32 → 8KB 输入，哈希远快于字符串化）；
// tenant / viewer 以长度前缀拼接，避免 "a|bc" 与 "ab|c" 碰撞。
func vectorCacheKey(q QueryParams) string {
	h := fnv.New64a()
	// 向量：逐元素写入二进制（小端），避免浮点格式化的开销与歧义。
	buf := make([]byte, 4)
	for _, v := range q.Vector {
		bits := math.Float32bits(v)
		buf[0] = byte(bits)
		buf[1] = byte(bits >> 8)
		buf[2] = byte(bits >> 16)
		buf[3] = byte(bits >> 24)
		_, _ = h.Write(buf)
	}
	return strconv.FormatUint(h.Sum64(), 16) + "|" +
		strconv.Itoa(q.TopK) + "|" +
		strconv.Itoa(len(q.TenantID)) + ":" + q.TenantID + "|" +
		strconv.Itoa(len(q.Viewer)) + ":" + q.Viewer
}

// VectorCacheStats 暴露缓存命中率（供诊断 / 日志）。
func VectorCacheStats() (hits, misses int64, rate float64) {
	return globalVectorCache.stats()
}
