// R27-D（L-238c，业主裁定「即时行动项 6」）：private 租户内强隔离的**防回归**用例。
//
// 业主裁定（2026-10-19，原文要点见 `_R27_并行任务书.md` §0）：
//
//	规则 1｜跨租户与不存在不可区分：跨租户访问与资源不存在必须返回同一错误类型、
//	        同一状态码、同一消息体，不得通过 403 vs 404 的差异泄漏资源归属。
//	规则 2｜private 仅对归属租户内的请求主体可见；「系统管理员」不构成跨租户读的语义依据。
//
// 本文件是**先证伪**的证据载体：A 线修复前，下面两条必红——
//
//	① `TestCanViewPrivateCrossTenant`：跨租户同名 user 能读到 private（L-238b）；
//	② `TestPrivateCrossTenantGetIndistinguishableFromNotFound`：「不可见」与「不存在」
//	   消息体不同（泄漏资源存在性，违反规则 1）。
//
// 修复后两条转绿。
//
// 声明式夹具（裁定三）：每条用例显式声明 positive_tenants / negative_tenants；
// 负样本建不出来一律 `t.Fatalf`（**绝不 t.Skip**）——负样本缺失会让跨租户断言
// 物理上不可能失败，用例就永远是绿的（裁定点名的「假过」故障模式）。
package kernel

import (
	"context"
	"fmt"
	"testing"
)

// 声明式夹具：本组用例的正样本租户（应命中）与负样本租户（应不可见）。
const (
	r27PositiveTenant = "r27-tenant-a" // positive_tenants
	r27NegativeTenant = "r27-tenant-b" // negative_tenants（必须建出数据，否则 Fatal）
	r27SameUser       = "r27-same-user"
)

// r27PrivateSeed 构造一条 private 记忆（正/负样本共用；Upsert 失败即 Fatal）。
func r27PrivateSeed(t *testing.T, b StorageBackend, id, tenant, user string) *Memory {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     VisPrivate,
		MemoryKind:     KindFact,
		Fact:           "R27 private tenant-scope probe " + id,
		Confidence:     0.9,
		Permission:     Permission{Owner: user},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		// 裁定三：夹具建不出来 → 用例报错，绝不跳过。
		t.Fatalf("夹具建不出来（裁定：负样本缺失必须 Fatal，不得 Skip）: upsert %s: %v", id, err)
	}
	// 注：SQLite 用例跑在临时库上，无需清残留；PG 用例由 pgVisSeed 自行注册 Cleanup。
	return m
}

// TestCanViewPrivateTenantScoped 规则 2 的单元级断言（canView 是 SQLite/Qdrant/PG 共用真源）。
func TestCanViewPrivateTenantScoped(t *testing.T) {
	cases := []struct {
		name       string
		memTenant  string
		memUser    string
		wantTenant string
		wantUser   string
		want       bool
	}{
		{"归属租户内 owner 可见（正样本）", r27PositiveTenant, r27SameUser, r27PositiveTenant, r27SameUser, true},
		{"跨租户同名 user 不可见（L-238b）", r27NegativeTenant, r27SameUser, r27PositiveTenant, r27SameUser, false},
		{"同租户他人不可见", r27PositiveTenant, r27SameUser, r27PositiveTenant, "r27-other-user", false},
		{"匿名 viewer 不可见", r27PositiveTenant, r27SameUser, r27PositiveTenant, "", false},
		// 防生产回归：solo 档与「未传租户上下文」下两侧 tenant 同为空 = 同一默认租户，owner 必须仍可见。
		{"solo 默认租户（两侧空）owner 仍可见", "", "u-solo", "", "u-solo", true},
	}
	for _, c := range cases {
		m := &Memory{Visibility: VisPrivate, TenantID: c.memTenant, UserID: c.memUser}
		if got := m.canView(c.wantUser, c.wantTenant); got != c.want {
			t.Errorf("%s: canView(viewer=%q, tenant=%q) = %v, want %v（memory tenant=%q user=%q）",
				c.name, c.wantUser, c.wantTenant, got, c.want, c.memTenant, c.memUser)
		}
	}
}

// TestPrivateCrossTenantGetIndistinguishableFromNotFound 规则 1 的端到端断言（SQLite 侧）：
// 跨租户读 private 必须与「读一个不存在的 id」返回**完全一致**的错误（同类型、同消息）。
func TestPrivateCrossTenantGetIndistinguishableFromNotFound(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	r27PrivateSeed(t, b, "r27-priv-b", r27NegativeTenant, r27SameUser)

	// 1) 跨租户（租户 A 的上下文读租户 B 的 private）必须失败。
	_, errCross := b.Get(ctx, "r27-priv-b", r27SameUser, r27PositiveTenant)
	if errCross == nil {
		t.Fatalf("L-238b 复发：租户 A 读到了租户 B 同名 user 的 private 记忆")
	}
	// 2) 与「不存在」完全一致（规则 1：不得泄漏资源归属）。
	//    注意：比较的是**错误模板一致性**（同为 "memory <id> not found"，无附加身份/租户信息），
	//    不是字面全等——两个 id 本就不同，字面全等是过严且无意义的断言。
	_, errMissing := b.Get(ctx, "r27-no-such-memory", r27SameUser, r27PositiveTenant)
	if errMissing == nil {
		t.Fatalf("读不存在的记忆竟然成功，用例前提失效")
	}
	wantCross := fmt.Sprintf("memory %s not found", "r27-priv-b")
	wantMissing := fmt.Sprintf("memory %s not found", "r27-no-such-memory")
	if errCross.Error() != wantCross {
		t.Fatalf("裁定规则 1 违反：跨租户错误 %q 与「不存在」模板 %q 不一致（泄漏资源存在性/归属）",
			errCross.Error(), wantCross)
	}
	if errMissing.Error() != wantMissing {
		t.Fatalf("「不存在」错误形态异常：%q，want %q", errMissing.Error(), wantMissing)
	}
	// 3) 正样本：归属租户内 owner 仍可读（不得把 private 整体打成不可见）。
	got, err := b.Get(ctx, "r27-priv-b", r27SameUser, r27NegativeTenant)
	if err != nil || got == nil {
		t.Fatalf("归属租户内 owner 应可读 private，got err=%v", err)
	}
}

// TestPrivateCrossTenantQueryLeaksNothing Query 路径：跨租户 private 不得出现在结果中。
func TestPrivateCrossTenantQueryLeaksNothing(t *testing.T) {
	b := newTestSQLiteBackend(t)
	ctx := context.Background()
	r27PrivateSeed(t, b, "r27-priv-b2", r27NegativeTenant, r27SameUser)

	res, err := b.Query(ctx, QueryParams{Text: "R27", Viewer: r27SameUser, TenantID: r27PositiveTenant, TopK: 10})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, r := range res {
		if r.MemoryID == "r27-priv-b2" {
			t.Fatalf("L-238b 复发：跨租户 private 记忆泄漏进 Query 结果")
		}
	}
}

// r27PgQueryIDs 用真实 pgBackend.Query 取回可见的 memory_id 集合。
// 说明：不复用 pgVisQueryIDs——后者按 "l225-" 前缀过滤（属它自己的用例约定），
// 会把本文件的 r227-* 种子全部滤掉，造成「负样本不可见」的假绿。
func r27PgQueryIDs(t *testing.T, b StorageBackend, viewer, tenant string) map[string]bool {
	t.Helper()
	res, err := b.Query(context.Background(), QueryParams{
		Text:     "L225", // pgVisSeed 的事实文本前缀
		Viewer:   viewer,
		TenantID: tenant,
		TopK:     50,
	})
	if err != nil {
		t.Fatalf("query(viewer=%s,tenant=%s): %v", viewer, tenant, err)
	}
	out := make(map[string]bool)
	for _, r := range res {
		out[r.MemoryID] = true
	}
	return out
}

// TestPGPrivateCrossTenantInvisible PG 真机侧（L-238b 的另一半：`pgVisibilityPredicate`
// 与 `canView` 必须逐条对齐；R21-B 的 G8 教训是「只改一侧 = 假绿」）。
func TestPGPrivateCrossTenantInvisible(t *testing.T) {
	cfg := loadPgVisConfig()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST not set; skipping real-PG private tenant-scope test")
	}
	b := newPgVisBackend(t, cfg)

	// 负样本：租户 B 的 private（同 user），必须建出来（否则 pgVisSeed 内部 Fatal）。
	negID := pgVisSeed(t, b, "r227-priv-b", VisPrivate, r27NegativeTenant, r27SameUser, nil, nil)
	// 正样本：租户 A 的同 user private，用于证明「不是把 private 整体打瞎」。
	posID := pgVisSeed(t, b, "r227-priv-a", VisPrivate, r27PositiveTenant, r27SameUser, nil, nil)

	seen := r27PgQueryIDs(t, b, r27SameUser, r27PositiveTenant)
	if seen[negID] {
		t.Fatalf("L-238b（PG 侧）复发：跨租户同 user 的 private 记忆 %s 对租户 A 可见", negID)
	}
	if !seen[posID] {
		t.Fatalf("正样本断裂：归属租户 A 的 private 记忆 %s 应对 owner 可见；got %v", posID, seen)
	}
	t.Logf("R27 OK: PG 侧 private 已按租户收敛（负样本 %s 不可见 / 正样本 %s 可见，db=%s）",
		negID, posID, cfg.dbname)
}
