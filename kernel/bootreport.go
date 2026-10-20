// 启动恢复自检（R29-T0 S0 落骨架；**R29-E 硬化**，任务书 §8.1.2 / 设计稿 §3.4.4）。
//
// 职责边界（任务书 §2.2 / §10）：
//   - T0 落 BootReport 结构与 RunBootReport 骨架（只查「未完成标记」+ 回填后端名）。
//   - E 线硬化：表存在、关键列断言、条数、恢复点、VERSION 一致性、未完成标记联动，
//     **缺表/缺列 → fail-fast（行为变更 B2）**。
//
// 检查矩阵（设计稿 §3.4.4，逐条对齐）：
//
//	| 检查        | SQLite                              | PG                                          | 失败处置      |
//	|-------------|-------------------------------------|---------------------------------------------|---------------|
//	| 可达        | Ping                                | Ping                                        | fail-fast（外层已有） |
//	| 表存在      | 自建恒过（仍显式断言，防漂移）      | SELECT to_regclass('memories')              | **fail-fast** |
//	| 关键列存在  | PRAGMA table_info → memory_id,data  | information_schema.columns → 7 列           | **fail-fast** |
//	| 数据条数    | SELECT count(*)                      | 同                                          | 记录          |
//	| 恢复点      | WAL 大小 + wal_checkpoint(PASSIVE)   | pg_last_xact_replay_timestamp()（备库）     | 记录          |
//	| 版本一致性  | VERSION 文件（install_lims_rc_d.sh） | 同                                          | 警告+继续     |
//	| 上次异常关闭| 未完成标记                          | 同                                          | 记录          |
//
// 默认关（红线④）：`main.go` 仅当显式配置 `-data-dir` 才调用 RunBootReport；
// dataDir 为空 = 与 c203db8 逐位一致（不执行任何自检）。自检本身**只读**：
// 除 PRAGMA wal_checkpoint(PASSIVE) 外不写任何库对象（PASSIVE 不阻塞读写，
// 是 SQLite 官方推荐的「获取 WAL 大小」手段，非破坏性操作）。
//
// 红线②③：只用 stdlib；只读检查，不改任何 Schema/SQL/migrations。
package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BootReport 启动自检结果（§10 冻结字段，E 线填实）。
type BootReport struct {
	Backend      string
	Table        string // "ok" / "missing" / 后端名相关描述
	Columns      string // "ok" / "missing:<col>,<col>"
	Rows         int64
	VersionMatch bool
	Unfinished   string // 非空 = 上次有未完成的排水任务（残留标记）
	RestorePoint string // WAL 大小 / PG 恢复时间戳等
	// Durability 是本后端的耐久配置快照（R29-E，B5 留档；不含口令/DSN）。
	Durability string
}

// bootCheckColumnSet 是 PG 关键列清单（设计稿 §3.4.4；真源 migrations/001_pgvector.up.sql）。
// SQLite 侧对应 memory_id, data（单表 JSON 方案，见 backend_sqlite.go:1-9）。
var (
	pgRequiredColumns = []string{
		"memory_id", "tenant_id", "user_id", "fact",
		"recorded_at", "superseded_by", "vector_status",
	}
	sqliteRequiredColumns = []string{"memory_id", "data"}
)

// bootVersionFile 是安装脚本写的版本文件（install_lims_rc_d.sh:128-134）。
// 内容含 source_commit=<sha>；自检只读它做「二进制 vs 库」一致性提示。
const bootVersionFile = "VERSION"

// RunBootReport 执行启动恢复自检（§10 冻结签名；E 线硬化）。
//
// 执行顺序：后端名 → 表/列/条数/恢复点（按后端类型分派）→ 耐久配置 → VERSION →
// 未完成标记。任一步骤 fail-fast 条件（缺表/缺列）即返回 error，由 main.go
// log.Fatalf 阻断启动（B2 行为变更）；其余项只记录不阻断。
//
// dataDir 为空时只回填后端名与耐久配置，不读标记/版本（默认关）。
func RunBootReport(ctx context.Context, backend StorageBackend, dataDir string) (*BootReport, error) {
	rep := &BootReport{}
	if backend != nil {
		rep.Backend = backend.Name()
		rep.Durability = backendDurabilitySummary(backend)
	}

	if backend != nil {
		if err := runBootSchemaCheck(ctx, backend, rep); err != nil {
			return rep, err // 缺表/缺列 → fail-fast（B2）
		}
	}

	rep.VersionMatch = bootVersionMatch(dataDir)

	if dataDir != "" {
		unfinished, err := readUnfinishedMarkers(dataDir)
		if err != nil {
			return rep, err
		}
		rep.Unfinished = unfinished
	}
	return rep, nil
}

// runBootSchemaCheck 按后端类型执行表/列/条数/恢复点检查。
// 返回非 nil error = fail-fast（B2：缺表或缺列不得带病上线）。
func runBootSchemaCheck(ctx context.Context, backend StorageBackend, rep *BootReport) error {
	switch b := backend.(type) {
	case *sqliteBackend:
		if b == nil || b.db == nil {
			return nil
		}
		return sqliteBootCheck(ctx, b, rep)
	case *pgBackend:
		if b == nil || b.db == nil {
			return nil
		}
		return pgBootCheck(ctx, b, rep)
	default:
		// 其他后端（qdrant / 测试桩）无本自检契约：只回填后端名，不阻断。
		rep.Table = "skipped"
		rep.Columns = "skipped"
		return nil
	}
}

// sqliteBootCheck：表恒过（newSQLiteBackend 自建）但仍显式断言列与条数，
// 防止「有人手工改了表结构」的漂移（设计稿 §3.4.4 评审第 13 点）。
func sqliteBootCheck(ctx context.Context, b *sqliteBackend, rep *BootReport) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var tblName string
	err := b.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memories'`).Scan(&tblName)
	if err == sql.ErrNoRows {
		rep.Table = "missing"
		return fmt.Errorf("boot self-check: table memories missing on %s (backend=%s)",
			b.path, b.name)
	}
	if err != nil {
		return fmt.Errorf("boot self-check: sqlite_master query: %w", err)
	}
	rep.Table = "ok"

	rows, err := b.db.QueryContext(ctx, `PRAGMA table_info(memories)`)
	if err != nil {
		return fmt.Errorf("boot self-check: pragma table_info: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("boot self-check: pragma table_info scan: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("boot self-check: pragma table_info rows: %w", err)
	}
	if missing := missingColumns(have, sqliteRequiredColumns); len(missing) > 0 {
		rep.Columns = "missing:" + strings.Join(missing, ",")
		return fmt.Errorf("boot self-check: %s missing columns %v (want %v)",
			b.name, missing, sqliteRequiredColumns)
	}
	rep.Columns = "ok"

	var n int64
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
		return fmt.Errorf("boot self-check: count(*) : %w", err)
	}
	rep.Rows = n

	// 恢复点：WAL 大小（bytes）+ PASSIVE checkpoint 回传的 WAL 剩余页数。
	// PASSIVE 不阻塞读写、不截断，是官方推荐的 WAL 观测手段（非破坏性）。
	if b.path != "" {
		if sz, werr := walFileSize(b.path); werr == nil {
			rep.RestorePoint = fmt.Sprintf("wal_bytes=%d", sz)
		}
		var busy, logPages, ckptPages int
		if cerr := b.db.QueryRowContext(ctx,
			`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logPages, &ckptPages); cerr == nil {
			rep.RestorePoint += fmt.Sprintf(" wal_pages=%d checkpointed=%d", logPages, ckptPages)
		}
	}
	return nil
}

// pgBootCheck：PG 档表/列断言（设计稿 §3.4.4；真源 migrations/001_pgvector.up.sql）。
// 只读系统视图 information_schema / to_regclass，零 migration 成本（红线②）。
func pgBootCheck(ctx context.Context, b *pgBackend, rep *BootReport) error {
	var regclass sql.NullString
	if err := b.db.QueryRowContext(ctx, `SELECT to_regclass('memories')`).Scan(&regclass); err != nil {
		return fmt.Errorf("boot self-check: pg to_regclass: %w", err)
	}
	if !regclass.Valid || strings.TrimSpace(regclass.String) == "" {
		rep.Table = "missing"
		return fmt.Errorf("boot self-check: table memories missing on pgvector backend "+
			"(database=%s); apply kernel/migrations/001_pgvector.up.sql before startup",
			b.conf.Database)
	}
	rep.Table = "ok"

	rows, err := b.db.QueryContext(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_name = 'memories'`)
	if err != nil {
		return fmt.Errorf("boot self-check: pg information_schema: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("boot self-check: pg information_schema scan: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("boot self-check: pg information_schema rows: %w", err)
	}
	if missing := missingColumns(have, pgRequiredColumns); len(missing) > 0 {
		rep.Columns = "missing:" + strings.Join(missing, ",")
		return fmt.Errorf("boot self-check: pgvector backend table memories missing columns %v "+
			"(want %v, source kernel/migrations/001_pgvector.up.sql); "+
			"migrations are not auto-applied — refuse to start with a mismatched schema",
			missing, pgRequiredColumns)
	}
	rep.Columns = "ok"

	var n int64
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
		return fmt.Errorf("boot self-check: pg count(*): %w", err)
	}
	rep.Rows = n

	// 恢复点：备库时才查询（主库该函数不存在/为 NULL，不视为错误）。
	var replay sql.NullString
	if err := b.db.QueryRowContext(ctx,
		`SELECT pg_last_xact_replay_timestamp()::text`).Scan(&replay); err == nil &&
		replay.Valid && strings.TrimSpace(replay.String) != "" {
		rep.RestorePoint = "pg_replay=" + replay.String
	}
	return nil
}

// missingColumns 返回 have 中缺失的必需列（保持需求清单顺序，输出确定性）。
func missingColumns(have map[string]bool, want []string) []string {
	var missing []string
	for _, c := range want {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

// backendDurabilitySummary 汇总后端耐久配置（仅 sqlite 有；pg 档返回空）。
// 不含口令/DSN（B 线最小响应口径；ProfileJSON 已脱敏，勿重复实现）。
// synchronous 归一化为小写，保证日志输出确定性（FULL/full 同形）。
func backendDurabilitySummary(backend StorageBackend) string {
	type durabler interface {
		DurabilityConfig() (string, time.Duration, int, bool)
	}
	d, ok := backend.(durabler)
	if !ok {
		return ""
	}
	sync, busy, wal, ckpt := d.DurabilityConfig()
	sync = strings.ToLower(sync)
	if sync == "" && busy <= 0 && wal <= 0 && !ckpt {
		return "default"
	}
	return fmt.Sprintf("synchronous=%s busy_timeout=%s wal_autocheckpoint=%d checkpoint_on_close=%v",
		sync, busy.String(), wal, ckpt)
}

// bootVersionMatch 比较 dataDir/VERSION 的 source_commit 与当前二进制构建版本。
// 版本文件缺失（如未走 install_lims_rc_d.sh 安装）→ 视为一致（true），不误报。
// 当前构建版本经 ldflags 注入 kernel.BuildVersion（默认 unknown；无法比对时同样
// 视为一致，避免开发构建被误判为不一致）。
func bootVersionMatch(dataDir string) bool {
	if dataDir == "" {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, bootVersionFile))
	if err != nil {
		return true // 无版本文件（开发/测试态）→ 不判定不一致
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "source_commit=") {
			continue
		}
		installed := strings.TrimSpace(strings.TrimPrefix(line, "source_commit="))
		if installed == "" || installed == "unknown" {
			return true
		}
		if BuildVersion == "" || BuildVersion == "unknown" || BuildVersion == "dev" {
			return true // 开发构建无法比对
		}
		return strings.HasPrefix(BuildVersion, installed) || strings.HasPrefix(installed, BuildVersion)
	}
	return true
}

// readUnfinishedMarkers 读取 dataDir 下残留的 .drain.unfinished 标记（D 线写入）。
// 按 job 名排序后逗号拼接，输出确定性（供 E 线自检与 /healthz 消费）。
func readUnfinishedMarkers(dataDir string) (string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var unfinished []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".drain.unfinished") {
			unfinished = append(unfinished, strings.TrimSuffix(name, ".drain.unfinished"))
		}
	}
	if len(unfinished) == 0 {
		return "", nil
	}
	sort.Strings(unfinished) // 确定性：多标记时输出稳定
	return strings.Join(unfinished, ","), nil
}

// walFileSize 返回 <dbPath>-wal 的字节数（WAL 不存在 = 0）。
func walFileSize(dbPath string) (int64, error) {
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

// bootReportMarkerPath 仅用于单测构造断言路径（包内可见）。
func bootReportMarkerPath(dataDir, job string) string {
	return filepath.Join(dataDir, job+".drain.unfinished")
}

// parseBootInt 解析 PRAGMA wal_checkpoint 回传的整数字符串（防御性，勿外泄）。
func parseBootInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
