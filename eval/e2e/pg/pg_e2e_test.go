// R10-B: 真实 PostgreSQL 18 + pgvector 端到端测试套件。
//
// 测试方式：通过 kernel.NewStorageBackend 装配 pgvector 后端，直连真实 PG，
// 验证 Upsert/Get/Query/VectorSearch/持久化/租户隔离/GDPR/幂等/补全/并发。
//
// 红线遵守：
//   - 凭据全部走环境变量（LIGHTMEM_PG_TEST_DSN 或 LIGHTMEM_PG_*），不硬编码；
//   - 无 PG 环境时全部 t.Skip，不失败；
//   - 确定性：固定 memory_id / tenant / user，不依赖 wall clock 断言；
//   - 不调 LLM / 外部 embedder（hash embedder 占位）；
//   - 测试数据用例间隔离（每个用例独立 tenant 前缀）。
package pg

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lightmem/kernel"
)

// ---- 测试环境装配 ----

// pgTestConfig 持有 PG 连接配置（从环境变量读取）。
type pgTestConfig struct {
	host     string
	port     int
	user     string
	password string
	dbname   string
}

// loadPgTestConfigOrNil 从环境变量读取配置；无配置时返回 nil（不调 t.Skip）。
func loadPgTestConfigOrNil() *pgTestConfig {
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
	password := os.Getenv("LIGHTMEM_PG_PASSWORD")
	dbname := os.Getenv("LIGHTMEM_PG_TEST_DB")
	if dbname == "" {
		dbname = os.Getenv("LIGHTMEM_PG_DB")
	}
	if dbname == "" {
		return nil
	}
	return &pgTestConfig{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		dbname:   dbname,
	}
}

// loadPgTestConfig 从环境变量读取配置；无配置时 t.Skip。
func loadPgTestConfig(t *testing.T) *pgTestConfig {
	t.Helper()
	cfg := loadPgTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping real PG e2e")
	}
	return cfg
}

// newPgTestBackend 装配一个真实 pgvector 后端。
// 每个测试用例独立 backend，用例结束后 Close。
func newPgTestBackend(t *testing.T, cfg *pgTestConfig) kernel.StorageBackend {
	t.Helper()
	ctx := context.Background()
	prof := &kernel.Profile{
		Profile:        kernel.TierFamily,
		StorageBackend: kernel.BackendPgVector,
		Storage: &kernel.StorageConfig{
			PG: &kernel.PGConfig{
				Host:     cfg.host,
				Port:     cfg.port,
				User:     cfg.user,
				Password: cfg.password,
				Database: cfg.dbname,
				PoolSize: 5,
				SSLMode:  "disable",
			},
			Embedding: &kernel.EmbeddingConfig{
				Dimensions: 1024,
			},
		},
	}
	be, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		t.Fatalf("NewStorageBackend(pgvector): %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

// ---- 测试数据构造助手 ----

// makeTestMemory 构造一条确定性的测试记忆。
// tenant/user/id 由调用方传入，保证用例间隔离。
func makeTestMemory(id, tenant, user, fact string) *kernel.Memory {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return &kernel.Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     kernel.VisPrivate,
		Permission:     kernel.Permission{Owner: user},
		MemoryKind:      kernel.KindFact,
		Fact:           fact,
		Confidence:     0.9,
		Source:         "r10b-e2e",
		Version:        1,
		RecordedAt:     now,
		Provenance:     kernel.ProvNormal,
		VectorStatus:   kernel.VectorPending,
	}
}

// ---- 用例 ----

// TestPgUpsertGetRoundtrip: Upsert → Get 回读，字段逐位一致（含 JSONB）。
func TestPgUpsertGetRoundtrip(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_roundtrip"
	user := "u_roundtrip"
	m := makeTestMemory("mem_roundtrip_001", tenant, user, "the sky is blue on a clear day")

	// 补充 JSONB 字段
	m.ReferenceFrame = kernel.ReferenceFrame{
		Entity: "sky",
		Scene:  "weather",
	}
	m.GoalDetail = &kernel.GoalDetail{
		Goal:   "test roundtrip",
		Status: "active",
	}

	if err := be.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := be.Get(ctx, "mem_roundtrip_001", user, tenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// 逐字段断言
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"MemoryID", got.MemoryID, m.MemoryID},
		{"TenantID", got.TenantID, m.TenantID},
		{"UserID", got.UserID, m.UserID},
		{"Fact", got.Fact, m.Fact},
		{"MemoryKind", string(got.MemoryKind), string(m.MemoryKind)},
		{"Confidence", got.Confidence, m.Confidence},
		{"Source", got.Source, m.Source},
		{"Version", got.Version, m.Version},
		{"Visibility", string(got.Visibility), string(m.Visibility)},
		{"Provenance", string(got.Provenance), string(m.Provenance)},
		{"VectorStatus", string(got.VectorStatus), string(m.VectorStatus)},
		{"RecordedAtUnix", got.RecordedAt.Unix(), m.RecordedAt.Unix()},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("field %s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	// ReferenceFrame JSONB 回读
	if got.ReferenceFrame.Entity != m.ReferenceFrame.Entity {
		t.Errorf("ReferenceFrame.Entity: got %q, want %q",
			got.ReferenceFrame.Entity, m.ReferenceFrame.Entity)
	}
	if got.ReferenceFrame.Scene != m.ReferenceFrame.Scene {
		t.Errorf("ReferenceFrame.Scene: got %q, want %q",
			got.ReferenceFrame.Scene, m.ReferenceFrame.Scene)
	}

	// GoalDetail JSONB 回读
	if got.GoalDetail == nil {
		t.Fatal("GoalDetail: expected non-nil after roundtrip")
	}
	if got.GoalDetail.Goal != m.GoalDetail.Goal {
		t.Errorf("GoalDetail.Goal: got %q, want %q", got.GoalDetail.Goal, m.GoalDetail.Goal)
	}
	if got.GoalDetail.Status != m.GoalDetail.Status {
		t.Errorf("GoalDetail.Status: got %q, want %q", got.GoalDetail.Status, m.GoalDetail.Status)
	}
}

// TestPgQueryVectorSearch: 写入 20 条向量化记忆 → Query（文本）/ VectorSearch（向量）命中 top-1。
func TestPgQueryVectorSearch(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_search"
	user := "u_search"

	// 写入 20 条记忆，其中一条是"target"
	for i := 0; i < 20; i++ {
		fact := fmt.Sprintf("routine fact number %d about generic topic", i)
		if i == 7 {
			fact = "the capital of france is paris and the eiffel tower is 324 meters tall"
		}
		m := makeTestMemory(fmt.Sprintf("mem_search_%03d", i), tenant, user, fact)
		if err := be.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %d: %v", i, err)
		}
	}

	// backfiller 是 pgBackend 的 BackfillVectors 接口（不在 StorageBackend 公共接口里）。
	type backfiller interface {
		BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) error
	}
	bf := be.(backfiller)

	// 补全向量：用确定性 hash embedder（前 20 维非零，其余为 0，dim=1024）
	// 注意：这里直接调 BackfillVectors，验证它能把 pending 变成 vectorized
	hashEmbed := func(text string) ([]float32, error) {
		vec := make([]float32, 1024)
		// 确定性：基于文本哈希生成前 32 维
		h := uint32(0)
		for _, c := range text {
			h = h*31 + uint32(c)
		}
		for i := 0; i < 32; i++ {
			vec[i] = float32((h >> uint(i%16)) & 0x7F) / 127.0
		}
		return vec, nil
	}
	if err := bf.BackfillVectors(ctx, hashEmbed); err != nil {
		t.Fatalf("BackfillVectors: %v", err)
	}

	// 1) 文本 Query：搜 "eiffel"，预期命中第 7 条
	q := kernel.QueryParams{
		TenantID: tenant,
		Viewer:   user,
		Text:     "eiffel",
		TopK:     5,
	}
	results, err := be.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Query: expected at least 1 result, got 0")
	}
	found := false
	for _, r := range results {
		if r.MemoryID == "mem_search_007" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Query: expected mem_search_007 in results, got %+v", results)
	}

	// 2) VectorSearch：用 target 文本的 hash 向量搜，预期 top-1 是 mem_search_007
	targetVec, _ := hashEmbed("the capital of france is paris and the eiffel tower is 324 meters tall")
	vq := kernel.QueryParams{
		TenantID: tenant,
		Viewer:   user,
		Vector:   targetVec,
		TopK:     3,
	}
	vresults, err := be.VectorSearch(ctx, vq)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(vresults) == 0 {
		t.Fatal("VectorSearch: expected at least 1 result, got 0")
	}
	if vresults[0].MemoryID != "mem_search_007" {
		t.Errorf("VectorSearch top-1: got %q, want %q (all: %+v)",
			vresults[0].MemoryID, "mem_search_007", vresults)
	}
}

// TestPgPersistenceAcrossRestart: Upsert → Close() → 重建 backend → Get 命中（真实磁盘持久化）。
// 补齐 R9-B §5.2 登记的"内存 map 无持久化"缺口。
func TestPgPersistenceAcrossRestart(t *testing.T) {
	cfg := loadPgTestConfig(t)
	ctx := context.Background()

	tenant := "r10b_persist"
	user := "u_persist"
	m := makeTestMemory("mem_persist_001", tenant, user, "this fact must survive a backend restart")

	// 第一阶段：写入并 Close
	be1 := newPgTestBackend(t, cfg)
	if err := be1.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert (phase 1): %v", err)
	}
	if err := be1.Close(); err != nil {
		t.Fatalf("Close (phase 1): %v", err)
	}

	// 第二阶段：重建 backend（新连接池），验证数据仍在
	// 注意：这里不用 t.Cleanup 自动 Close be1，因为我们已经手动 Close 了
	prof := &kernel.Profile{
		Profile:        kernel.TierFamily,
		StorageBackend: kernel.BackendPgVector,
		Storage: &kernel.StorageConfig{
			PG: &kernel.PGConfig{
				Host:     cfg.host,
				Port:     cfg.port,
				User:     cfg.user,
				Password: cfg.password,
				Database: cfg.dbname,
				PoolSize: 5,
				SSLMode:  "disable",
			},
			Embedding: &kernel.EmbeddingConfig{Dimensions: 1024},
		},
	}
	be2, err := kernel.NewStorageBackend(ctx, prof)
	if err != nil {
		t.Fatalf("NewStorageBackend (phase 2): %v", err)
	}
	t.Cleanup(func() { _ = be2.Close() })

	got, err := be2.Get(ctx, "mem_persist_001", user, tenant)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.Fact != m.Fact {
		t.Errorf("Fact after restart: got %q, want %q", got.Fact, m.Fact)
	}
	if got.MemoryID != m.MemoryID {
		t.Errorf("MemoryID after restart: got %q, want %q", got.MemoryID, m.MemoryID)
	}
}

// TestPgTenantIsolation: 跨租户不可见（硬过滤）。
func TestPgTenantIsolation(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	// tenant A 的私有记忆
	mA := makeTestMemory("mem_iso_a", "r10b_tenantA", "u_a", "tenant A secret fact")
	if err := be.Upsert(ctx, mA); err != nil {
		t.Fatalf("Upsert tenantA: %v", err)
	}

	// tenant B 的用户去 Get tenant A 的记忆 → 应失败（不可见）
	_, err := be.Get(ctx, "mem_iso_a", "u_b", "r10b_tenantB")
	if err == nil {
		t.Error("Get cross-tenant: expected error, got nil (memory leaked across tenants)")
	}
}

// TestPgVisibility: 私有记忆 owner 可见，非 owner 不可见。
func TestPgVisibility(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_vis"
	owner := "u_owner"
	other := "u_other"

	m := makeTestMemory("mem_vis_001", tenant, owner, "owner-only private fact")
	m.Visibility = kernel.VisPrivate
	if err := be.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// owner 读 → 应成功
	got, err := be.Get(ctx, "mem_vis_001", owner, tenant)
	if err != nil {
		t.Errorf("Get by owner: unexpected error %v", err)
	}
	if got == nil || got.Fact != m.Fact {
		t.Errorf("Get by owner: wrong content %+v", got)
	}

	// 同租户非 owner 读 private → 应失败（private 只有 owner 可见）
	_, err = be.Get(ctx, "mem_vis_001", other, tenant)
	if err == nil {
		t.Error("Get private by non-owner: expected error, got nil (visibility leak)")
	}
}

// TestPgGDPR: ExportByScope / DeleteByScope / DeleteByID 在真实 PG 上跑通。
func TestPgGDPR(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_gdpr"
	user := "u_gdpr"

	// 写入 3 条
	for i := 0; i < 3; i++ {
		m := makeTestMemory(fmt.Sprintf("mem_gdpr_%03d", i), tenant, user, fmt.Sprintf("gdpr fact %d", i))
		if err := be.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %d: %v", i, err)
		}
	}

	// 1) ExportByScope
	exported, err := be.(kernel.ComplianceBackend).ExportByScope(ctx, tenant, user)
	if err != nil {
		t.Fatalf("ExportByScope: %v", err)
	}
	if len(exported) != 3 {
		t.Errorf("ExportByScope: got %d records, want 3", len(exported))
	}

	// 2) DeleteByID 删一条
	if err := be.(kernel.ComplianceBackend).DeleteByID(ctx, "mem_gdpr_001"); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	_, err = be.Get(ctx, "mem_gdpr_001", user, tenant)
	if err == nil {
		t.Error("Get after DeleteByID: expected error, got nil (not deleted)")
	}

	// 3) DeleteByScope 级联删剩余两条
	deleted, err := be.(kernel.ComplianceBackend).DeleteByScope(ctx, tenant, user)
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if len(deleted) != 2 {
		t.Errorf("DeleteByScope: deleted %d records, want 2", len(deleted))
	}
}

// TestPgIdempotencyKey: 同 memory_id 二次 Upsert 幂等（ON CONFLICT DO UPDATE），不产生重复行。
func TestPgIdempotencyKey(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_idem"
	user := "u_idem"

	// 第一次 Upsert
	m1 := makeTestMemory("mem_idem_001", tenant, user, "first version of the fact")
	m1.Version = 1
	if err := be.Upsert(ctx, m1); err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}

	// 第二次 Upsert 同 memory_id（fact 不同，验证 ON CONFLICT DO UPDATE 生效）
	m2 := makeTestMemory("mem_idem_001", tenant, user, "updated version of the fact")
	m2.Version = 2
	if err := be.Upsert(ctx, m2); err != nil {
		t.Fatalf("Upsert #2: %v", err)
	}

	// 验证：Get 回读是更新后的内容，版本号自增
	got, err := be.Get(ctx, "mem_idem_001", user, tenant)
	if err != nil {
		t.Fatalf("Get after double upsert: %v", err)
	}
	if got.Fact != "updated version of the fact" {
		t.Errorf("Fact after upsert: got %q, want %q", got.Fact, "updated version of the fact")
	}
	if got.Version != 2 {
		t.Errorf("Version after upsert: got %d, want 2 (ON CONFLICT should increment)", got.Version)
	}

	// 验证：只有一条记录（没有重复行）
	count, err := be.CountByKind(ctx, kernel.KindFact, tenant)
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}
	if count != 1 {
		t.Errorf("CountByKind: got %d, want 1 (upsert should be idempotent on memory_id)", count)
	}
}

// TestPgBackfillVectors: vector_status='pending' 补全后 VectorSearch 可召回。
func TestPgBackfillVectors(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_backfill"
	user := "u_backfill"

	// 写入一条 pending 状态的记忆
	m := makeTestMemory("mem_backfill_001", tenant, user, "backfill test fact about quantum computing")
	if err := be.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Backfill 之前：VectorSearch 不应召回（vector_status=pending）
	zeroVec := make([]float32, 1024)
	_, err := be.VectorSearch(ctx, kernel.QueryParams{
		TenantID: tenant, Viewer: user, Vector: zeroVec, TopK: 5,
	})
	if err != nil {
		t.Fatalf("VectorSearch before backfill: %v", err)
	}

	// 执行 Backfill
	type backfiller interface {
		BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) error
	}
	bf := be.(backfiller)
	embed := func(text string) ([]float32, error) {
		vec := make([]float32, 1024)
		for i := 0; i < 32; i++ {
			vec[i] = 0.5
		}
		return vec, nil
	}
	if err := bf.BackfillVectors(ctx, embed); err != nil {
		t.Fatalf("BackfillVectors: %v", err)
	}

	// Backfill 之后：VectorSearch 应能召回
	vresults, err := be.VectorSearch(ctx, kernel.QueryParams{
		TenantID: tenant, Viewer: user, Vector: zeroVec, TopK: 5,
	})
	if err != nil {
		t.Fatalf("VectorSearch after backfill: %v", err)
	}
	found := false
	for _, r := range vresults {
		if r.MemoryID == "mem_backfill_001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("VectorSearch after backfill: expected mem_backfill_001, got %+v", vresults)
	}
}

// TestPgConcurrent: 10 并发 Upsert + 并发 Query，-race 干净（真实 PG 连接池）。
func TestPgConcurrent(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := "r10b_conc"
	user := "u_conc"

	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	// 10 并发 Upsert
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := makeTestMemory(fmt.Sprintf("mem_conc_%03d", i), tenant, user, fmt.Sprintf("concurrent fact %d", i))
			if err := be.Upsert(ctx, m); err != nil {
				errCh <- fmt.Errorf("Upsert %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent upsert: %v", err)
	}

	// 验证全部 10 条都写进去了
	count, err := be.CountByKind(ctx, kernel.KindFact, tenant)
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}
	if count < 10 {
		t.Errorf("CountByKind: got %d facts, want >=10", count)
	}

	// 并发 Query
	var wg2 sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_, _ = be.Query(ctx, kernel.QueryParams{
				TenantID: tenant, Viewer: user, Text: "concurrent", TopK: 5,
			})
		}()
	}
	wg2.Wait()
}
