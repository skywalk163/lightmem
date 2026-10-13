// 检索性能 P95 + 热点缓存（P7-B，§5.3 监控 / §3.13 性能）
// 真源：roadmap.md v4 §3.1 P7-B「检索性能 P95<200ms」、§3.13 热点缓存。
//
// 设计约束（R5-C 红线条款）：
//   - 不触碰检索链路 ranking：HotCache 是「只读旁挂缓存」，命中即原样返回底层检索结果，
//     绝不改写 QueryResult 的 Score / 排序，保证检索 MRR 逐位不回归。
//   - 只增不改 kernel Schema：本文件不改动 Memory / RouterContext / StorageBackend 语义。
package kernel

import (
	"container/list"
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// LatencyRecorder 检索延迟采集器（滑动窗口），支持 P95 统计。
// 线程安全。
type LatencyRecorder struct {
	mu         sync.Mutex
	samples    []int64 // 毫秒
	maxSamples int
}

// NewLatencyRecorder 创建延迟采集器；cap<=0 时默认窗口 10000。
func NewLatencyRecorder(cap int) *LatencyRecorder {
	if cap <= 0 {
		cap = 10000
	}
	return &LatencyRecorder{samples: make([]int64, 0, cap), maxSamples: cap}
}

// Record 记录一次检索延迟（毫秒）。
func (r *LatencyRecorder) Record(ms int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, ms)
	if len(r.samples) > r.maxSamples {
		r.samples = r.samples[r.maxSamples/2:]
	}
}

// P95 返回最近窗口的 P95 延迟（毫秒）；无样本时返回 0。
// 采用最近秩（nearest-rank）法：rank = ceil(0.95 * n)，对应排序后索引 rank-1。
func (r *LatencyRecorder) P95() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return 0
	}
	cp := make([]int64, len(r.samples))
	copy(cp, r.samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(float64(len(cp))*0.95)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// Mean 返回平均延迟（毫秒）。
func (r *LatencyRecorder) Mean() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return 0
	}
	var sum int64
	for _, s := range r.samples {
		sum += s
	}
	return float64(sum) / float64(len(r.samples))
}

// Count 返回样本数。
func (r *LatencyRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.samples)
}

// HotCache 热点缓存（LRU + TTL）。键由 (tenant+user+scene+query) 构造，值是对底层
// 检索结果的原样快照（不改写 Score/排序）。线程安全。
type HotCache struct {
	mu       sync.Mutex
	ll       *list.List // 前端 = 最近使用
	items    map[string]*list.Element
	capacity int
	ttl      time.Duration
	hits     int64
	misses   int64
}

type hotCacheEntry struct {
	key string
	val []QueryResult
	exp int64 // 过期 unix 纳秒
}

// NewHotCache 创建热点缓存；capacity<=0 默认 1024，ttl<=0 默认 5 分钟。
func NewHotCache(capacity int, ttl time.Duration) *HotCache {
	if capacity <= 0 {
		capacity = 1024
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &HotCache{
		ll:       list.New(),
		items:    make(map[string]*list.Element),
		capacity: capacity,
		ttl:      ttl,
	}
}

// cacheKey 构造确定性缓存键（tenant+user+scene+text）。scene 为空时省略。
func CacheKey(tenant, user, scene, text string) string {
	if scene == "" {
		return tenant + "\x1f" + user + "\x1f" + text
	}
	return tenant + "\x1f" + user + "\x1f" + scene + "\x1f" + text
}

// Get 命中且未过期返回缓存结果；否则返回 (nil, false)。
func (c *HotCache) Get(key string) ([]QueryResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	ent := el.Value.(*hotCacheEntry)
	if time.Now().UnixNano() > ent.exp {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	// 返回深拷贝，避免调用方改动底层缓存
	out := make([]QueryResult, len(ent.val))
	copy(out, ent.val)
	c.hits++
	return out, true
}

// Put 写入缓存；超出容量时淘汰最近最少使用的条目。
func (c *HotCache) Put(key string, val []QueryResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make([]QueryResult, len(val))
	copy(snapshot, val)
	if el, ok := c.items[key]; ok {
		el.Value.(*hotCacheEntry).val = snapshot
		el.Value.(*hotCacheEntry).exp = time.Now().Add(c.ttl).UnixNano()
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&hotCacheEntry{key: key, val: snapshot, exp: time.Now().Add(c.ttl).UnixNano()})
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*hotCacheEntry).key)
		}
	}
}

// Len 当前缓存条目数。
func (c *HotCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats 返回 (hits, misses, size)。
func (c *HotCache) Stats() (hits, misses, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, int64(c.ll.Len())
}

// RecallWithHotCache 在 recall 闭包外包一层热点缓存。命中即原样返回；未命中调用底层
// 检索并把结果原样写入缓存。绝不改写 Score/排序，保证检索 MRR 不回归。
func RecallWithHotCache(ctx context.Context, cache *HotCache, key string, recall func() ([]QueryResult, error)) ([]QueryResult, error) {
	if cache != nil {
		if res, ok := cache.Get(key); ok {
			return res, nil
		}
	}
	res, err := recall()
	if err != nil {
		return nil, err
	}
	if cache != nil {
		cache.Put(key, res)
	}
	return res, nil
}
