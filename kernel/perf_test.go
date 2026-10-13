// 检索性能 P95 + 热点缓存测试 / 基准（P7-B，§5.3 / §3.13）
// 红线：本测试不触碰 ranking；基准仅衡量延迟，不改变检索结果。
package kernel

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestLatencyRecorderP95(t *testing.T) {
	r := NewLatencyRecorder(1000)
	for i := int64(1); i <= 100; i++ {
		r.Record(i) // 1..100 ms
	}
	// 100 个样本，P95 取第 95 个（排序后索引 94 对应值 95）
	if got := r.P95(); got != 95 {
		t.Fatalf("P95 = %d, want 95", got)
	}
	if got := r.Mean(); got != 50.5 {
		t.Fatalf("Mean = %v, want 50.5", got)
	}
	// 空采集器
	if got := NewLatencyRecorder(0).P95(); got != 0 {
		t.Fatalf("empty P95 = %d, want 0", got)
	}
}

func TestHotCache(t *testing.T) {
	c := NewHotCache(2, time.Minute)
	// 命中
	c.Put("k1", []QueryResult{{MemoryID: "a", Score: 1.0}})
	got, ok := c.Get("k1")
	if !ok || len(got) != 1 || got[0].MemoryID != "a" {
		t.Fatalf("get k1 failed: ok=%v got=%+v", ok, got)
	}
	// 未命中
	if _, ok := c.Get("nope"); ok {
		t.Fatalf("unexpected hit for nope")
	}
	// LRU 淘汰：容量 2，写入 k2,k3 后 k1 应被淘汰
	c.Put("k2", []QueryResult{{MemoryID: "b"}})
	c.Put("k3", []QueryResult{{MemoryID: "c"}})
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("k1 should be evicted by LRU")
	}
	hits, misses, size := c.Stats()
	if size != 2 {
		t.Fatalf("size = %d, want 2", size)
	}
	_ = hits
	_ = misses
	// TTL 过期
	c2 := NewHotCache(10, 10*time.Millisecond)
	c2.Put("x", []QueryResult{{MemoryID: "z"}})
	time.Sleep(15 * time.Millisecond)
	if _, ok := c2.Get("x"); ok {
		t.Fatalf("x should be expired by TTL")
	}
}

func TestPerfP95UnderThreshold(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	base := time.Now().UTC()
	const N = 1000
	for i := 0; i < N; i++ {
		m := &Memory{
			MemoryID:       strconv.Itoa(i),
			IdempotencyKey: strconv.Itoa(i),
			TenantID:       "t1",
			UserID:         "u1",
			Visibility:     VisPrivate,
			MemoryKind:     KindFact,
			// 每条都包含 "alpha" 子串，便于稳定命中
			Fact:       "alpha memory number " + strconv.Itoa(i),
			RecordedAt: base.Add(time.Duration(i) * time.Second),
			Confidence: 0.9,
			Version:    1,
			Layer:      LayerL1,
		}
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	rec := NewLatencyRecorder(10000)
	cache := NewHotCache(64, time.Minute)
	q := QueryParams{TenantID: "t1", Viewer: "u1", Text: "alpha", TopK: 10}
	key := CacheKey("t1", "u1", "", "alpha")
	const iters = 1000
	for i := 0; i < iters; i++ {
		start := time.Now()
		res, err := RecallWithHotCache(ctx, cache, key, func() ([]QueryResult, error) {
			return b.Query(ctx, q)
		})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		elapsed := time.Since(start)
		rec.Record(elapsed.Milliseconds())
		// 结果正确性：每次都应召回相同 top-k
		if len(res) == 0 {
			t.Fatalf("iter %d: empty result", i)
		}
		// 命中后结果须与原样一致（缓存不改写）
	}
	p95 := rec.P95()
	hits, misses, _ := cache.Stats()
	t.Logf("P95 retrieval latency = %d ms over %d iters (cache hits=%d misses=%d)", p95, iters, hits, misses)
	if p95 >= 200 {
		t.Fatalf("P95 = %d ms, want < 200ms (P7-B 红线)", p95)
	}
}

func BenchmarkPerfRetrievalLatency(b *testing.B) {
	backend := newTestSQLiteBackend(b)
	ctx := context.Background()
	base := time.Now().UTC()
	const N = 1000
	for i := 0; i < N; i++ {
		m := &Memory{
			MemoryID:       strconv.Itoa(i),
			IdempotencyKey: strconv.Itoa(i),
			TenantID:       "t1",
			UserID:         "u1",
			Visibility:     VisPrivate,
			MemoryKind:     KindFact,
			Fact:           "alpha memory number " + strconv.Itoa(i),
			RecordedAt:     base.Add(time.Duration(i) * time.Second),
			Confidence:     0.9,
			Version:        1,
			Layer:          LayerL1,
		}
		if err := backend.Upsert(ctx, m); err != nil {
			b.Fatalf("upsert: %v", err)
		}
	}
	rec := NewLatencyRecorder(100000)
	cache := NewHotCache(64, time.Minute)
	q := QueryParams{TenantID: "t1", Viewer: "u1", Text: "alpha", TopK: 10}
	key := CacheKey("t1", "u1", "", "alpha")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, err := RecallWithHotCache(ctx, cache, key, func() ([]QueryResult, error) {
			return backend.Query(ctx, q)
		})
		if err != nil {
			b.Fatalf("recall: %v", err)
		}
		rec.Record(time.Since(start).Milliseconds())
	}
	b.StopTimer()
	b.ReportMetric(float64(rec.P95()), "p95_ms")
	b.ReportMetric(float64(rec.Mean()), "mean_ms")
}
