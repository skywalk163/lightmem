// R28-C1 · 可见性内存索引一致性用例。
//
// 索引的**全部风险**在于「与 DB 不一致会导致可见性误判」（安全问题），
// 故本文件的核心是：索引路径与 SQL 路径结果必须逐位一致，且写入/删除后立即可见。
package kernel

import (
	"context"
	"testing"
)

func newVisIdxTestBackend(t *testing.T) *sqliteBackend {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b.(*sqliteBackend)
}

func visIdxSeed(t *testing.T, b *sqliteBackend) {
	t.Helper()
	ctx := context.Background()
	seeds := []*Memory{
		{MemoryID: "m_priv_t1u1", TenantID: "t1", UserID: "u1", Visibility: VisPrivate, Fact: "private of u1"},
		{MemoryID: "m_priv_t2u1", TenantID: "t2", UserID: "u1", Visibility: VisPrivate, Fact: "private of u1 in t2"},
		{MemoryID: "m_tenant_t1", TenantID: "t1", UserID: "u2", Visibility: VisTenant, Fact: "tenant visible"},
		{MemoryID: "m_public", TenantID: "t9", UserID: "u9", Visibility: VisPublic, Fact: "public note"},
	}
	for _, m := range seeds {
		if err := b.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert %s: %v", m.MemoryID, err)
		}
	}
}

// TestVisIndexMatchesSQLPath：索引路径与 SQL 路径在多种 (viewer, tenant) 下逐位一致。
func TestVisIndexMatchesSQLPath(t *testing.T) {
	ctx := context.Background()

	bOn := newVisIdxTestBackend(t)
	bOff := newVisIdxTestBackend(t)
	visIdxSeed(t, bOn)
	visIdxSeed(t, bOff)

	// 两个 backend 分别固定开关（避免同一实例中途切换导致索引陈旧——
	// 生产不会动态切换，这里是测试隔离）。
	t.Setenv("LIMS_VIS_INDEX", "1")
	onEnabled := visIndexEnabled()
	t.Setenv("LIMS_VIS_INDEX", "0")
	offEnabled := visIndexEnabled()
	if !onEnabled || offEnabled {
		t.Fatalf("开关解析异常：on=%v off=%v", onEnabled, offEnabled)
	}

	cases := []struct{ viewer, tenant string }{
		{"u1", "t1"}, // private 本人 + tenant 可见
		{"u2", "t1"}, // 异用户：不得见 t1 的 private；跨租户 private 亦不得见
		{"u1", "t2"}, // 同名 user 在另一租户
		{"", "t1"},   // 匿名
		{"u9", "t9"}, // public 来源租户
	}

	for _, c := range cases {
		t.Setenv("LIMS_VIS_INDEX", "1")
		gotOn, err := bOn.visibleMemoryIDs(ctx, c.viewer, c.tenant)
		if err != nil {
			t.Fatalf("索引路径 (%s,%s): %v", c.viewer, c.tenant, err)
		}
		t.Setenv("LIMS_VIS_INDEX", "0")
		gotOff, err := bOff.visibleMemoryIDs(ctx, c.viewer, c.tenant)
		if err != nil {
			t.Fatalf("SQL 路径 (%s,%s): %v", c.viewer, c.tenant, err)
		}
		if len(gotOn) != len(gotOff) {
			t.Fatalf("(%s,%s) 索引路径与 SQL 路径不一致：idx=%v sql=%v",
				c.viewer, c.tenant, gotOn, gotOff)
		}
		for id := range gotOff {
			if !gotOn[id] {
				t.Fatalf("(%s,%s) 索引路径漏了 %s（SQL 路径可见）", c.viewer, c.tenant, id)
			}
		}
		for id := range gotOn {
			if !gotOff[id] {
				t.Fatalf("(%s,%s) 索引路径多出 %s（SQL 路径不可见 → 越权风险）",
					c.viewer, c.tenant, id)
			}
		}
	}
}

// TestVisIndexReflectsWrites：写入 / 删除后**立即**查索引，必须反映最新状态。
func TestVisIndexReflectsWrites(t *testing.T) {
	t.Setenv("LIMS_VIS_INDEX", "1")
	ctx := context.Background()
	b := newVisIdxTestBackend(t)

	// 1) 写入后立即可见
	m := &Memory{MemoryID: "m_new", TenantID: "t1", UserID: "u1", Visibility: VisPrivate, Fact: "new"}
	if err := b.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	set, err := b.visibleMemoryIDs(ctx, "u1", "t1")
	if err != nil {
		t.Fatalf("visibleMemoryIDs: %v", err)
	}
	if !set["m_new"] {
		t.Fatalf("Upsert 后索引未包含 m_new（写入未同步 → 会漏召回）")
	}

	// 2) 删除后立即不可见
	if err := b.DeleteByID(ctx, "m_new"); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	set, err = b.visibleMemoryIDs(ctx, "u1", "t1")
	if err != nil {
		t.Fatalf("visibleMemoryIDs: %v", err)
	}
	if set["m_new"] {
		t.Fatalf("DeleteByID 后索引仍含 m_new（删除未同步 → 可见性失效风险）")
	}

	// 3) DeleteByScope 批量删除同步
	m2 := &Memory{MemoryID: "m_scope", TenantID: "t5", UserID: "u5", Visibility: VisPrivate, Fact: "scope"}
	if err := b.Upsert(ctx, m2); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := b.DeleteByScope(ctx, "t5", "u5"); err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	set, err = b.visibleMemoryIDs(ctx, "u5", "t5")
	if err != nil {
		t.Fatalf("visibleMemoryIDs: %v", err)
	}
	if set["m_scope"] {
		t.Fatalf("DeleteByScope 后索引仍含 m_scope")
	}
}

// TestVisIndexStatsSanity：索引只构建一次（增量维护生效，不每次重建）。
func TestVisIndexStatsSanity(t *testing.T) {
	t.Setenv("LIMS_VIS_INDEX", "1")
	ctx := context.Background()
	b := newVisIdxTestBackend(t)
	visIdxSeed(t, b)

	if _, err := b.visibleMemoryIDs(ctx, "u1", "t1"); err != nil {
		t.Fatalf("1st: %v", err)
	}
	builds1, _ := b.visIdx.stats()
	if builds1 != 1 {
		t.Fatalf("首次查询应构建 1 次，实际 %d", builds1)
	}
	for i := 0; i < 3; i++ {
		if _, err := b.visibleMemoryIDs(ctx, "u1", "t1"); err != nil {
			t.Fatalf("repeat: %v", err)
		}
	}
	builds2, size := b.visIdx.stats()
	if builds2 != 1 {
		t.Fatalf("后续查询不应重建索引（否则性能收益归零），实际构建 %d 次", builds2)
	}
	if size != 4 {
		t.Fatalf("索引条目数应为 4，实际 %d", size)
	}
}
