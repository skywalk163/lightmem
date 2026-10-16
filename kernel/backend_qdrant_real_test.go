// backend_qdrant_real_test.go — R17 S2 Qdrant 真机联调测试（env-gated）。
//
// 背景：R16 交付的 backend_qdrant.go + httptest mock 契约测试只证明 HTTP 形状
// 正确；0.88 实机部署 Qdrant 1.19.1 后，需用真实服务端验证：
//
//   - 点 ID 合法性——真机 ExtendedPointId 只接受 u64/UUID，任意字符串 400
//     （R17 S2 已在 backend_qdrant.go 增加 qdrantPointID 确定性映射修复）；
//   - ensureCollection 真机幂等建集合（dim=1024, Cosine）；
//   - StorageBackend / LayerScanner / ComplianceBackend 全路径真实往返；
//   - VectorSearch 真实向量召回（排名正确、非降级路径）；
//   - 读己之写（wait=true 后立即可见）；
//   - Upsert 保留已有向量（fetchPoint 探针；真机实测 Qdrant 1.19 GET 单点
//     默认返回 vector，且 Cosine 集合存储归一化向量——保留判据为方向保持）。
//
// 门控口径（与 R15 门禁 [11] PG「显式 SKIP / 配置后硬阻断」一致）：
//   - LIGHTMEM_QDRANT_HOST 未配置 → 全部真机测试 t.Skip（计入 SKIP，不假 PASS）；
//   - 已配置 → 任何失败即 FAIL 阻断。
//
// 环境变量：
//
//	LIGHTMEM_QDRANT_HOST     必填（0.88 实机为 10.0.10.60）
//	LIGHTMEM_QDRANT_PORT     可选，默认 6333
//	LIGHTMEM_QDRANT_API_KEY  可选（Qdrant 未开鉴权时不需要）
//
// 只增不改：本文件为纯新增；不修改 backend_qdrant.go 与 R16 已交付的两个
// Qdrant 测试文件；stdlib-only，不引入第三方依赖、不动 go.mod。
// 数据自洽：全部用例固定 scope（tenant=r17qdreal），执行前后 DeleteByScope
// 预清/收尾，重复执行幂等，不在真机留下残留。
package kernel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"testing"
)

// ---------------------------------------------------------------------
// env 门控装配（对齐 R15 edge_persist_pg_test.go 的 loadPgEdgeTestConfig 模式）
// ---------------------------------------------------------------------

// qdrantRealTestConfig 真机连接配置（从环境变量读取）。
type qdrantRealTestConfig struct {
	host   string
	port   int
	apiKey string
}

// loadQdrantRealTestConfigOrNil 从环境变量读取配置；无 LIGHTMEM_QDRANT_HOST
// 时返回 nil（不调 t.Skip，由调用方决定 SKIP 措辞）。
func loadQdrantRealTestConfigOrNil() *qdrantRealTestConfig {
	host := os.Getenv("LIGHTMEM_QDRANT_HOST")
	if host == "" {
		return nil
	}
	cfg := &qdrantRealTestConfig{host: host, port: 6333}
	if p := os.Getenv("LIGHTMEM_QDRANT_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &cfg.port)
	}
	cfg.apiKey = os.Getenv("LIGHTMEM_QDRANT_API_KEY")
	return cfg
}

// loadQdrantRealTestConfig 从环境变量读取配置；无配置时显式 t.Skip。
func loadQdrantRealTestConfig(t *testing.T) *qdrantRealTestConfig {
	t.Helper()
	cfg := loadQdrantRealTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_QDRANT_HOST not set; skipping real Qdrant e2e (gate [11]-style explicit SKIP, not a fake PASS)")
	}
	return cfg
}

// storageConfig 装配 StorageConfig（Qdrant 段）。
func (c *qdrantRealTestConfig) storageConfig() *StorageConfig {
	return &StorageConfig{Qdrant: &QdrantConfig{
		Host:   c.host,
		Port:   c.port,
		APIKey: c.apiKey,
	}}
}

// newRealQdrantBackend 在真机 Qdrant 上构造 backend（ensureCollection 幂等）。
func newRealQdrantBackend(t *testing.T, cfg *qdrantRealTestConfig) *qdrantBackend {
	t.Helper()
	got, err := newQdrantBackend(context.Background(), cfg.storageConfig())
	if err != nil {
		t.Fatalf("newQdrantBackend (real qdrant %s:%d): %v", cfg.host, cfg.port, err)
	}
	b, ok := got.(*qdrantBackend)
	if !ok {
		t.Fatalf("newQdrantBackend returned %T, want *qdrantBackend", got)
	}
	return b
}

// qdrantRealTestVector 生成确定性 1024 维向量（简单 LCG，不依赖 math/rand
// 全局态，跨进程可复现）。分量取 [0,1)，避免零向量在 Cosine 下的退化行为。
func qdrantRealTestVector(seed int) []float32 {
	v := make([]float32, qdrantDefaultDim)
	x := uint32(seed*2654435761 + 1)
	for i := range v {
		x = x*1664525 + 1013904223
		v[i] = float32(x%1000) / 1000.0
	}
	return v
}

// ---------------------------------------------------------------------
// 门控自检（无 env 依赖，随常规测试一起跑）
// ---------------------------------------------------------------------

// TestQdrantRealConfigGate 验证 env 门控判据：未配置→nil（真机测试据此
// SKIP，绝不假 PASS）；配置后正确装配、端口缺省 6333。
func TestQdrantRealConfigGate(t *testing.T) {
	t.Setenv("LIGHTMEM_QDRANT_HOST", "")
	if cfg := loadQdrantRealTestConfigOrNil(); cfg != nil {
		t.Fatalf("expected nil config when LIGHTMEM_QDRANT_HOST empty, got %+v", cfg)
	}

	t.Setenv("LIGHTMEM_QDRANT_HOST", "10.0.10.60")
	t.Setenv("LIGHTMEM_QDRANT_PORT", "6333")
	t.Setenv("LIGHTMEM_QDRANT_API_KEY", "k-abc")
	cfg := loadQdrantRealTestConfigOrNil()
	if cfg == nil || cfg.host != "10.0.10.60" || cfg.port != 6333 || cfg.apiKey != "k-abc" {
		t.Fatalf("config mismatch: %+v", cfg)
	}

	t.Setenv("LIGHTMEM_QDRANT_PORT", "")
	cfg = loadQdrantRealTestConfigOrNil()
	if cfg == nil || cfg.port != 6333 {
		t.Fatalf("expected default port 6333, got %+v", cfg)
	}
}

// ---------------------------------------------------------------------
// 真机联调（LIGHTMEM_QDRANT_HOST 已配置才跑）
// ---------------------------------------------------------------------

// TestQdrantRealConnection 验证真机连通、ensureCollection 幂等建集合、
// 集合参数（dim=1024 / Cosine）与 R16 骨架约定一致。
func TestQdrantRealConnection(t *testing.T) {
	cfg := loadQdrantRealTestConfig(t)
	ctx := context.Background()

	// 首次构造：真机建集合（不存在则 CREATE）
	b := newRealQdrantBackend(t, cfg)
	if b.Name() != "qdrant" {
		t.Fatalf("Name() = %q, want qdrant", b.Name())
	}
	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// 集合参数核对（真机 GET /collections/lightmem_memories）。
	// 用 map 逐层下钻，兼容 Qdrant 版本的 vectors 配置两种形态：
	// 直接对象 {"size":1024,"distance":"Cosine"} 或命名向量 {"":{...}}。
	var info map[string]any
	if err := b.do(ctx, http.MethodGet, "/collections/"+b.col, nil, &info); err != nil {
		t.Fatalf("GET collection info: %v", err)
	}
	result, _ := info["result"].(map[string]any)
	if result == nil {
		t.Fatalf("collection info missing result: %v", info)
	}
	if status, _ := result["status"].(string); status == "" {
		t.Fatalf("collection status empty: %v", info)
	}
	config, _ := result["config"].(map[string]any)
	params, _ := config["params"].(map[string]any)
	vectors, _ := params["vectors"].(map[string]any)
	if vectors == nil {
		t.Fatalf("collection config missing params.vectors: %v", info)
	}
	if _, ok := vectors["size"]; !ok {
		if inner, ok2 := vectors[""].(map[string]any); ok2 {
			vectors = inner
		}
	}
	size, _ := vectors["size"].(float64)
	distance, _ := vectors["distance"].(string)
	if int(size) != qdrantDefaultDim || distance != "Cosine" {
		t.Fatalf("collection vectors config mismatch: size=%v distance=%q raw=%v",
			size, distance, vectors)
	}

	// 幂等：集合已存在时再次构造不报错（ensureCollection 不再 CREATE）
	if _, err := newQdrantBackend(ctx, cfg.storageConfig()); err != nil {
		t.Fatalf("second newQdrantBackend (ensureCollection idempotent): %v", err)
	}
}

// TestQdrantRealEndToEnd 在真机上端到端走 StorageBackend / LayerScanner /
// ComplianceBackend 主路径：Upsert→Get（读己之写）→Query→VectorSearch
// （真实向量排名）→UpdateLayer→ScanLayer→CountByKind→ExportByScope→
// DeleteByID→DeleteByScope。固定 scope，执行前后自清理，可重复执行。
func TestQdrantRealEndToEnd(t *testing.T) {
	cfg := loadQdrantRealTestConfig(t)
	ctx := context.Background()
	b := newRealQdrantBackend(t, cfg)

	// 固定 scope：预清历史残留 + 收尾清理（幂等，可重复执行）
	const tenant, viewer = "r17qdreal", "r17qdreal-u1"
	if _, err := b.DeleteByScope(ctx, tenant, viewer); err != nil {
		t.Fatalf("pre-clean DeleteByScope: %v", err)
	}
	defer func() { _, _ = b.DeleteByScope(ctx, tenant, viewer) }()

	m1 := mkMemory("r17real-m1", "客户A 偏好 简洁文档", KindPreference)
	m2 := mkMemory("r17real-m2", "客户B 偏好 长报告", KindPreference)
	m3 := mkMemory("r17real-m3", "项目C 里程碑 十月交付", KindEvent)
	for _, m := range []*Memory{m1, m2, m3} {
		m.TenantID, m.UserID = tenant, viewer
		// mkMemory（R16 httptest 助手）不设 Visibility；零值不匹配 canView
		// 任何 case → return false（R16 存量红测同根因）。fixture 显式补齐。
		m.Visibility = VisPrivate
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %s: %v", m.MemoryID, err)
		}
	}

	// Get：wait=true 读己之写，落盘即可见
	got1, err := b.Get(ctx, "r17real-m1", viewer, tenant)
	if err != nil {
		t.Fatalf("Get m1: %v", err)
	}
	if got1.Fact != m1.Fact || got1.MemoryID != "r17real-m1" {
		t.Fatalf("Get m1 mismatch: %+v", got1)
	}
	if got1.TenantID != tenant || got1.UserID != viewer {
		t.Fatalf("Get m1 scope mismatch: %+v", got1)
	}
	// 可见性：异 viewer 不可见（canView 过滤）
	if _, err := b.Get(ctx, "r17real-m1", "u_other", tenant); err == nil {
		t.Fatal("expected visibility error for foreign viewer")
	}

	// Query：文本检索（tokenize 词元重合度，与 SQLite 骨架同口径）
	res, err := b.Query(ctx, QueryParams{Text: "客户A 简洁", TenantID: tenant, Viewer: viewer, TopK: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) == 0 || res[0].MemoryID != "r17real-m1" {
		t.Fatalf("expected r17real-m1 first, got %+v", res)
	}
	if res[0].Source != "qdrant" {
		t.Fatalf("expected Source=qdrant (text path), got %q", res[0].Source)
	}

	// VectorSearch 路径一：backend Upsert 按契约只写 payload（零向量，
	// VectorStatus=pending），向量由 embedder 后续补全——此处用直接 PUT
	// 模拟 embedder 落库（与路径二同手法），再以非零查询向量检索：
	// 应返回全部 3 点且 Source 必须是 qdrant-vector（真机搜索成功，
	// 而非失败后降级文本检索）。
	for i, m := range []*Memory{m1, m2, m3} {
		pl, err := memoryPayload(m)
		if err != nil {
			t.Fatalf("memoryPayload(%s): %v", m.MemoryID, err)
		}
		fillReq := map[string]any{
			"points": []qdrantPoint{{ID: qdrantPointID(m.MemoryID), Vector: qdrantRealTestVector(100 + i), Payload: pl}},
		}
		if err := b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", fillReq, nil); err != nil {
			t.Fatalf("seed vector for %s: %v", m.MemoryID, err)
		}
	}
	vs, err := b.VectorSearch(ctx, QueryParams{
		Vector: qdrantRealTestVector(2), TenantID: tenant, Viewer: viewer, TopK: 10,
	})
	if err != nil {
		t.Fatalf("VectorSearch (embedder-filled points): %v", err)
	}
	if len(vs) != 3 {
		t.Fatalf("expected 3 vector results, got %d: %+v", len(vs), vs)
	}
	if vs[0].Source != "qdrant-vector" {
		t.Fatalf("expected Source=qdrant-vector (real search, not degraded), got %q", vs[0].Source)
	}

	// VectorSearch 路径二：预置真实向量点（模拟 embedder 产物），查询向量
	// 贴近它 → top1 必须命中（真机 Cosine 排名，非字符串匹配）。
	vecMem := mkMemory("r17real-vec1", "向量检索探针 预置向量", KindFact)
	vecMem.TenantID, vecMem.UserID = tenant, viewer
	vecMem.Visibility = VisPrivate
	payload, err := memoryPayload(vecMem)
	if err != nil {
		t.Fatalf("memoryPayload: %v", err)
	}
	vec1 := qdrantRealTestVector(1)
	seedReq := map[string]any{
		"points": []qdrantPoint{{ID: qdrantPointID("r17real-vec1"), Vector: vec1, Payload: payload}},
	}
	if err := b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", seedReq, nil); err != nil {
		t.Fatalf("seed real-vector point: %v", err)
	}
	query := append([]float32(nil), vec1...)
	query[0] += 0.01 // 轻微扰动：证明是相似度计算而非精确匹配
	vs2, err := b.VectorSearch(ctx, QueryParams{Vector: query, TenantID: tenant, Viewer: viewer, TopK: 5})
	if err != nil {
		t.Fatalf("VectorSearch (seeded vector): %v", err)
	}
	if len(vs2) == 0 || vs2[0].MemoryID != "r17real-vec1" {
		t.Fatalf("expected r17real-vec1 top1, got %+v", vs2)
	}
	if vs2[0].Source != "qdrant-vector" {
		t.Fatalf("expected Source=qdrant-vector (real search), got %q", vs2[0].Source)
	}

	// UpdateLayer（LayerScanner）
	if err := b.UpdateLayer(ctx, "r17real-m1", LayerL2); err != nil {
		t.Fatalf("UpdateLayer: %v", err)
	}
	got1b, err := b.Get(ctx, "r17real-m1", viewer, tenant)
	if err != nil {
		t.Fatalf("Get after UpdateLayer: %v", err)
	}
	if got1b.Layer != LayerL2 {
		t.Fatalf("UpdateLayer mismatch: layer=%s", got1b.Layer)
	}

	// CountByKind
	if n, err := b.CountByKind(ctx, KindPreference, tenant); err != nil || n != 2 {
		t.Fatalf("CountByKind preference: n=%d err=%v", n, err)
	}
	if n, err := b.CountByKind(ctx, KindEvent, tenant); err != nil || n != 1 {
		t.Fatalf("CountByKind event: n=%d err=%v", n, err)
	}

	// ScanLayer（LayerScanner）
	sc, err := b.ScanLayer(ctx, LayerL2, tenant, 100)
	if err != nil {
		t.Fatalf("ScanLayer: %v", err)
	}
	if len(sc) != 1 || sc[0].MemoryID != "r17real-m1" {
		t.Fatalf("ScanLayer L2 mismatch: %+v", sc)
	}

	// ExportByScope（ComplianceBackend）：m1/m2/m3 + vec1 探针 = 4
	exp, err := b.ExportByScope(ctx, tenant, viewer)
	if err != nil {
		t.Fatalf("ExportByScope: %v", err)
	}
	if len(exp) != 4 {
		t.Fatalf("ExportByScope len=%d, want 4: %+v", len(exp), exp)
	}

	// DeleteByID
	if err := b.DeleteByID(ctx, "r17real-m2"); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	if _, err := b.Get(ctx, "r17real-m2", viewer, tenant); err == nil {
		t.Fatal("expected error after DeleteByID")
	}

	// DeleteByScope（合规删除）：剩余 m1/m3/vec1 = 3
	ids, err := b.DeleteByScope(ctx, tenant, viewer)
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("DeleteByScope ids=%v, want 3", ids)
	}
	if n, _ := b.CountByKind(ctx, KindPreference, tenant); n != 0 {
		t.Fatalf("expected 0 preference after DeleteByScope, got %d", n)
	}
}

// TestQdrantRealVectorPreserve 验证 Upsert 更新已有点时保留其向量
// （真机路径）。探针实测结论（Qdrant 1.19.1 真机，2026-10）：
//   - GET 单点默认返回 vector（无需 with_vector=true），len=dim；
//   - Cosine 集合落库时归一化向量（读回 = 预置向量 / |v|，如首分量
//     0.647 → 0.03493356 ≈ 0.647/18.5）——保留判据因此为「方向保持」：
//     读回向量与预置向量平行（余弦相似度 ≈ 1），而非逐分量相等。
func TestQdrantRealVectorPreserve(t *testing.T) {
	cfg := loadQdrantRealTestConfig(t)
	ctx := context.Background()
	b := newRealQdrantBackend(t, cfg)

	const tenant, viewer = "r17qdreal", "r17qdreal-u1"
	if _, err := b.DeleteByScope(ctx, tenant, viewer); err != nil {
		t.Fatalf("pre-clean DeleteByScope: %v", err)
	}
	defer func() { _, _ = b.DeleteByScope(ctx, tenant, viewer) }()

	// 1) 预置带真实向量的点（模拟 embedder 先写向量）
	seed := mkMemory("r17real-vp1", "向量保留探针 初版", KindFact)
	seed.TenantID, seed.UserID = tenant, viewer
	seed.Visibility = VisPrivate
	payload, err := memoryPayload(seed)
	if err != nil {
		t.Fatalf("memoryPayload: %v", err)
	}
	vec := qdrantRealTestVector(7)
	seedReq := map[string]any{
		"points": []qdrantPoint{{ID: qdrantPointID("r17real-vp1"), Vector: vec, Payload: payload}},
	}
	if err := b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", seedReq, nil); err != nil {
		t.Fatalf("seed vector point: %v", err)
	}

	// 2) 经 backend Upsert 同 ID（payload 变更）：必须保留原向量
	upd := mkMemory("r17real-vp1", "向量保留探针 修订版", KindFact)
	upd.TenantID, upd.UserID = tenant, viewer
	upd.Visibility = VisPrivate
	if err := b.Upsert(ctx, upd); err != nil {
		t.Fatalf("Upsert over existing point: %v", err)
	}

	// 3) fetchPoint 回读：Cosine 集合存储归一化向量，逐分量必不等于预置值；
	//    保留判据 = 方向保持（读回向量与预置向量余弦相似度 ≈ 1）。
	pt, err := b.fetchPoint(ctx, "r17real-vp1")
	if err != nil {
		t.Fatalf("fetchPoint: %v", err)
	}
	if len(pt.Vector) != qdrantDefaultDim {
		t.Fatalf("vector not preserved: len=%d want %d", len(pt.Vector), qdrantDefaultDim)
	}
	var dot, na, nb float64
	for i := 0; i < qdrantDefaultDim; i++ {
		dot += float64(pt.Vector[i]) * float64(vec[i])
		na += float64(pt.Vector[i]) * float64(pt.Vector[i])
		nb += float64(vec[i]) * float64(vec[i])
	}
	if na == 0 || nb == 0 {
		t.Fatalf("degenerate vector: |read|²=%v |seed|²=%v", na, nb)
	}
	cos := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if cos < 1-1e-4 {
		t.Fatalf("vector direction not preserved: cosine=%v want ≈1 (read[0]=%v seed[0]=%v)",
			cos, pt.Vector[0], vec[0])
	}

	// 4) payload 已更新（Get 可见新 fact）
	got, err := b.Get(ctx, "r17real-vp1", viewer, tenant)
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if got.Fact != upd.Fact {
		t.Fatalf("fact not updated: %q", got.Fact)
	}
}
