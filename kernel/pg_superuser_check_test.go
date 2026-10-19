// R28-A（L-240 / 业主裁定二 2026-10-20）· superuser 启动自检用例。
//
// 判绿口径：
//   - 纯逻辑用例（策略解析 / 判定矩阵）无需真机，恒跑；
//   - 真机用例跟随既有 PG env 惯例（LIGHTMEM_PG_HOST + TEST_DB），无 env 则 Skip；
//     有 env 时**断言当前连接角色非 superuser**——这本身就是一道门禁：
//     谁用 superuser 连接跑 CI，谁就红（裁定二「安全底线，不是可选项」）。
package kernel

import (
	"context"
	"errors"
	"testing"
)

func TestParsePgSuperuserPolicy(t *testing.T) {
	cases := []struct {
		env  string
		want pgSuperuserPolicy
	}{
		{"", pgSuperFail},                    // 未设置 → 安全默认 fail
		{"fail", pgSuperFail},                //
		{"FAIL", pgSuperFail},                // 大小写不敏感
		{"  fail  ", pgSuperFail},            // 空白容忍
		{"warn", pgSuperWarn},                //
		{"warn_only", pgSuperWarn},           //
		{"off", pgSuperOff},                  //
		{"0", pgSuperOff},                    //
		{"false", pgSuperOff},                //
		{"disable", pgSuperOff},              //
		{"bogus-unknown-value", pgSuperFail}, // 无法识别 → 绝不静默降级为放行
	}
	for _, c := range cases {
		t.Setenv("LIMS_PG_SUPERUSER_CHECK", c.env)
		if got := parsePgSuperuserPolicy(); got != c.want {
			t.Errorf("parsePgSuperuserPolicy(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestEnforcePgSuperuserMatrix(t *testing.T) {
	checkErr := errors.New("boom")
	cases := []struct {
		name    string
		pol     pgSuperuserPolicy
		isSuper bool
		err     error
		wantErr bool
	}{
		{"fail+superuser→阻断", pgSuperFail, true, nil, true},
		{"fail+非superuser→放行", pgSuperFail, false, nil, false},
		{"fail+自检失败→fail-closed阻断", pgSuperFail, false, checkErr, true},
		{"warn+superuser→只告警", pgSuperWarn, true, nil, false},
		{"warn+自检失败→放行", pgSuperWarn, false, checkErr, false},
		{"off+superuser→跳过", pgSuperOff, true, nil, false},
		{"off+自检失败→跳过", pgSuperOff, true, checkErr, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := enforcePgSuperuser(c.pol, c.isSuper, c.err)
			if (err != nil) != c.wantErr {
				t.Fatalf("enforcePgSuperuser(%v, super=%v, err=%v) = %v, wantErr=%v",
					c.pol, c.isSuper, c.err, err, c.wantErr)
			}
			if c.wantErr && err != nil && !containsAny(err.Error(), "superuser", "fail-closed") {
				t.Errorf("错误信息应含可操作指引，实际：%v", err)
			}
		})
	}
}

// containsAny 报告 s 是否包含任一子串（避免为此引入新依赖）。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// TestPgSuperuserCheckRealPG：真机自检——**断言 CI/真机连接角色不是 superuser**。
func TestPgSuperuserCheckRealPG(t *testing.T) {
	cfg := loadPgEdgeTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping real PG superuser check")
	}
	db := openPgEdgeDB(t, loadPgEdgeTestConfig(t))
	isSuper, err := CheckPgSuperuser(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckPgSuperuser 真机查询失败：%v", err)
	}
	if isSuper {
		t.Fatalf("真机连接角色 %q 是 superuser —— 违反业主裁定二："+
			"RLS / pg_read_all_data 可被绕过，多租户隔离形同虚设。"+
			"请用 deploy/init-roles.sql 建 lightmem_app 并切换 LIGHTMEM_PG_USER / lightmem_pg_user",
			cfg.user)
	}
}

// TestEnforcePgSuperuserPolicyRealPG：默认策略（fail）下，真机连接必须能放行。
func TestEnforcePgSuperuserPolicyRealPG(t *testing.T) {
	cfg := loadPgEdgeTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping real PG superuser policy check")
	}
	db := openPgEdgeDB(t, loadPgEdgeTestConfig(t))
	ctx := context.Background()

	isSuper, err := CheckPgSuperuser(ctx, db)
	if err != nil {
		t.Fatalf("CheckPgSuperuser 真机查询失败：%v", err)
	}
	if isSuper {
		t.Fatalf("真机连接角色 %q 是 superuser，默认 fail 策略下服务将拒绝启动", cfg.user)
	}

	// 默认（未设置 env）→ fail 策略：非 superuser 必须放行。
	t.Setenv("LIMS_PG_SUPERUSER_CHECK", "")
	if err := EnforcePgSuperuserPolicy(ctx, db); err != nil {
		t.Fatalf("非 superuser 连接在默认 fail 策略下不应被阻断，实际：%v", err)
	}
	// warn 策略：同样放行。
	t.Setenv("LIMS_PG_SUPERUSER_CHECK", "warn")
	if err := EnforcePgSuperuserPolicy(ctx, db); err != nil {
		t.Fatalf("非 superuser 连接在 warn 策略下不应被阻断，实际：%v", err)
	}
	// off 策略：放行。
	t.Setenv("LIMS_PG_SUPERUSER_CHECK", "off")
	if err := EnforcePgSuperuserPolicy(ctx, db); err != nil {
		t.Fatalf("off 策略下不应被阻断，实际：%v", err)
	}
}
