package kernel

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openRawSQLite 绕过 newSQLiteBackend 的自建表逻辑，直接开一个裸连接，
// 供「缺表/缺列」fail-fast 用例构造残缺 schema（B2 反证面）。
func openRawSQLite(t *testing.T, path string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite %s: %v", path, err)
	}
	return db, nil
}

func TestRunBootReport_NilBackend(t *testing.T) {
	rep, err := RunBootReport(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("RunBootReport(nil backend) error: %v", err)
	}
	if rep == nil {
		t.Fatalf("RunBootReport returned nil report")
	}
	if rep.Backend != "" {
		t.Fatalf("nil backend should yield empty Backend, got %q", rep.Backend)
	}
}

func TestRunBootReport_EmptyDataDir(t *testing.T) {
	b := &mockBackend{}
	rep, err := RunBootReport(context.Background(), b, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Backend != "mock" {
		t.Fatalf("Backend = %q, want mock", rep.Backend)
	}
	if rep.Unfinished != "" {
		t.Fatalf("Unfinished should be empty with empty dataDir")
	}
}

func TestRunBootReport_DetectsUnfinishedMarker(t *testing.T) {
	dir := t.TempDir()
	b := &mockBackend{}
	if err := MarkUnfinished(dir, "drain-x"); err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Unfinished != "drain-x" {
		t.Fatalf("Unfinished = %q, want drain-x", rep.Unfinished)
	}
}

// E 线：多标记时输出确定性（排序后逗号拼接），不随 ReadDir 顺序漂移。
func TestRunBootReport_MultipleMarkersSorted(t *testing.T) {
	dir := t.TempDir()
	b := &mockBackend{}
	for _, job := range []string{"zz", "aa", "mm"} {
		if err := MarkUnfinished(dir, job); err != nil {
			t.Fatalf("MarkUnfinished(%s): %v", job, err)
		}
	}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Unfinished != "aa,mm,zz" {
		t.Fatalf("Unfinished = %q, want aa,mm,zz (deterministic order)", rep.Unfinished)
	}
}

func TestRunBootReport_ClearUnfinished(t *testing.T) {
	dir := t.TempDir()
	b := &mockBackend{}
	_ = MarkUnfinished(dir, "drain-y")
	if err := ClearUnfinished(dir); err != nil {
		t.Fatalf("ClearUnfinished: %v", err)
	}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Unfinished != "" {
		t.Fatalf("Unfinished should be cleared, got %q", rep.Unfinished)
	}
}

func TestRunBootReport_DataDirNotExist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-dir")
	b := &mockBackend{}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("missing dataDir should not error: %v", err)
	}
	if rep.Unfinished != "" {
		t.Fatalf("Unfinished should be empty, got %q", rep.Unfinished)
	}
}

func TestBootReportStructFields(t *testing.T) {
	rep := BootReport{
		Backend:      "pgvector",
		Table:        "ok",
		Columns:      "ok",
		Rows:         100,
		VersionMatch: true,
		Unfinished:   "",
		RestorePoint: "2026-10-20T00:00:00Z",
	}
	if rep.Rows != 100 || !rep.VersionMatch || rep.Backend != "pgvector" {
		t.Fatalf("BootReport fields not carried")
	}
}

func TestBootReportMarkerPath(t *testing.T) {
	p := bootReportMarkerPath("/data", "job1")
	if p != "/data/job1.drain.unfinished" {
		t.Fatalf("marker path = %q", p)
	}
}

// ------------------------------------------------------------------
// E 线：表/列断言（真实 SQLite 后端）
// ------------------------------------------------------------------

// 真 SQLite 后端：表/列齐备 → ok + 条数正确（R16 对比基线）。
func TestBootCheck_SQLiteTableColumnsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	b := newPersistBackend(t, path)
	defer b.Close()
	persistSeed(t, b, "m1", "t1", "u1")
	persistSeed(t, b, "m2", "t1", "u1")

	rep, err := RunBootReport(ctx, b, "")
	if err != nil {
		t.Fatalf("RunBootReport: %v", err)
	}
	if rep.Table != "ok" {
		t.Fatalf("Table = %q, want ok", rep.Table)
	}
	if rep.Columns != "ok" {
		t.Fatalf("Columns = %q, want ok", rep.Columns)
	}
	if rep.Rows != 2 {
		t.Fatalf("Rows = %d, want 2", rep.Rows)
	}
	if !rep.VersionMatch {
		t.Fatalf("VersionMatch should be true when no VERSION file")
	}
}

// WAL 恢复点：文件档下 RestorePoint 必须含 wal_bytes=。
func TestBootCheck_SQLiteRestorePointWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	b := newPersistBackend(t, path)
	defer b.Close()
	persistSeed(t, b, "m1", "t1", "u1")

	rep, err := RunBootReport(ctx, b, "")
	if err != nil {
		t.Fatalf("RunBootReport: %v", err)
	}
	if !strings.Contains(rep.RestorePoint, "wal_bytes=") {
		t.Fatalf("RestorePoint = %q, want contains wal_bytes=", rep.RestorePoint)
	}
}

// 缺表 → fail-fast（B2）：删表后自检必须返回 error。
func TestBootCheck_SQLiteMissingTableFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	b := newPersistBackend(t, path)
	defer b.Close()
	persistSeed(t, b, "m1", "t1", "u1")
	if _, err := b.db.ExecContext(ctx, `DROP TABLE memories`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	rep, err := RunBootReport(ctx, b, "")
	if err == nil {
		t.Fatalf("missing table should fail-fast (B2), got nil error")
	}
	if rep.Table != "missing" {
		t.Fatalf("Table = %q, want missing", rep.Table)
	}
}

// 缺列 → fail-fast（B2，设计稿 §3.4.4 评审第 13 点：不能只 SELECT 1）。
// 做法：建一张只有 memory_id 的残缺表（另开库文件，避免污染真实后端）。
func TestBootCheck_SQLiteMissingColumnFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.sqlite")
	ctx := context.Background()
	db, err := openRawSQLite(t, path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE memories (memory_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create broken table: %v", err)
	}
	b := &sqliteBackend{name: "sqlite", path: path, db: db}

	rep, err := RunBootReport(ctx, b, "")
	if err == nil {
		t.Fatalf("missing column should fail-fast (B2), got nil error")
	}
	if !strings.HasPrefix(rep.Columns, "missing:") || !strings.Contains(rep.Columns, "data") {
		t.Fatalf("Columns = %q, want missing:...data...", rep.Columns)
	}
}

// 测试桩后端 → skipped，不阻断（qdrant / mock 无本契约）。
func TestBootCheck_NonSQLEndpointSkipped(t *testing.T) {
	rep, err := RunBootReport(context.Background(), &mockBackend{}, "")
	if err != nil {
		t.Fatalf("non-sql backend should be skipped, got error: %v", err)
	}
	if rep.Table != "skipped" || rep.Columns != "skipped" {
		t.Fatalf("Table/Columns = %q/%q, want skipped/skipped", rep.Table, rep.Columns)
	}
}

// ------------------------------------------------------------------
// E 线：VERSION 一致性
// ------------------------------------------------------------------

func TestBootVersionMatch(t *testing.T) {
	dir := t.TempDir()
	old := BuildVersion
	defer func() { BuildVersion = old }()

	// 无 VERSION 文件 → true（不误报）
	if !bootVersionMatch(dir) {
		t.Fatalf("missing VERSION file should be match=true")
	}
	// 未知构建版本 → true（开发构建无法比对）
	BuildVersion = "unknown"
	writeVersionFile(t, dir, "abcdef1234567890")
	if !bootVersionMatch(dir) {
		t.Fatalf("unknown build version should be match=true")
	}
	// 一致（短前缀）→ true
	BuildVersion = "abcdef1234567890"
	if !bootVersionMatch(dir) {
		t.Fatalf("matching commit should be match=true")
	}
	// 不一致 → false
	BuildVersion = "ffffffffffffffff"
	if bootVersionMatch(dir) {
		t.Fatalf("mismatched commit should be match=false")
	}
}

func writeVersionFile(t *testing.T, dir, commit string) {
	t.Helper()
	content := "installed_at=2026-10-20T00:00:00Z\nsource_commit=" + commit + "\nport=18443\n"
	if err := os.WriteFile(filepath.Join(dir, bootVersionFile), []byte(content), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
}

// ------------------------------------------------------------------
// E 线：耐久配置汇总（B5 留档）
// ------------------------------------------------------------------

func TestBackendDurabilitySummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true,
		Synchronous: "FULL", BusyTimeoutMS: 2500, WALAutocheckpoint: 1000, CheckpointOnClose: true,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	rep, err := RunBootReport(ctx, b, "")
	if err != nil {
		t.Fatalf("RunBootReport: %v", err)
	}
	if !strings.Contains(rep.Durability, "synchronous=full") {
		t.Fatalf("Durability = %q, want synchronous=full", rep.Durability)
	}
	if !strings.Contains(rep.Durability, "checkpoint_on_close=true") {
		t.Fatalf("Durability = %q, want checkpoint_on_close=true", rep.Durability)
	}
	// 默认关：零值 profile → "default"
	b2 := newPersistBackend(t, filepath.Join(t.TempDir(), "lims2.sqlite"))
	defer b2.Close()
	rep2, err := RunBootReport(ctx, b2, "")
	if err != nil {
		t.Fatalf("RunBootReport: %v", err)
	}
	if rep2.Durability != "default" {
		t.Fatalf("Durability = %q, want default", rep2.Durability)
	}
}

// 非 SQL 后端 → 空串（不含任何后端名以外信息）。
func TestBackendDurabilitySummary_NonSQL(t *testing.T) {
	if got := backendDurabilitySummary(&mockBackend{}); got != "" {
		t.Fatalf("mock backend durability = %q, want empty", got)
	}
}

// ------------------------------------------------------------------
// E 线：辅助函数
// ------------------------------------------------------------------

func TestMissingColumns(t *testing.T) {
	have := map[string]bool{"memory_id": true, "tenant_id": true}
	got := missingColumns(have, pgRequiredColumns)
	want := []string{"user_id", "fact", "recorded_at", "superseded_by", "vector_status"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing[%d] = %q, want %q (order must be deterministic)", i, got[i], want[i])
		}
	}
	if len(missingColumns(have, sqliteRequiredColumns)) == 0 {
		// memory_id 在 have 里，但 data 不在 → 应缺 1 个
		t.Fatalf("data column should be reported missing")
	}
}

func TestWalFileSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lims.sqlite")
	// 无 -wal 文件 → 0
	if sz, err := walFileSize(p); err != nil || sz != 0 {
		t.Fatalf("walFileSize(no wal) = %d, %v; want 0, nil", sz, err)
	}
	// 造一个 -wal 文件
	wal := p + "-wal"
	if err := os.WriteFile(wal, make([]byte, 42), 0o644); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	if sz, err := walFileSize(p); err != nil || sz != 42 {
		t.Fatalf("walFileSize = %d, %v; want 42, nil", sz, err)
	}
}

func TestParseBootInt(t *testing.T) {
	cases := map[string]int64{"12": 12, " 7 ": 7, "abc": 0, "": 0}
	for in, want := range cases {
		if got := parseBootInt(in); got != want {
			t.Fatalf("parseBootInt(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSetBuildVersion(t *testing.T) {
	old := BuildVersion
	defer func() { BuildVersion = old }()
	SetBuildVersion("test-version-123")
	if BuildVersion != "test-version-123" {
		t.Fatalf("SetBuildVersion did not take effect: %q", BuildVersion)
	}
}

// checkpointTimeout 必须与 B 线探针预算同口径（2s），防止关闭路径长挂。
func TestCheckpointTimeoutConstant(t *testing.T) {
	if checkpointTimeout != 2*time.Second {
		t.Fatalf("checkpointTimeout = %s, want 2s", checkpointTimeout)
	}
}
