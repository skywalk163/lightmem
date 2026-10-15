// R10-A：RankFusion 性能基准（BenchmarkRecallAndFuse）。
//
// 目的：验证 R10-A 并发安全加固（ForRequest 请求级实例构造）相对修复前的
// 性能回退 <5%。评测链路 P95 处于毫秒级以下，绝对数值无意义，采用相对回退对比。
//
// 测量方式：固定候选集 + 固定 RouterContext，重复调用 RecallAndFuse，
// 输出 ns/op 与 allocs/op。
//
// 红线遵守：不调 LLM；确定性；不修改 Fuse 算法 / 权重语义。
package kernel

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// benchRecallServer 构造一个共享 *RankFusion 的 APIServer（模拟生产 main.go 装配）。
// 传入 useFix 以区分修复前后路径（通过构造方式体现，非开关生产代码）。
func benchRecallServer(b *testing.B) *APIServer {
	b.Helper()
	be, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		b.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, be)
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	srv.SetRankFusion(rf)
	// 固定候选集：写入 200 条确定性记忆，保证每次融合候选规模一致。
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		m := &Memory{
			MemoryID:   fmt.Sprintf("bench-m-%03d", i),
			Fact:       fmt.Sprintf("alpha memory number %03d", i),
			TenantID:   "t1",
			UserID:     "u1",
			MemoryKind: KindFact,
			Layer:      LayerL1,
			Confidence: 0.9,
			Visibility: VisPrivate,
			Permission: Permission{Owner: "u1"},
			Source:     "bench",
		}
		if err := srv.backend.Upsert(ctx, m); err != nil {
			b.Fatalf("seed upsert: %v", err)
		}
	}
	return srv
}

// BenchmarkRecallAndFuse 固定 RouterContext 重复 RecallAndFuse，测 ns/op + allocs/op。
// 用于 R10-A 修复前后对比（相对回退 <5%）。
func BenchmarkRecallAndFuse(b *testing.B) {
	srv := benchRecallServer(b)
	ctx := context.Background()
	rc := RouterContext{Text: "alpha", TenantID: "t1", UserID: "u1", TopK: 20}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := srv.RecallAndFuse(ctx, rc)
		if err != nil {
			b.Fatalf("RecallAndFuse: %v", err)
		}
		_ = res
	}
}

// BenchmarkRecallAndFuse_FixedCandidates 固定候选集规模下的纯融合路径基准。
// 与 BenchmarkRecallAndFuse 的区别：本项侧重融合阶段（已缓存召回），供上层对比
// ForRequest 构造开销在候选规模变化下的占比趋势。
func BenchmarkRecallAndFuse_FixedCandidates(b *testing.B) {
	srv := benchRecallServer(b)
	ctx := context.Background()
	// 先做一次召回，固定候选集快照（不重复召回，专注融合路径）。
	pre, err := srv.RecallAndFuse(ctx, RouterContext{Text: "alpha", TenantID: "t1", UserID: "u1", TopK: 20})
	if err != nil {
		b.Fatalf("prep: %v", err)
	}
	flat := make([]RecalledCandidate, 0, len(pre.Fused))
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	// 使用请求级实例路径（R10-A），模拟修复后的融合阶段。
	reqRF := rf.ForRequest(rf.Now, nil, &ReferenceFrame{Tenant: "t1", User: "u1"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := reqRF.Fuse(ctx, "alpha", flat)
		_ = out
	}
}

// BenchmarkForRequest 单独测 ForRequest 构造开销（R10-A 新增路径的成本上限）。
// 用于量化「每次请求新建请求级实例」的绝对开销，判断是否影响 5% 回退红线。
func BenchmarkForRequest(b *testing.B) {
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	qf := &ReferenceFrame{Tenant: "t1", User: "u1"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rf.ForRequest(rf.Now, nil, qf)
	}
}
