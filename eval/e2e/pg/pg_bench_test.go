// R10-B: 真实 PostgreSQL 18 + pgvector 性能基准测试。
//
// 测量指标：
//   - BenchmarkPgUpsert: 写入一条记忆的 ns/op + allocs/op
//   - BenchmarkPgQuery: 文本检索一次的 ns/op + allocs/op
//   - BenchmarkPgVectorSearch: 向量检索一次的 ns/op + allocs/op
//
// 与内存 SQLite 对照（R9-B e2e 基线）：
//   - 内存 SQLite（map 实现）：Upsert ~100ns/op / Query ~200ns/op
//   - 真实 PG（网络+磁盘）：预期 Upsert ~500-2000µs/op / Query ~200-1000µs/op
//   - 报告中记录具体数值，不做性能回归要求（本路目标是验证正确性，不是性能优化）。
package pg

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightmem/kernel"
)

// benchEmbed 确定性 hash embedder（dim=1024）。
func benchEmbed(text string) ([]float32, error) {
	vec := make([]float32, 1024)
	h := uint32(0)
	for _, c := range text {
		h = h*31 + uint32(c)
	}
	for i := 0; i < 32; i++ {
		vec[i] = float32((h>>uint(i%16))&0x7F) / 127.0
	}
	return vec, nil
}

// BenchmarkPgUpsert: 单次 Upsert 写入性能。
func BenchmarkPgUpsert(b *testing.B) {
	cfg := loadPgTestConfigOrNil()
	if cfg == nil {
		b.Skip("LIGHTMEM_PG_HOST not set; skipping benchmark")
	}
	be := newPgTestBackend(&testing.T{}, cfg)
	ctx := context.Background()
	tenant := uniqueTenant("bench_upsert")
	user := "u_bench"

	// 预填充：确保表有一定数据量（PG 性能与表大小相关）
	for i := 0; i < 100; i++ {
		m := makeTestMemory(fmt.Sprintf("bench_pre_%04d", i), tenant, user, fmt.Sprintf("preload fact %d", i))
		_ = be.Upsert(ctx, m)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := makeTestMemory(fmt.Sprintf("bench_up_%06d", i), tenant, user, fmt.Sprintf("benchmark upsert fact %d", i))
		if err := be.Upsert(ctx, m); err != nil {
			b.Fatalf("Upsert %d: %v", i, err)
		}
	}
}

// BenchmarkPgQuery: 文本检索性能。
func BenchmarkPgQuery(b *testing.B) {
	cfg := loadPgTestConfigOrNil()
	if cfg == nil {
		b.Skip("LIGHTMEM_PG_HOST not set; skipping benchmark")
	}
	be := newPgTestBackend(&testing.T{}, cfg)
	ctx := context.Background()
	tenant := uniqueTenant("bench_query")
	user := "u_bench"

	// 预填充 200 条
	for i := 0; i < 200; i++ {
		m := makeTestMemory(fmt.Sprintf("bench_q_%04d", i), tenant, user, fmt.Sprintf("generic fact number %d about topic xyz", i))
		_ = be.Upsert(ctx, m)
	}

	q := kernel.QueryParams{
		TenantID: tenant,
		Viewer:   user,
		Text:     "generic fact",
		TopK:     10,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := be.Query(ctx, q)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
	}
}

// BenchmarkPgVectorSearch: 向量检索性能。
func BenchmarkPgVectorSearch(b *testing.B) {
	cfg := loadPgTestConfigOrNil()
	if cfg == nil {
		b.Skip("LIGHTMEM_PG_HOST not set; skipping benchmark")
	}
	be := newPgTestBackend(&testing.T{}, cfg)
	ctx := context.Background()
	tenant := uniqueTenant("bench_vec")
	user := "u_bench"

	// 预填充 100 条 + backfill 向量
	for i := 0; i < 100; i++ {
		m := makeTestMemory(fmt.Sprintf("bench_v_%04d", i), tenant, user, fmt.Sprintf("vector fact %d about quantum physics", i))
		_ = be.Upsert(ctx, m)
	}
	type backfiller interface {
		BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) (kernel.BackfillReport, error)
	}
	bf := be.(backfiller)
	_, _ = bf.BackfillVectors(ctx, benchEmbed)

	// 查询向量
	queryVec, _ := benchEmbed("quantum physics vector search test")
	vq := kernel.QueryParams{
		TenantID: tenant,
		Viewer:   user,
		Vector:   queryVec,
		TopK:     10,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := be.VectorSearch(ctx, vq)
		if err != nil {
			b.Fatalf("VectorSearch: %v", err)
		}
	}
}
