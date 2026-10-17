// R21-B：PG 后端可见性真机测试（G8 清零，P0 越权读）。
//
// 背景（E 线 0.88 实测复现）：PG 的 SQL 可见性谓词与规范 canView 语义分叉——
// 旧谓词把 q.Viewer（用户名）当 visibility 枚举比较（恒假），且 tenant_id 分支
// 无差别放行 private 行 → 任何同租户 viewer（含**不传 viewer**）都能搜到他人
// private 记忆。SQLite/Qdrant 走 canView 本就正确，仅 PG 分叉；而既有单测
// TestSearchViewerPrivateIsolation 跑在 SQLite 上，永远测不到此分叉（绿灯假象）。
//
// 本文件是 PG 专用（env-gated，复用 pg_e2e_test.go 的 PG 基础设施），必须在
// 0.88 真机跑（任务书 §五 B 线第 4 节）。覆盖五条必测路径：
//
//	① 不传 viewer        → private 命中数 = 0（反漏网断言，显式）
//	② 异用户 viewer      → private 命中数 = 0
//	③ 同租户 tenant 级   → 同租户任意 viewer 可见
//	④ owner 本人        → private 可见
//	⑤ 跨租户            → 不可见（除显式 cross_tenant 共享）
//
// 另覆盖 shared_user（readers）与 cross_tenant（shared_with_tenants）两条
// 正向分支，以及 VectorSearch 的同一谓词（G8 两处，Query + VectorSearch）。
//
// 红线遵守：只改测试，不动 Schema/SQL/migration；env 缺省时全部 t.Skip；
// 夹具用 uniqueTenant/uniqueID 进程级隔离（R18-A1 口径）。
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/lightmem/kernel"
)

// pgTestDSN 由 pgTestConfig 拼 pgx DSN（与 pg_e2e_test.go 的 ensurePgSchema 同口径；
// pgx stdlib 驱动由同包 pg_e2e_test.go 的 blank import 注册）。
func pgTestDSN(cfg *pgTestConfig) string {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
		cfg.host, cfg.port, cfg.user, cfg.dbname)
	if cfg.password != "" {
		dsn += " password=" + cfg.password
	}
	return dsn
}

// seedCrossTenant 造一条 cross_tenant 记忆并补写 shared_with_tenants 数组列。
//
// 为什么要直连 SQL 补写：PG 后端的 Upsert 对 SharedWithTenants 有**既有缺陷**
// （R20 基线复现，与本轮 G8 修复无关）——`stringArrFromJSON` 把 JSON 数组串
// （`["a"]`）直接当 text[] 字面量传参 → `malformed array literal`（SQLSTATE 22P02），
// 任何带 SharedWithTenants 的记忆在 PG 上都写不进去。该缺陷登记在 B 线交付报告
// （建议列入缺陷账；修复点在 Upsert 的参数编码，不在可见性谓词，故本轮不动）。
// 这里其余字段仍走 Upsert 正规路径，仅数组列用 $1::text[] 显式转型补写，
// 以保证 cross_tenant 谓词分支有真机覆盖。
func seedCrossTenant(t *testing.T, cfg *pgTestConfig, be kernel.StorageBackend, m *kernel.Memory, sharedWith []string) {
	t.Helper()
	if err := be.Upsert(context.Background(), m); err != nil {
		t.Fatalf("Upsert %s: %v", m.MemoryID, err)
	}
	db, err := sql.Open("pgx", pgTestDSN(cfg))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	arr := "{" + strings.Join(sharedWith, ",") + "}"
	if _, err := db.Exec(`UPDATE memories SET shared_with_tenants = $1::text[] WHERE memory_id = $2`,
		arr, m.MemoryID); err != nil {
		t.Fatalf("patch shared_with_tenants for %s: %v", m.MemoryID, err)
	}
}

// visHits 返回 results 中 Fact 含 substr 的 memory_id 列表（大小写不敏感）。
func visHits(results []kernel.QueryResult, substr string) []string {
	var ids []string
	needle := strings.ToLower(substr)
	for _, r := range results {
		if strings.Contains(strings.ToLower(r.Fact), needle) {
			ids = append(ids, r.MemoryID)
		}
	}
	return ids
}

// sameIDSet 判断两个 id 集合是否逐项相同（顺序无关）。
func sameIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[string]int, len(want))
	for _, w := range want {
		m[w]++
	}
	for _, g := range got {
		m[g]--
		if m[g] < 0 {
			return false
		}
	}
	for _, c := range m {
		if c != 0 {
			return false
		}
	}
	return true
}

// visMemory 在给定 tenant/user 下造一条指定可见性的记忆。
func visMemory(id, tenant, user string, vis kernel.Visibility, fact string) *kernel.Memory {
	m := makeTestMemory(id, tenant, user, fact)
	m.Visibility = vis
	return m
}

// TestPgVisibilityMatrix_G8Regression 是 G8 的主回归用例（真机）。
//
// 同一租户内造四种可见性记忆，按七种 (viewer, tenant) 组合查询，
// 期望集合**逐条对齐 canView 规范语义**（SQLite 参考实现）：
//
//	viewer=""          tenant=T   → {tenant}
//	viewer=异用户       tenant=T   → {tenant}            ← ① ② private 不泄露（G8 核心）
//	viewer=owner       tenant=T   → {private, tenant}    ← ④ owner 本人可见
//	viewer=reader      tenant=T   → {tenant, shared}     ← shared_user readers 分支
//	viewer=sharedOwner tenant=T   → {tenant, shared}     ← shared_user owner 分支
//	viewer=""          tenant=B   → {cross}              ← cross_tenant 定向共享
//	viewer=异用户       tenant=C   → {}                   ← ⑤ 跨租户不可见
func TestPgVisibilityMatrix_G8Regression(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := uniqueTenant("g8matrix")
	tenantB := uniqueTenant("g8matrix_b")
	tenantC := uniqueTenant("g8matrix_c")
	tag := "g8matrix-probe-" + tenant // 查询令牌：只用一次，全库唯一

	owner := "u_owner_a"
	stranger := "u_stranger_b" // 同租户异用户
	reader := "u_reader_c"
	sharedOwner := "u_shared_owner_d"
	writer := "u_writer_e"

	mPriv := visMemory(uniqueID("mem_g8", "priv"), tenant, owner, kernel.VisPrivate, tag+" private secret")
	mTenant := visMemory(uniqueID("mem_g8", "tenant"), tenant, writer, kernel.VisTenant, tag+" tenant shared note")
	mShared := visMemory(uniqueID("mem_g8", "shared"), tenant, sharedOwner, kernel.VisSharedUser, tag+" shared_user note")
	mShared.Permission = kernel.Permission{Owner: sharedOwner, Readers: []string{reader}}
	mCross := visMemory(uniqueID("mem_g8", "cross"), tenant, writer, kernel.VisCrossTenant, tag+" cross tenant note")

	for _, m := range []*kernel.Memory{mPriv, mTenant, mShared} {
		if err := be.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %s: %v", m.MemoryID, err)
		}
	}
	// cross_tenant 的 shared_with_tenants 数组列走直连 SQL 补写（既有 Upsert 缺陷，见 seedCrossTenant 注释）
	seedCrossTenant(t, cfg, be, mCross, []string{tenantB})

	cases := []struct {
		name   string
		viewer string
		tenant string
		want   []string
	}{
		// ① 反漏网断言：不传 viewer 时，private 命中数必须为 0
		{"anonymous same tenant", "", tenant, []string{mTenant.MemoryID}},
		// ② 异用户 viewer：private 不得泄露（G8 直接复现路径）
		{"stranger same tenant", stranger, tenant, []string{mTenant.MemoryID}},
		// ④ owner 本人：private + tenant 可见
		{"owner same tenant", owner, tenant, []string{mPriv.MemoryID, mTenant.MemoryID}},
		// shared_user：readers 列表内的用户可见（private 仍不可见）
		{"reader same tenant", reader, tenant, []string{mTenant.MemoryID, mShared.MemoryID}},
		// shared_user：owner 本人可见
		{"shared owner same tenant", sharedOwner, tenant, []string{mTenant.MemoryID, mShared.MemoryID}},
		// cross_tenant：显式共享的目标租户可见（private 的 owner 不在场，不泄露）
		{"anonymous shared tenant B", "", tenantB, []string{mCross.MemoryID}},
		// ⑤ 跨租户：未共享的租户什么都看不到
		{"stranger unrelated tenant C", stranger, tenantC, nil},
	}

	for _, c := range cases {
		res, err := be.Query(ctx, kernel.QueryParams{
			TenantID: c.tenant,
			Viewer:   c.viewer,
			Text:     tag,
			TopK:     50,
		})
		if err != nil {
			t.Fatalf("%s: Query: %v", c.name, err)
		}
		got := visHits(res, tag)
		if !sameIDSet(got, c.want) {
			t.Errorf("%s (viewer=%q tenant=%q): got %v, want %v — 可见性语义与 canView 分叉（G8 未清零）",
				c.name, c.viewer, c.tenant, got, c.want)
		}
	}
}

// TestPgVisibilityVectorSearchPrivateIsolation：VectorSearch 同一谓词（G8 两处之一）。
// 向量路径同样覆盖「不传 viewer」与「异用户 viewer」两条越权路径。
func TestPgVisibilityVectorSearchPrivateIsolation(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := uniqueTenant("g8vec")
	tag := "g8vec-probe-" + tenant
	owner := "u_vec_owner"
	stranger := "u_vec_stranger"

	mPriv := visMemory(uniqueID("mem_g8vec", "priv"), tenant, owner, kernel.VisPrivate, tag+" private vector secret")
	mTenant := visMemory(uniqueID("mem_g8vec", "tenant"), tenant, owner, kernel.VisTenant, tag+" tenant vector note")
	for _, m := range []*kernel.Memory{mPriv, mTenant} {
		if err := be.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %s: %v", m.MemoryID, err)
		}
	}

	// 补全向量（确定性 embedder，前 32 维 0.5；复用 pg_e2e_test.go 的口径）。
	// 库内可能堆积历史 pending 行（BackfillVectors 每轮 LIMIT 100）→ 轮询补全，
	// 直到 owner 视角能向量召回本条（确定性收敛，不依赖库的干净程度）。
	type backfiller interface {
		BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) error
	}
	bf := be.(backfiller)
	embed := func(string) ([]float32, error) {
		vec := make([]float32, 1024)
		for i := 0; i < 32; i++ {
			vec[i] = 0.5
		}
		return vec, nil
	}
	queryVec, _ := embed(tag)
	vectorized := false
	for i := 0; i < 10 && !vectorized; i++ {
		if err := bf.BackfillVectors(ctx, embed); err != nil {
			t.Fatalf("BackfillVectors: %v", err)
		}
		res, err := be.VectorSearch(ctx, kernel.QueryParams{
			TenantID: tenant, Viewer: owner, Vector: queryVec, TopK: 50,
		})
		if err != nil {
			t.Fatalf("VectorSearch (backfill poll): %v", err)
		}
		for _, id := range visHits(res, tag) {
			if id == mPriv.MemoryID {
				vectorized = true
				break
			}
		}
	}
	if !vectorized {
		t.Fatalf("backfill did not vectorize %s after 10 rounds", mPriv.MemoryID)
	}

	cases := []struct {
		name   string
		viewer string
		want   []string
	}{
		{"anonymous", "", []string{mTenant.MemoryID}},                // ① 不传 viewer：private = 0
		{"stranger", stranger, []string{mTenant.MemoryID}},           // ② 异用户：private = 0
		{"owner", owner, []string{mPriv.MemoryID, mTenant.MemoryID}}, // ④ owner：private 可见
	}
	for _, c := range cases {
		res, err := be.VectorSearch(ctx, kernel.QueryParams{
			TenantID: tenant,
			Viewer:   c.viewer,
			Vector:   queryVec,
			TopK:     50,
		})
		if err != nil {
			t.Fatalf("%s: VectorSearch: %v", c.name, err)
		}
		got := visHits(res, tag)
		if !sameIDSet(got, c.want) {
			t.Errorf("%s (viewer=%q): got %v, want %v — VectorSearch 谓词未对齐 canView",
				c.name, c.viewer, got, c.want)
		}
	}
}

// TestPgVisibilityCrossTenantSharedWith：cross_tenant 的定向共享语义。
// shared_with_tenants 含目标租户 → 可见；不含 → 不可见（含 private owner 不在场时）。
func TestPgVisibilityCrossTenantSharedWith(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := uniqueTenant("g8cross")
	sharedTo := uniqueTenant("g8cross_to")
	unrelated := uniqueTenant("g8cross_no")
	tag := "g8cross-probe-" + tenant

	m := visMemory(uniqueID("mem_g8cross", "001"), tenant, "u_cross_owner",
		kernel.VisCrossTenant, tag+" cross tenant shared payload")
	// shared_with_tenants 数组列走直连 SQL 补写（既有 Upsert 缺陷，见 seedCrossTenant 注释）
	seedCrossTenant(t, cfg, be, m, []string{sharedTo})

	// 共享目标租户：可见
	res, err := be.Query(ctx, kernel.QueryParams{TenantID: sharedTo, Viewer: "u_any", Text: tag, TopK: 10})
	if err != nil {
		t.Fatalf("Query shared tenant: %v", err)
	}
	if got := visHits(res, tag); !sameIDSet(got, []string{m.MemoryID}) {
		t.Errorf("shared tenant: got %v, want [%s]", got, m.MemoryID)
	}

	// 未共享租户：不可见
	res2, err := be.Query(ctx, kernel.QueryParams{TenantID: unrelated, Viewer: "u_any", Text: tag, TopK: 10})
	if err != nil {
		t.Fatalf("Query unrelated tenant: %v", err)
	}
	if got := visHits(res2, tag); len(got) != 0 {
		t.Errorf("unrelated tenant: got %v, want [] (cross-tenant leak)", got)
	}
}

// TestPgVisibilitySharedUserAnonymousIsolation：shared_user 对匿名/陌生人不可见，
// 仅 owner 与 readers 列表可见（对齐 canView 的 shared_user 分支）。
func TestPgVisibilitySharedUserAnonymousIsolation(t *testing.T) {
	cfg := loadPgTestConfig(t)
	be := newPgTestBackend(t, cfg)
	ctx := context.Background()

	tenant := uniqueTenant("g8shared")
	tag := "g8shared-probe-" + tenant
	owner := "u_sh_owner"
	reader := "u_sh_reader"
	stranger := "u_sh_stranger"

	m := visMemory(uniqueID("mem_g8shared", "001"), tenant, owner,
		kernel.VisSharedUser, tag+" shared user only payload")
	m.Permission = kernel.Permission{Owner: owner, Readers: []string{reader}}
	if err := be.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	for _, c := range []struct {
		name   string
		viewer string
		want   int
	}{
		{"anonymous", "", 0},
		{"stranger", stranger, 0},
		{"owner", owner, 1},
		{"reader", reader, 1},
	} {
		res, err := be.Query(ctx, kernel.QueryParams{TenantID: tenant, Viewer: c.viewer, Text: tag, TopK: 10})
		if err != nil {
			t.Fatalf("%s: Query: %v", c.name, err)
		}
		if got := len(visHits(res, tag)); got != c.want {
			t.Errorf("%s (viewer=%q): got %d hits, want %d", c.name, c.viewer, got, c.want)
		}
	}
}
