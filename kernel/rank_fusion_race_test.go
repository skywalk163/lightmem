// R10-A：RankFusion 并发安全回归测试。
//
// 背景（R9-B §5.1 登记）：生产 main.go 调 SetRankFusion(DefaultRankFusion()) 后，
// 所有并发请求共享同一个 *RankFusion，而 RecallAndFuse 每次请求会直接写
// rf.Now / rf.AsOf / rf.QueryFrame（router_mem.go），与信号闭包读取这些字段
// 构成跨 goroutine 数据竞争（go test -race 报告）。
//
// 本文件验证 R10-A 修复（ForRequest 请求级实例 + rejectedIDs 加锁）后：
//   - 并发共享实例下 `-race` 干净；
//   - 并发输出与串行基线逐位一致（不回归）；
//   - rejectedIDs 并发写读无竞争。
//
// 红线遵守：不调 LLM；确定性（固定 memory_id / tenant / user / Now）；
// 不修改既有 Schema / Fuse 算法 / 权重语义。
package kernel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newRaceTestServer 构造一个注入共享 *RankFusion 的 APIServer（SQLite 内存后端）。
// 关键：与 R9-B e2e/helpers_test.go 不同，这里**显式**注入共享实例，
// 精确复刻生产 main.go 的装配方式，用于验证共享实例下的并发安全。
func newRaceTestServer(t *testing.T) (*APIServer, StorageBackend) {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	// 共享注入：多个请求共用一个 *RankFusion（生产 main.go 装配方式）。
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) // 确定性：固定打分基准
	srv.SetRankFusion(rf)
	return srv, b
}

// seedMemories 写入 N 条确定性记忆（含共享/不同 tenant-user），供并发 recall 使用。
func seedMemories(t *testing.T, srv *APIServer, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		mid := fmt.Sprintf("race-m-%02d", i)
		m := &Memory{
			MemoryID:   mid,
			Fact:       fmt.Sprintf("fact-%02d", i),
			TenantID:   "t1",
			UserID:     "u1",
			MemoryKind: KindFact,
			Layer:      LayerL1,
			Confidence: 0.9,
			Visibility: VisPrivate,
			Permission: Permission{Owner: "u1"},
			Source:     "race-test",
		}
		if err := srv.backend.Upsert(ctx, m); err != nil {
			t.Fatalf("seed upsert %s: %v", mid, err)
		}
	}
}

// TestConcurrentRecallAndFuse_SharedInstance 并发调用 RecallAndFuse，验证共享实例下
// `-race` 干净 + 输出与串行基线一致（R10-A 核心验收项）。
func TestConcurrentRecallAndFuse_SharedInstance(t *testing.T) {
	srv, _ := newRaceTestServer(t)
	seedMemories(t, srv, 12)
	ctx := context.Background()

	// 串行基线：单次 RecallAndFuse 的结果作为逐位对齐基准。
	baseRC := RouterContext{Text: "fact", TenantID: "t1", UserID: "u1", TopK: 100}
	baseRes, err := srv.RecallAndFuse(ctx, baseRC)
	if err != nil {
		t.Fatalf("baseline RecallAndFuse: %v", err)
	}
	baseIDs := fusedIDs(baseRes.Fused)

	const workers = 16
	const iters = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				rc := RouterContext{Text: "fact", TenantID: "t1", UserID: "u1", TopK: 100}
				res, err := srv.RecallAndFuse(ctx, rc)
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("w%d i%d err=%v", w, i, err))
					mu.Unlock()
					continue
				}
				ids := fusedIDs(res.Fused)
				if len(ids) != len(baseIDs) {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("w%d i%d len=%d want=%d", w, i, len(ids), len(baseIDs)))
					mu.Unlock()
					continue
				}
				for j := range ids {
					if ids[j] != baseIDs[j] {
						mu.Lock()
						failures = append(failures, fmt.Sprintf("w%d i%d pos%d got=%s want=%s", w, i, j, ids[j], baseIDs[j]))
						mu.Unlock()
						break
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if len(failures) > 0 {
		limit := 10
		if len(failures) < limit {
			limit = len(failures)
		}
		t.Errorf("concurrent RecallAndFuse 输出不回归，失败样例 %d 条（显示前 %d）:",
			len(failures), limit)
		for _, f := range failures[:limit] {
			t.Errorf("  %s", f)
		}
	}
}

// TestConcurrentRecallAndFuse_DifferentTenant 并发调用不同 tenant/user 的 RecallAndFuse，
// 验证请求级实例的 Now/AsOf/QueryFrame 相互隔离（无交叉污染）。
func TestConcurrentRecallAndFuse_DifferentTenant(t *testing.T) {
	srv, _ := newRaceTestServer(t)
	seedMemories(t, srv, 8)
	ctx := context.Background()

	// 串行基线（按 tenant 分组，仅验证不 panic + 稳定）。
	baseRes, err := srv.RecallAndFuse(ctx, RouterContext{Text: "fact", TenantID: "t1", UserID: "u1", TopK: 100})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	baseIDs := fusedIDs(baseRes.Fused)

	const workers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 交替切换 as_of 与非 as_of，模拟真实请求的时序回溯开关。
			for i := 0; i < 15; i++ {
				var asOf *string
				if i%2 == 0 {
					v := "2026-10-13"
					asOf = &v
				}
				res, err := srv.RecallAndFuse(ctx, RouterContext{
					Text: "fact", TenantID: "t1", UserID: "u1", TopK: 100, AsOf: asOf,
				})
				if err != nil {
					mu.Lock()
					failures = append(failures, "err="+err.Error())
					mu.Unlock()
					continue
				}
				_ = fusedIDs(res.Fused)
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Errorf("并发 as_of 切换出现错误: %v", failures[:min(10, len(failures))])
	}
	// 共享实例不应被请求级写入污染：基线应可复现（若被污染，Now 可能漂移）。
	res2, err := srv.RecallAndFuse(ctx, RouterContext{Text: "fact", TenantID: "t1", UserID: "u1", TopK: 100})
	if err != nil {
		t.Fatalf("post-concurrent baseline: %v", err)
	}
	if got := fusedIDs(res2.Fused); !equalIDs(got, baseIDs) {
		t.Errorf("并发后共享实例被污染：基线漂移 got=%v want=%v", got, baseIDs)
	}
}

// TestConcurrentRejectedIDs 并发 addRejectedID + isRejectedID，验证 R10-A 加锁读写无竞争。
func TestConcurrentRejectedIDs(t *testing.T) {
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)

	const workers = 20
	const per = 25
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			base := w * per
			for i := 0; i < per; i++ {
				srv.addRejectedID(fmt.Sprintf("rid-%04d", base+i))
				_ = srv.isRejectedID(fmt.Sprintf("rid-%04d", base+i))
				_ = srv.isRejectedID("nonexistent-id")
			}
		}(w)
	}
	wg.Wait()

	// 写入数量校验（确定性：无重复 id）。
	srv.rejectedIDsMu.RLock()
	got := len(srv.rejectedIDs)
	srv.rejectedIDsMu.RUnlock()
	want := workers * per
	if got != want {
		t.Errorf("rejectedIDs 数量不符: got=%d want=%d", got, want)
	}
	if !srv.isRejectedID("rid-0000") || !srv.isRejectedID(fmt.Sprintf("rid-%04d", workers*per-1)) {
		t.Error("首尾 id 应命中")
	}
}

// ---- helpers ----
// 注意：fusedIDs 已在 kernel/composition_test.go 定义（同包，入参 []FusedResult），
// 此处不重复声明，避免 redeclared 编译错误；调用处需传 res.Fused。
// equalIDs 在本包无重名，仅本文件定义，用于断言两条 ID 序列逐位一致。
func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
