// R28-C2 · /search/vector 结果缓存用例（LRU + TTL + 命中率埋点）。
package kernel

import (
	"testing"
	"time"
)

func TestVectorCacheHitMissAndStats(t *testing.T) {
	c := newVectorCache(2, 10*time.Second)
	now := time.Now()

	res := []QueryResult{{MemoryID: "m1", Score: 0.9}}
	c.put("k1", res, now)

	got, ok := c.get("k1", now.Add(time.Second))
	if !ok {
		t.Fatalf("同 key 未命中（应命中）")
	}
	if len(got) != 1 || got[0].MemoryID != "m1" {
		t.Fatalf("缓存内容错误：%+v", got)
	}
	if _, ok := c.get("nope", now); ok {
		t.Fatalf("不存在的 key 不应命中")
	}

	hits, misses, rate := c.stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("统计错误：hits=%d misses=%d", hits, misses)
	}
	if rate < 0.49 || rate > 0.51 {
		t.Fatalf("命中率应为 0.5，实际 %f", rate)
	}
}

func TestVectorCacheTTLExpiry(t *testing.T) {
	c := newVectorCache(8, 1*time.Second)
	now := time.Now()
	c.put("k", []QueryResult{{MemoryID: "m"}}, now)

	if _, ok := c.get("k", now.Add(500*time.Millisecond)); !ok {
		t.Fatalf("TTL 内应命中")
	}
	if _, ok := c.get("k", now.Add(2*time.Second)); ok {
		t.Fatalf("TTL 过期后不应命中（陈旧结果会破坏语义）")
	}
}

func TestVectorCacheLRUEviction(t *testing.T) {
	c := newVectorCache(2, 60*time.Second)
	now := time.Now()
	c.put("a", []QueryResult{{MemoryID: "a"}}, now)
	c.put("b", []QueryResult{{MemoryID: "b"}}, now)
	// 访问 a 使其成为最近使用
	_, _ = c.get("a", now)
	// 插入 c → 应淘汰最久未用的 b
	c.put("c", []QueryResult{{MemoryID: "c"}}, now.Add(time.Second))

	if _, ok := c.get("a", now.Add(2*time.Second)); !ok {
		t.Fatalf("a 是最近使用，不应被淘汰")
	}
	if _, ok := c.get("b", now.Add(2*time.Second)); ok {
		t.Fatalf("b 是最久未用，应已被淘汰")
	}
	if _, ok := c.get("c", now.Add(2*time.Second)); !ok {
		t.Fatalf("c 应存在")
	}
}

// TestVectorCacheKeyIsolation：key 必须区分 tenant / viewer / topK / 向量，
// 否则会串租户返回结果（安全事故）。
func TestVectorCacheKeyIsolation(t *testing.T) {
	base := QueryParams{Vector: []float32{0.1, 0.2, 0.3}, TopK: 5, TenantID: "t1", Viewer: "u1"}
	diffTenant := base
	diffTenant.TenantID = "t2"
	diffViewer := base
	diffViewer.Viewer = "u2"
	diffTopK := base
	diffTopK.TopK = 10
	diffVec := base
	diffVec.Vector = []float32{0.1, 0.2, 0.3000001}

	if vectorCacheKey(base) == vectorCacheKey(diffTenant) {
		t.Fatal("不同 tenant 的 key 相同 → 跨租户串数据风险")
	}
	if vectorCacheKey(base) == vectorCacheKey(diffViewer) {
		t.Fatal("不同 viewer 的 key 相同 → 越权可见风险")
	}
	if vectorCacheKey(base) == vectorCacheKey(diffTopK) {
		t.Fatal("不同 topK 的 key 相同")
	}
	if vectorCacheKey(base) == vectorCacheKey(diffVec) {
		t.Fatal("不同向量的 key 相同")
	}
	// 长度前缀拼接：防 "a|bc" 与 "ab|c" 类碰撞
	p1 := QueryParams{TenantID: "a", Viewer: "bc", TopK: 1}
	p2 := QueryParams{TenantID: "ab", Viewer: "c", TopK: 1}
	if vectorCacheKey(p1) == vectorCacheKey(p2) {
		t.Fatal("tenant/viewer 拼接存在歧义碰撞")
	}
}

func TestVectorCacheEmptyNotCached(t *testing.T) {
	c := newVectorCache(4, 60*time.Second)
	now := time.Now()
	c.put("empty", nil, now)
	if _, ok := c.get("empty", now.Add(time.Second)); ok {
		t.Fatalf("空结果不应入缓存（会把「暂时为空」固化）")
	}
}
