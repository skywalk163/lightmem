// R29-D · keyset 批量扫描测试（任务书 §7 / §10；判据 D-unit「不丢不重」）。
//
// 覆盖：
//   - keyset 顺序 / 不丢不重（含并发写插曲）；
//   - 回调错误中止、ctx 取消、空表、与 ScanAll 的一致性；
//   - PG 真机往返（LIGHTMEM_PG_HOST 未设时 t.Skip，与 gate [11] 同口径）；
//   - 开关默认关自证（LIMS_MAINT_BATCH 未设 = 旧全表路径）。
//
// 红线：只用 stdlib + 仓库既有测试夹具模式；不动 ScanAll 契约。
package kernel

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newBatchedSqlite 构造临时文件型 sqliteBackend（:memory: 亦可，但文件型
// 更贴近生产路径；批间让位在两种模式下语义一致）。
func newBatchedSqlite(t *testing.T) *sqliteBackend {
	t.Helper()
	path := filepath.Join(t.TempDir(), "batched.db")
	b, err := newSQLiteBackend(context.Background(), &StorageConfig{
		SQLite: &SQLiteConfig{Path: path},
	})
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b.(*sqliteBackend)
}

// batchSeed 写入指定 id 的记忆（最小字段集，反序列化后可与 ScanAll 对齐比较）。
func batchSeed(t *testing.T, b *sqliteBackend, id string) {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       "t1",
		UserID:         "u1",
		Visibility:     VisPrivate,
		MemoryKind:     KindFact,
		Fact:           "fact-" + id,
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// collectBatched 跑完整趟 ScanAllBatched，返回 (全部 id 按返回序, 每批大小, next, err)。
func collectBatched(t *testing.T, b BatchedMemoryScanner, max int, ctx context.Context) ([]string, []int, string, error) {
	t.Helper()
	var (
		all    []string
		sizes  []int
		after  = ""
		rounds = 0
	)
	for {
		rounds++
		if rounds > 10000 { // 防呆：游标不推进 = 死循环，必须炸而不是挂死
			t.Fatalf("ScanAllBatched cursor did not advance (after=%q)", after)
		}
		var batchIDs []string
		next, err := b.ScanAllBatched(ctx, max, after, func(batch []*Memory) error {
			for _, m := range batch {
				batchIDs = append(batchIDs, m.MemoryID)
			}
			return nil
		})
		if err != nil {
			return all, sizes, after, err
		}
		if len(batchIDs) > 0 {
			sizes = append(sizes, len(batchIDs))
			all = append(all, batchIDs...)
		}
		if next == "" {
			return all, sizes, "", nil
		}
		after = next
	}
}

// assertNoDupStrictlyAscending 断言 id 序列无重复且严格升序（keyset 正确性核心）。
func assertNoDupStrictlyAscending(t *testing.T, ids []string) {
	t.Helper()
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q at position %d (keyset 游标重复)", id, i)
		}
		seen[id] = true
		if i > 0 && ids[i-1] >= id {
			t.Fatalf("not strictly ascending at %d: %q >= %q", i, ids[i-1], id)
		}
	}
}

// ------------------------------------------------------------------
// SQLite：keyset 正确性
// ------------------------------------------------------------------

func TestScanAllBatchedSqlite_KeysetOrderAndNoDup(t *testing.T) {
	b := newBatchedSqlite(t)
	const n = 50
	for i := 0; i < n; i++ {
		batchSeed(t, b, "sb-a-"+pad3(i))
	}
	ids, sizes, next, err := collectBatched(t, b, 10, context.Background())
	if err != nil {
		t.Fatalf("collectBatched: %v", err)
	}
	if next != "" {
		t.Fatalf("scan finished but next=%q (should be empty)", next)
	}
	if len(ids) != n {
		t.Fatalf("collected %d ids, want %d (不丢不重)", len(ids), n)
	}
	assertNoDupStrictlyAscending(t, ids)
	// 每批 ≤ max=10；最后一批：n%max==0 时是满批（无不满批），否则不满
	wantLast := n % 10
	if wantLast == 0 {
		wantLast = 10
	}
	if sizes[len(sizes)-1] != wantLast {
		t.Fatalf("last batch %d, want %d", sizes[len(sizes)-1], wantLast)
	}
}

func TestScanAllBatchedSqlite_EmptyTableReturnsAtOnce(t *testing.T) {
	b := newBatchedSqlite(t)
	ids, _, next, err := collectBatched(t, b, 10, context.Background())
	if err != nil {
		t.Fatalf("collectBatched on empty: %v", err)
	}
	if len(ids) != 0 || next != "" {
		t.Fatalf("empty table: ids=%v next=%q, want empty", ids, next)
	}
	// 回调一次都不应被调用（next=="" 立即返回）
	called := false
	n2, err := b.ScanAllBatched(context.Background(), 10, "", func([]*Memory) error {
		called = true
		return nil
	})
	if err != nil || n2 != "" || called {
		t.Fatalf("empty table direct call: next=%q err=%v called=%v", n2, err, called)
	}
}

func TestScanAllBatchedSqlite_CallbackErrorStops(t *testing.T) {
	b := newBatchedSqlite(t)
	for i := 0; i < 25; i++ {
		batchSeed(t, b, "sb-e-"+pad3(i))
	}
	sentinel := os.ErrInvalid // 任一哨兵错误即可
	var seen int
	failOnSecond := func(batch []*Memory) error {
		seen += len(batch)
		if seen >= 15 { // 第二批（累计 15+）失败
			return sentinel
		}
		return nil
	}
	// 第一批成功：返回下一批游标
	next, err := b.ScanAllBatched(context.Background(), 10, "", failOnSecond)
	if err != nil {
		t.Fatalf("first batch should succeed: %v", err)
	}
	if next != "sb-e-009" {
		t.Fatalf("first next = %q, want sb-e-009", next)
	}
	// 第二批回调失败：原样回传入参游标（失败的批次可原样重试，不丢不重）
	resume, err := b.ScanAllBatched(context.Background(), 10, next, failOnSecond)
	if err != sentinel {
		t.Fatalf("err = %v, want sentinel %v", err, sentinel)
	}
	if resume != next {
		t.Fatalf("failed batch must resume at input cursor: got %q, want %q", resume, next)
	}
}

func TestScanAllBatchedSqlite_ConcurrentWritesNoLossNoDup(t *testing.T) {
	b := newBatchedSqlite(t)
	const seeded = 40
	for i := 0; i < seeded; i++ {
		batchSeed(t, b, "sb-a-"+pad3(i))
	}
	// 并发写入 20 条 id 全部大于已 seed 区间的记录（keyset 快照语义：
	// 期内新插入且游标未过的记录允许被后续批次看到；断言只要求
	// 「输出无重复 + 初始 40 条一条不丢 + 每条至多出现一次」）。
	// 注意：t.Fatalf 不得在非测试 goroutine 调用，写侧错误先攒着、回主 goroutine 判。
	var (
		wg     sync.WaitGroup
		werr   error
		werrMu sync.Mutex
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			m := &Memory{
				MemoryID: "sb-z-new-" + pad3(i), IdempotencyKey: "sb-z-new-" + pad3(i),
				TenantID: "t1", UserID: "u1",
				Visibility: VisPrivate, MemoryKind: KindFact,
				Fact: "fact-new",
			}
			if err := b.Upsert(context.Background(), m); err != nil {
				werrMu.Lock()
				if werr == nil {
					werr = err
				}
				werrMu.Unlock()
				return
			}
		}
	}()
	ids, _, _, err := collectBatched(t, b, 5, context.Background())
	wg.Wait()
	if werr != nil {
		t.Fatalf("concurrent upsert: %v", werr)
	}
	if err != nil {
		t.Fatalf("collectBatched: %v", err)
	}
	assertNoDupStrictlyAscending(t, ids)
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for i := 0; i < seeded; i++ {
		if !seen["sb-a-"+pad3(i)] {
			t.Fatalf("seeded id sb-a-%s lost during concurrent writes", pad3(i))
		}
	}
	// 新写入项被观察到时也必须无重复（已在 assertNoDup 覆盖）
}

func TestScanAllBatchedSqlite_ParityWithScanAll(t *testing.T) {
	b := newBatchedSqlite(t)
	const n = 37 // 故意非批长整数倍，逼出「最后不满批」路径
	for i := 0; i < n; i++ {
		batchSeed(t, b, "sb-p-"+pad3(i))
	}
	// 批量趟：分页器循环收集 id→fact
	batched := map[string]string{}
	for after := ""; ; {
		next, err := b.ScanAllBatched(context.Background(), 7, after, func(batch []*Memory) error {
			for _, m := range batch {
				batched[m.MemoryID] = m.Fact
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ScanAllBatched(after=%q): %v", after, err)
		}
		if next == "" {
			break
		}
		after = next
	}
	// 全量趟：ScanAll 原语义（契约未动，作为比对面）
	all, err := b.ScanAll(context.Background(), 5000)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(all) != n || len(batched) != n {
		t.Fatalf("count mismatch: batched=%d scanall=%d want %d", len(batched), len(all), n)
	}
	for _, m := range all {
		if f, ok := batched[m.MemoryID]; !ok || f != m.Fact {
			t.Fatalf("id %s: batched fact %q (present=%v) != scanall %q", m.MemoryID, f, ok, m.Fact)
		}
	}
}

func TestScanAllBatchedSqlite_ContextCanceled(t *testing.T) {
	b := newBatchedSqlite(t)
	for i := 0; i < 30; i++ {
		batchSeed(t, b, "sb-c-"+pad3(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.ScanAllBatched(ctx, 10, "", func([]*Memory) error { return nil })
	if err == nil {
		t.Fatalf("canceled ctx should return error, got nil")
	}
}

func TestScanAllBatchedSqlite_MaxDefaultsWhenNonPositive(t *testing.T) {
	b := newBatchedSqlite(t)
	for i := 0; i < 5; i++ {
		batchSeed(t, b, "sb-d-"+pad3(i))
	}
	// max<=0 → 默认 200；小表下一轮扫完且结果正确
	ids, _, _, err := collectBatched(t, b, 0, context.Background())
	if err != nil {
		t.Fatalf("collectBatched max=0: %v", err)
	}
	if len(ids) != 5 {
		t.Fatalf("default-batch scan got %d ids, want 5", len(ids))
	}
}

func TestScanAllBatchedSqlite_NilCallbackRejected(t *testing.T) {
	b := newBatchedSqlite(t)
	if _, err := b.ScanAllBatched(context.Background(), 10, "", nil); err == nil {
		t.Fatalf("nil callback should error, got nil")
	}
}

// ------------------------------------------------------------------
// PG 真机往返（LIGHTMEM_PG_HOST 未设 → t.Skip；与 gate [11] 同口径）
// ------------------------------------------------------------------

// bootstrapBatchedPgSchema 用生产迁移脚本幂等建表（只读 migrations/，不修改——红线②）。
func bootstrapBatchedPgSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("migrations", "001_pgvector.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
}

func TestScanAllBatchedPg_KeysetOrderAndNoDup(t *testing.T) {
	cfg := loadPgEdgeTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping real PG batched-scan e2e")
	}
	db := openPgEdgeDB(t, cfg)
	bootstrapBatchedPgSchema(t, db)

	const n = 23 // 非批长整数倍（max=6）
	prefix := "r29db-"
	for i := 0; i < n; i++ {
		id := prefix + pad3(i)
		m := &Memory{
			MemoryID: id, IdempotencyKey: id,
			TenantID: "t1", UserID: "u1",
			Visibility: VisPrivate, MemoryKind: KindFact,
			Fact: "pg-fact-" + id,
		}
		if _, err := db.Exec(`INSERT INTO memories (memory_id, idempotency_key, tenant_id, user_id,
			visibility, memory_kind, fact, recorded_at, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7, now(), 1)
			ON CONFLICT (memory_id) DO UPDATE SET fact = EXCLUDED.fact`,
			id, id, "t1", "u1", string(VisPrivate), string(KindFact), m.Fact); err != nil {
			t.Fatalf("pg seed %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM memories WHERE memory_id LIKE $1`, prefix+"%")
	})

	b := &pgBackend{db: db, table: "memories"}
	// 共享测试库（gate.env: lightmem_r28_gate）可能含其他用例的数据，
	// 而 ScanAllBatched 是全表游标（§10 无过滤参数）——回调内按前缀过滤后
	// 再断言「不丢不重 + 严格升序」。
	var (
		ids  []string
		next = ""
	)
	for {
		var batchPrefix []string
		n2, err := b.ScanAllBatched(context.Background(), 6, next, func(batch []*Memory) error {
			for _, m := range batch {
				if strings.HasPrefix(m.MemoryID, prefix) {
					batchPrefix = append(batchPrefix, m.MemoryID)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("pg ScanAllBatched: %v", err)
		}
		ids = append(ids, batchPrefix...)
		if n2 == "" {
			break
		}
		next = n2
	}
	if len(ids) != n {
		t.Fatalf("pg collected %d prefix ids, want %d (不丢不重)", len(ids), n)
	}
	assertNoDupStrictlyAscending(t, ids)
}

// ------------------------------------------------------------------
// 开关语义（默认关自证）
// ------------------------------------------------------------------

func TestMaintBatchEnabled_DefaultOff(t *testing.T) {
	// 未设 / 空 / 0 / false / 乱值 → 全部关（= 旧全表路径，L-249 仍开）
	for _, v := range []string{"", "0", "false", "off", "no", "banana"} {
		got := MaintBatchEnabled(func(k string) string {
			if k == "LIMS_MAINT_BATCH" {
				return v
			}
			return ""
		})
		if got {
			t.Fatalf("LIMS_MAINT_BATCH=%q should be OFF (default-off 红线④)", v)
		}
	}
	// getenv=nil → 关
	if MaintBatchEnabled(nil) {
		t.Fatalf("nil getenv should be OFF")
	}
	// 显式开的值
	for _, v := range []string{"1", "true", "TRUE", "Yes", "on"} {
		got := MaintBatchEnabled(func(k string) string {
			if k == "LIMS_MAINT_BATCH" {
				return v
			}
			return ""
		})
		if !got {
			t.Fatalf("LIMS_MAINT_BATCH=%q should be ON", v)
		}
	}
}

func TestMaintBatchSize_DefaultAndParsing(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == "LIMS_MAINT_BATCH_SIZE" {
				return v
			}
			return ""
		}
	}
	if got := MaintBatchSize(get("")); got != DefaultBatchSize {
		t.Fatalf("unset size = %d, want default %d", got, DefaultBatchSize)
	}
	if got := MaintBatchSize(nil); got != DefaultBatchSize {
		t.Fatalf("nil getenv size = %d, want default %d", got, DefaultBatchSize)
	}
	if got := MaintBatchSize(get("not-a-number")); got != DefaultBatchSize {
		t.Fatalf("garbage size = %d, want default %d", got, DefaultBatchSize)
	}
	if got := MaintBatchSize(get("-5")); got != DefaultBatchSize {
		t.Fatalf("negative size = %d, want default %d", got, DefaultBatchSize)
	}
	if got := MaintBatchSize(get("50")); got != 50 {
		t.Fatalf("size 50 = %d, want 50", got)
	}
	if got := MaintBatchSize(get("99999")); got != MaxBatchSize {
		t.Fatalf("oversize = %d, want capped %d", got, MaxBatchSize)
	}
}

func TestNormalizeBatchSize(t *testing.T) {
	if normalizeBatchSize(0) != DefaultBatchSize || normalizeBatchSize(-1) != DefaultBatchSize {
		t.Fatalf("non-positive should default to %d", DefaultBatchSize)
	}
	if normalizeBatchSize(50) != 50 {
		t.Fatalf("50 should pass through")
	}
	if normalizeBatchSize(MaxBatchSize+7) != MaxBatchSize {
		t.Fatalf("oversize should cap at %d", MaxBatchSize)
	}
}

func TestBatchedMemoryScannerImplementedByBothBackends(t *testing.T) {
	b := newBatchedSqlite(t)
	var _ BatchedMemoryScanner = b // sqlite：运行期也断言一次（编译期断言在实现文件）
	pb := &pgBackend{}
	var _ BatchedMemoryScanner = pb
}

// pad3 补零到 3 位（测试 id 生成用；保持字典序 = 数字序，便于断言）。
func pad3(i int) string {
	const digits = "0123456789"
	if i < 0 || i > 999 {
		panic("pad3 out of range")
	}
	return string([]byte{digits[i/100], digits[(i/10)%10], digits[i%10]})
}
