package kernel

// L-261 交付测试（业主裁定 2026-10-21，选项三）：
//   B2 启动自检对 PG 档**默认强制**，同时保留显式逃生开关 `LIMS_BOOT_SELFCHECK`。
//
// 本文件只测**纯判定逻辑**，不依赖真机 PG：
//   1. 开关解析矩阵（默认 / 各种关闭写法 / 各种开启写法 / 无法识别的值）；
//   2. **反式**：不设开关时必须 enabled —— 这是「安全底线不可被静默降级」的锁；
//   3. PG 分支在开关关闭时**不阻断**且把 BootReport 标为 skipped(env)（可观测）；
//   4. sqlite 档**不受**该开关影响（避免把逃生开关扩成全局后门）。
//
// 说明：PG 分支的完整断言需要真 PG 连接，属 pgBootCheck 既有真机用例（skip 逻辑），
// 本文件不重复造轮子；这里验证的是「开关是否被正确读取与分支」。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // 仅用于构造不可连接的 *sql.DB，不建实际连接
)

// TestBootSelfCheckEnabledMatrix 开关解析矩阵。
func TestBootSelfCheckEnabledMatrix(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"未设置 → 默认强制", "", true},
		{"0 → 关闭", "0", false},
		{"off → 关闭", "off", false},
		{"false → 关闭", "false", false},
		{"disable → 关闭", "disable", false},
		{"disabled → 关闭", "disabled", false},
		{"skip → 关闭", "skip", false},
		{"OFF 大写 → 关闭", "OFF", false},
		{"  off  带空白 → 关闭", "  off  ", false},
		{"1 → 开启", "1", true},
		{"on → 开启", "on", true},
		{"true → 开启", "true", true},
		{"无法识别的值 → 开启（不猜意图）", "maybe", true},
		{"yes → 开启（未列入关闭白名单）", "yes", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// t.Setenv 会自动在用例结束时还原，避免污染其它用例。
			t.Setenv("LIMS_BOOT_SELFCHECK", c.env)
			if got := bootSelfCheckEnabled(); got != c.want {
				t.Fatalf("bootSelfCheckEnabled() = %v (env=%q)，期望 %v", got, c.env, c.want)
			}
		})
	}
}

// TestBootSelfCheckDefaultIsEnforced 反式：安全底线不得被静默降级。
//
// 若有人为了「让 CI 好过」把默认值改成 false，本用例立刻变红。
func TestBootSelfCheckDefaultIsEnforced(t *testing.T) {
	// 显式清空，模拟「运维什么都没配」的真实默认态。
	t.Setenv("LIMS_BOOT_SELFCHECK", "")
	if !bootSelfCheckEnabled() {
		t.Fatal("未设置 LIMS_BOOT_SELFCHECK 时必须启用自检（安全默认）；" +
			"若确需放行请显式设 LIMS_BOOT_SELFCHECK=off 并接受日志告警")
	}
}

// TestRunBootSchemaCheckPgEscapesWhenDisabled 关闭开关时，PG 分支不得阻断，
// 且必须在 BootReport 上留下可观测痕迹（skipped(env)），防止关掉了却没人知道。
//
// 实现要点：必须先让 b.db != nil 才能穿过 nil db 守卫，否则用例会在守卫处
// 返回 nil，**根本触不到开关分支**——那样即使开关写错也照样绿，是假绿。
// 这里用不可连接的 *sql.DB（sql.Open 不做网络 IO）。
func TestRunBootSchemaCheckPgEscapesWhenDisabled(t *testing.T) {
	t.Setenv("LIMS_BOOT_SELFCHECK", "off")

	db, err := sql.Open("pgx", "host=127.0.0.1 port=1 user=none dbname=none sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open(pgx) failed: %v", err)
	}
	defer db.Close()

	rep := &BootReport{}
	if err := runBootSchemaCheck(context.Background(), &pgBackend{db: db}, rep); err != nil {
		t.Fatalf("开关关闭时 PG 分支不应阻断启动，实际 err=%v "+
			"(若为连接错误，说明判定没生效、代码走到了 pgBootCheck)", err)
	}
	if rep.Table != "skipped(env)" || rep.Columns != "skipped(env)" {
		t.Fatalf("开关关闭时必须标记 skipped(env) 以便可观测，实际 table=%q columns=%q",
			rep.Table, rep.Columns)
	}
}

// TestRunBootSchemaCheckPgEnforcedByDefault 反式：默认态下 PG 分支必须真的去自检。
//
// 用不可连接的 DB：若自检被执行，pgBootCheck 会因查询失败返回 error；
// 若返回 nil，说明自检被静默跳过 —— 这正是要防的回归。
func TestRunBootSchemaCheckPgEnforcedByDefault(t *testing.T) {
	t.Setenv("LIMS_BOOT_SELFCHECK", "")

	db, err := sql.Open("pgx", "host=127.0.0.1 port=1 user=none dbname=none sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open(pgx) failed: %v", err)
	}
	defer db.Close()

	rep := &BootReport{}
	if err := runBootSchemaCheck(context.Background(), &pgBackend{db: db}, rep); err == nil {
		t.Fatal("默认态下 PG 自检必须执行；返回 nil 说明自检被静默跳过（安全底线被降级）")
	}
	if rep.Table == "skipped(env)" {
		t.Fatal("默认态不应出现 skipped(env) 标记")
	}
}

// TestSqliteUnaffectedByBootSelfCheckSwitch sqlite 档不受逃生开关影响。
//
// 理由：sqlite 表由 newSQLiteBackend 自建，不存在「未迁移的库」问题，
// 因此不该给它开后门——否则开关一旦被误设，sqlite 档也失去表结构漂移保护。
func TestSqliteUnaffectedByBootSelfCheckSwitch(t *testing.T) {
	t.Setenv("LIMS_BOOT_SELFCHECK", "off")

	b := newTestSQLiteBackend(t)
	rep := &BootReport{}
	if err := runBootSchemaCheck(context.Background(), b, rep); err != nil {
		t.Fatalf("sqlite 自检不应受 LIMS_BOOT_SELFCHECK 影响，实际 err=%v", err)
	}
	// sqlite 自检确实执行了：不会留下 skipped(env) 标记。
	if rep.Table == "skipped(env)" {
		t.Fatal("sqlite 档被逃生开关误伤：自检被跳过（开关只应作用于 pg 档）")
	}
	if rep.Table != "ok" {
		t.Fatalf("sqlite 自检应正常完成并标记 ok，实际 table=%q", rep.Table)
	}
}
