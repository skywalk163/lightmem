// R28-B（业主裁定三·保险二）· CI「测试后 DB 残留检查」。
//
// 业主裁定原文：「自清失败留垃圾：CI 里加一个『测试后 DB 残留检查』步骤——
// 跑完测试后查 users 表里测试前缀的记录数，非零则 fail。这比事后人工发现强。」
//
// 本套件的前缀约定：
//   - tenant：`r18a_<tag>_<pid>_<nanos>`（uniqueTenant）
//   - user  ：`u_<tag>_<pid>_<nanos>`（uniqueUser）
//   - source：`r10b-e2e`（e2eSourceMarker）
//
// 执行顺序：**必须在本套件用例跑完之后**（gate.sh 中 [4] 跑完全量测试，
// 再由 [12] 单独 `-run TestPgFixtureResidue` 复跑一次，避免包内执行顺序
// 不确定导致漏检）。
package pg

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openPgResidueDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := loadPgTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping residue check")
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
		cfg.host, cfg.port, cfg.user, cfg.dbname)
	if cfg.password != "" {
		dsn += " password=" + cfg.password
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("pg open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("pg ping %s/%s: %v", cfg.host, cfg.dbname, err)
	}
	return db
}

// TestPgFixtureResidue：测试残留必须为 0，非零即 FAIL（不静默、不 Skip）。
func TestPgFixtureResidue(t *testing.T) {
	db := openPgResidueDB(t)

	checks := []struct {
		name  string
		where string
	}{
		{"source 标记（本套件写入）", `source = 'r10b-e2e'`},
		{"tenant 前缀 r18a_", `tenant_id LIKE 'r18a!_%' ESCAPE '!'`},
		{"user 前缀 u_", `user_id LIKE 'u!_%' ESCAPE '!'`},
	}

	total := 0
	for _, c := range checks {
		var n int
		q := `SELECT count(*) FROM memories WHERE ` + c.where
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("残留统计失败（%s）：%v", c.name, err)
		}
		if n == 0 {
			continue
		}
		total += n
		// 列出样例，便于定位是哪个用例没清理干净。
		rows, err := db.Query(`SELECT memory_id, tenant_id, user_id, source FROM memories WHERE ` +
			c.where + ` LIMIT 5`)
		if err == nil {
			for rows.Next() {
				var mid, tn, us, src string
				if err := rows.Scan(&mid, &tn, &us, &src); err != nil {
					break
				}
				t.Errorf("残留样例：memory_id=%s tenant=%s user=%s source=%s", mid, tn, us, src)
			}
			rows.Close()
		}
		t.Errorf("DB 残留：%s → %d 行（应为 0）", c.name, n)
	}
	if total > 0 {
		t.Errorf("CI 残留检查未通过：合计 %d 行测试残留。"+
			"说明某用例的 teardown（cleanupTenantsOnExit）未生效或清理失败，"+
			"或存在未走夹具的裸写入——这不是噪声，是隔离缺口。", total)
	}
}
