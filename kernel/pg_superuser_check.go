// PG superuser 启动自检（R28-A / L-240，业主裁定二 2026-10-20）。
//
// 背景（业主裁定原文要点）：
//   - superuser 连接会绕过 RLS、pg_read_all_data 等数据库层约束，
//     "多租户隔离形同虚设"；"非 superuser 断言必须做，这是安全底线，不是可选项"。
//   - 但此事**不需要动 migrations**：之前把它与「动 PG 角色体系」绑在一起是过度耦合。
//
// 三件事物理隔离（本文件只负责第 1 件）：
//  1. 运行时断言（本文件）：只读系统表 pg_roles，零 migration 成本；
//  2. 角色创建：deploy/init-roles.sql（bootstrap，非 schema 变更）；
//  3. 连接串切换：环境变量 / secrets 变更，代码零改动。
//
// 红线③约束的是 **schema 变更**，本自检只读 `pg_roles` 视图，不写任何库对象。
package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
)

// pgSuperuserPolicy 是 LIMS_PG_SUPERUSER_CHECK 的解析结果。
type pgSuperuserPolicy int

const (
	// pgSuperFail：默认策略。rolsuper=true → 阻断启动（fail-fast）。
	// 业主裁定：「安全底线，不是可选项」。
	pgSuperFail pgSuperuserPolicy = iota
	// pgSuperWarn：只告警，不阻断。用于既有 superuser 部署的过渡期。
	pgSuperWarn
	// pgSuperOff：跳过自检（必须显式设置，且留日志）。
	pgSuperOff
)

func (p pgSuperuserPolicy) String() string {
	switch p {
	case pgSuperWarn:
		return "warn"
	case pgSuperOff:
		return "off"
	default:
		return "fail"
	}
}

// parsePgSuperuserPolicy 解析 LIMS_PG_SUPERUSER_CHECK。
// 未设置 / 空 / 无法识别 → **fail**（安全默认，不静默降级）。
func parsePgSuperuserPolicy() pgSuperuserPolicy {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LIMS_PG_SUPERUSER_CHECK"))) {
	case "warn", "warn_only", "log":
		return pgSuperWarn
	case "off", "0", "false", "disable", "disabled", "skip":
		return pgSuperOff
	default:
		return pgSuperFail
	}
}

// CheckPgSuperuser 判定当前连接角色是否 superuser（只读系统表）。
//
// 查询：SELECT rolsuper FROM pg_roles WHERE rolname = current_user
// pg_roles 为系统视图，对所有角色可读（仅 rolpassword 恒为空），故非 superuser
// 角色也能完成自检。
func CheckPgSuperuser(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("pg superuser check: nil db")
	}
	var isSuper bool
	err := db.QueryRowContext(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper)
	if err == sql.ErrNoRows {
		// current_user 不在 pg_roles 中：异常态，无法证明非 superuser。
		return false, fmt.Errorf("pg superuser check: current_user not present in pg_roles")
	}
	if err != nil {
		return false, fmt.Errorf("pg superuser check: %w", err)
	}
	return isSuper, nil
}

// enforcePgSuperuser 是策略判定的纯函数（便于单测，不依赖真机）。
//
// 语义：
//   - isSuper=false → 放行；
//   - isSuper=true → fail 策略返回 error（阻断），warn 策略返回 nil（仅告警）；
//   - checkErr != nil（自检无法完成）→ **fail-closed**：fail 策略下同样阻断，
//     因为「无法证明非 superuser」等同「可能是 superuser」；warn/off 下仅告警。
func enforcePgSuperuser(pol pgSuperuserPolicy, isSuper bool, checkErr error) error {
	if pol == pgSuperOff {
		log.Printf("[pg-superuser-check] policy=off: 跳过自检（显式关闭，非默认行为）")
		return nil
	}
	const remediation = "请使用非 superuser 角色连接（见 deploy/init-roles.sql 创建 lightmem_app），" +
		"或临时设 LIMS_PG_SUPERUSER_CHECK=warn 过渡"
	if checkErr != nil {
		msg := fmt.Sprintf("[pg-superuser-check] 自检无法完成（fail-closed）：%v；%s", checkErr, remediation)
		log.Printf("%s", msg)
		if pol == pgSuperFail {
			return fmt.Errorf("pg superuser check 无法完成，按 fail-closed 阻断启动: %w；%s",
				checkErr, remediation)
		}
		return nil
	}
	if !isSuper {
		log.Printf("[pg-superuser-check] policy=%s: current_user 非 superuser — 通过", pol)
		return nil
	}
	msg := fmt.Sprintf("[pg-superuser-check] policy=%s: **当前连接角色是 superuser**——"+
		"RLS / pg_read_all_data 等数据库层约束可被绕过，多租户隔离形同虚设；%s", pol, remediation)
	log.Printf("%s", msg)
	if pol == pgSuperFail {
		return fmt.Errorf("pg 连接角色为 superuser，按 LIMS_PG_SUPERUSER_CHECK=fail 阻断启动；%s",
			remediation)
	}
	return nil
}

// EnforcePgSuperuserPolicy 执行启动自检：在**连接池初始化之后、对外 ready 之前**
// 调用（R28-A：接线点见 newPgBackend 的 PingContext 之后）。
//
// 返回非 nil error = 阻断启动。非 PG 后端（sqlite / qdrant）不调用本函数。
func EnforcePgSuperuserPolicy(ctx context.Context, db *sql.DB) error {
	pol := parsePgSuperuserPolicy()
	isSuper, err := CheckPgSuperuser(ctx, db)
	return enforcePgSuperuser(pol, isSuper, err)
}
