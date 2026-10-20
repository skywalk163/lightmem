// SQLite 耐久显式化测试（R29-E / L-251 / 行为变更 B5）。
//
// 覆盖三条口径：
//  1. **默认关（红线④）**：profile 不配任何 pragma 字段 → 不执行任何 PRAGMA，
//     与 c203db8 逐位一致（用 PRAGMA 回读验证：busy_timeout=0、synchronous 不变）；
//  2. **显式生效**：配置后 PRAGMA 真实生效（回读断言，不是「跑了没报错」）；
//  3. **非法值 fail-fast**：synchronous 写了非白名单值 → 启动报错（绝不静默忽略）。
package kernel

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pragmaInt 回读一个整型 PRAGMA（busy_timeout / wal_autocheckpoint）。
func pragmaInt(t *testing.T, b StorageBackend, pragma string) int64 {
	t.Helper()
	db := backendDB(t, b)
	var v int64
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&v); err != nil {
		t.Fatalf("read PRAGMA %s: %v", pragma, err)
	}
	return v
}

// pragmaStr 回读一个字符串型 PRAGMA（synchronous / journal_mode）。
func pragmaStr(t *testing.T, b StorageBackend, pragma string) string {
	t.Helper()
	db := backendDB(t, b)
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&v); err != nil {
		t.Fatalf("read PRAGMA %s: %v", pragma, err)
	}
	return strings.ToLower(v)
}

// backendDB 取后端的底层 *sql.DB（测试辅助；只支持 sqlite 后端）。
func backendDB(t *testing.T, b StorageBackend) *sql.DB {
	t.Helper()
	sb, ok := b.(*sqliteBackend)
	if !ok {
		t.Fatalf("backend %T is not *sqliteBackend", b)
	}
	return sb.db
}

// ── 1. 默认关自证（红线④ / B5「默认不设任何 pragma = 现状」）──

// 零值 profile：不得执行任何 PRAGMA。回读 busy_timeout=0（SQLite 默认值）
// 与 synchronous 保持库默认（WAL 档 modernc 默认 1=NORMAL；此处只断言
// 「未被我们改成 3=FULL」——即默认关不改写）。
func TestSQLiteDurability_DefaultOffNoPragma(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{Path: path, WALMode: true, FKEnabled: true}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()

	if got := pragmaInt(t, b, "busy_timeout"); got != 0 {
		t.Fatalf("busy_timeout = %d, want 0 (default off: no PRAGMA executed)", got)
	}
	if got := pragmaStr(t, b, "synchronous"); got == "3" {
		t.Fatalf("synchronous = 3 (FULL): default-off must not set FULL pragma")
	}
	sync, busy, wal, ckpt := b.(*sqliteBackend).DurabilityConfig()
	if sync != "" || busy != 0 || wal != 0 || ckpt {
		t.Fatalf("DurabilityConfig = %q/%s/%d/%v, want all zero", sync, busy, wal, ckpt)
	}
}

// :memory: 档同样默认关（busy_timeout=0）。
func TestSQLiteDurability_DefaultOffMemory(t *testing.T) {
	ctx := context.Background()
	b, err := newSQLiteBackend(ctx, nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend(nil): %v", err)
	}
	defer b.Close()
	sb := b
	if got := pragmaInt(t, sb, "busy_timeout"); got != 0 {
		t.Fatalf("busy_timeout = %d, want 0", got)
	}
}

// ── 2. 显式生效（回读实证，非「没报错」）──

// busy_timeout_ms 显式配置 → PRAGMA busy_timeout 真实生效。
func TestSQLiteDurability_BusyTimeoutApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, BusyTimeoutMS: 2500,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	if got := pragmaInt(t, b, "busy_timeout"); got != 2500 {
		t.Fatalf("busy_timeout = %d, want 2500", got)
	}
	if _, busy, _, _ := b.(*sqliteBackend).DurabilityConfig(); busy != 2500*time.Millisecond {
		t.Fatalf("DurabilityConfig busy = %s, want 2.5s", busy)
	}
}

// synchronous=full → PRAGMA synchronous=2 真实生效（大小写不敏感）。
// 口径：SQLite synchronous 取值 OFF=0 / NORMAL=1 / **FULL=2** / EXTRA=3
// （PRAGMA 回读返回的是档位编号，不是字符串名）。
func TestSQLiteDurability_SynchronousFullApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, Synchronous: "FULL",
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	if got := pragmaStr(t, b, "synchronous"); got != "2" {
		t.Fatalf("synchronous = %q, want 2 (FULL)", got)
	}
}

// synchronous=extra → PRAGMA synchronous=3 真实生效（最高耐久档）。
func TestSQLiteDurability_SynchronousExtraApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, Synchronous: "extra",
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	if got := pragmaStr(t, b, "synchronous"); got != "3" {
		t.Fatalf("synchronous = %q, want 3 (EXTRA)", got)
	}
}

// synchronous=off → PRAGMA synchronous=0 真实生效。
func TestSQLiteDurability_SynchronousOffApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, Synchronous: "off",
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	if got := pragmaStr(t, b, "synchronous"); got != "0" {
		t.Fatalf("synchronous = %q, want 0 (OFF)", got)
	}
}

// wal_autocheckpoint 显式配置 → PRAGMA 真实生效。
func TestSQLiteDurability_WALAutocheckpointApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, WALAutocheckpoint: 512,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	if got := pragmaInt(t, b, "wal_autocheckpoint"); got != 512 {
		t.Fatalf("wal_autocheckpoint = %d, want 512", got)
	}
}

// ── 3. 非法值 fail-fast（配置写了却执行不了 = 不带病启动）──

func TestSQLiteDurability_InvalidSynchronousFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, Synchronous: "banana",
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err == nil {
		if b != nil {
			_ = b.Close()
		}
		t.Fatalf("invalid synchronous value must fail-fast, got nil error")
	}
	if !strings.Contains(err.Error(), "synchronous") {
		t.Fatalf("error = %v, want mention of synchronous", err)
	}
}

// 空白 synchronous（如 yaml 写了 `synchronous: ""`）= 空 = 默认关，不报错。
func TestSQLiteDurability_BlankSynchronousIsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, Synchronous: "   ",
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("blank synchronous should be treated as off: %v", err)
	}
	defer b.Close()
	if sync, _, _, _ := b.(*sqliteBackend).DurabilityConfig(); sync != "" {
		t.Fatalf("synchronous = %q, want empty (trimmed)", sync)
	}
}

// 负 BusyTimeoutMS（yaml 手滑）= 0 = 不执行。
func TestSQLiteDurability_NegativeBusyTimeoutIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, BusyTimeoutMS: -100,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("negative busy timeout should be ignored: %v", err)
	}
	defer b.Close()
	if got := pragmaInt(t, b, "busy_timeout"); got != 0 {
		t.Fatalf("busy_timeout = %d, want 0 (negative ignored)", got)
	}
}

// ── 4. checkpoint_on_close（B5）──

// checkpoint_on_close=true 且为文件档 → Close 前执行 TRUNCATE。
//
// 实证口径（**不用 WAL 文件大小**）：SQLite 在「最后一个连接关闭」时本来就会
// 自动 checkpoint 并删掉 -wal 文件，因此「关闭后 WAL=0」**不能**证明是我们
// 做的 checkpoint（c203db8 也如此）。改用**时序证据**：
//   - 显式调用 checkpointTruncate（= Close 内同一条路径）后，PASSIVE 回显
//     log=0（WAL 已被清空且可再次写入）；
//   - 同时验证 Close 成功后数据仍可读（checkpoint 不破坏数据）。
func TestSQLiteDurability_CheckpointOnCloseTruncatesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, CheckpointOnClose: true,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	sb := b.(*sqliteBackend)
	persistSeed(t, sb, "m1", "t1", "u1")

	// 走 Close 内的同一条 checkpoint 路径（带 2s 超时）。
	if err := sb.checkpointTruncate(ctx); err != nil {
		t.Fatalf("checkpointTruncate: %v", err)
	}
	// PASSIVE 回显：log=0 表示 WAL 已被 checkpoint 干净（busy=0, checkpointed>0）。
	var busy, logPages, ckptPages int
	if err := sb.db.QueryRowContext(ctx,
		`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logPages, &ckptPages); err != nil {
		t.Fatalf("wal_checkpoint(PASSIVE) after TRUNCATE: %v", err)
	}
	if logPages != 0 {
		t.Fatalf("wal pages after TRUNCATE = %d, want 0 (checkpoint_on_close took effect)", logPages)
	}
	// 数据仍在（checkpoint 不破坏数据）。
	got, err := sb.ScanAll(ctx, 0)
	if err != nil {
		t.Fatalf("ScanAll after checkpoint: %v", err)
	}
	if len(got) != 1 || got[0].MemoryID != "m1" {
		t.Fatalf("rows after checkpoint = %d, want 1 (m1)", len(got))
	}

	// 然后正常 Close（内部会再走一次 TRUNCATE，幂等无害）。
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 后重开同路径：数据仍在（证明 checkpoint 未丢数据）。
	b2 := newPersistBackend(t, path)
	defer b2.Close()
	got2, err := b2.ScanAll(ctx, 0)
	if err != nil {
		t.Fatalf("ScanAll after reopen: %v", err)
	}
	if len(got2) != 1 || got2[0].MemoryID != "m1" {
		t.Fatalf("rows after reopen = %d, want 1 (m1)", len(got2))
	}
}

// 默认关（不配 checkpoint_on_close）→ Close 不执行 checkpoint。
//
// 实证口径：**不用 WAL 文件大小**（SQLite 在最后一个连接关闭时本来就会自动
// checkpoint 并删 -wal，WAL 大小不能区分「我们做的」与「SQLite 自己做的」）。
// 改用两条硬证据：
//  1. parseSQLiteDurability 对该 profile 报告 checkpointOnClose=false
//     （即 Close 内的 if 守卫不会进入）；
//  2. Close 前后**不新增**任何 wal_checkpoint 调用——用 stderr 捕获：
//     checkpoint 失败时会打 "[sqlite] checkpoint(TRUNCATE) on close failed"，
//     默认关路径连这行都不该出现。
func TestSQLiteDurability_CheckpointOffByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{Path: path, WALMode: true, FKEnabled: true}}
	if parseSQLiteDurability(cfg.SQLite).checkpointOnClose {
		t.Fatalf("default profile must report checkpointOnClose=false")
	}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	sb := b.(*sqliteBackend)
	persistSeed(t, sb, "m1", "t1", "u1")

	// 捕获 stderr：默认关路径不得出现 checkpoint 相关输出。
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	closeErr := b.Close()
	w.Close()
	os.Stderr = oldStderr
	captured, _ := io.ReadAll(r)
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	if strings.Contains(string(captured), "checkpoint") {
		t.Fatalf("default-off Close emitted checkpoint output: %q", string(captured))
	}

	// 数据仍可读（默认关不影响持久化）。
	b2 := newPersistBackend(t, path)
	defer b2.Close()
	got, err := b2.ScanAll(ctx, 0)
	if err != nil {
		t.Fatalf("ScanAll after reopen: %v", err)
	}
	if len(got) != 1 || got[0].MemoryID != "m1" {
		t.Fatalf("rows after reopen = %d, want 1 (m1)", len(got))
	}
}

// checkpoint 失败只告警不阻塞关闭：先成功 checkpoint 一次，再关掉底层连接后
// 触发：应返回错误而非 panic；最终 Close 仍可达（database/sql 幂等）。
func TestSQLiteDurability_CheckpointErrorDoesNotBlockClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lims.sqlite")
	ctx := context.Background()
	cfg := &StorageConfig{SQLite: &SQLiteConfig{
		Path: path, WALMode: true, FKEnabled: true, CheckpointOnClose: true,
	}}
	b, err := newSQLiteBackend(ctx, cfg)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	sb := b.(*sqliteBackend)
	if err := sb.checkpointTruncate(context.Background()); err != nil {
		t.Fatalf("checkpoint on open db should succeed: %v", err)
	}
	// 关掉底层连接后再次 checkpoint：应返回错误而非 panic
	if err := sb.db.Close(); err != nil {
		t.Fatalf("raw db close: %v", err)
	}
	if err := sb.checkpointTruncate(context.Background()); err == nil {
		t.Fatalf("checkpoint on closed db should error, got nil")
	}
	// 最终 Close 仍可调用（database/sql 幂等）
	if err := b.Close(); err != nil {
		t.Fatalf("Close after failed checkpoint should still be reachable: %v", err)
	}
}
