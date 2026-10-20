// 批量游标扫描（R29-D · 修 L-249：Solo 档整理与在线请求争同一把锁 → P95 尖刺）。
//
// 设计真源：docs/永续运行架构设计_v1.1.md §3.3.3 + 任务书 §7.1.4 / §10。
//
// 只增不改（红线⑦）：
//   - 本文件**只新增** BatchedMemoryScanner 接口与两个后端的实现；
//     ScanAll / MemoryScanner 既有契约**一字不动**（backend_sqlite.go:739 /
//     backend_pg.go:628 的 ScanAll 原样保留，本文件不触碰那两个文件——它们
//     归 E 线独占，见任务书 §2.2 文件域）。
//   - keyset 游标按 memory_id 推进（**不用 OFFSET**：并发写会漏/重）。
//   - 每批受锁读取后**出锁**再反序列化 + 回调 + 让位（runtime.Gosched()
//   - time.Sleep），否则「让出锁」不成立。
//
// 默认关（红线④）：
//   - LIMS_MAINT_BATCH 未设 → MaintBatchEnabled=false → 消费方（C 线
//     consolidate.go）走旧全表路径，**此时 L-249 仍开**（R13 必须注明开关）。
//   - 本文件不读 os.Getenv（与 profile.go 的注入式口径一致）：开关判定经
//     MaintBatchEnabled/MaintBatchSize 的 getenv 参数注入，测试可伪造。
//
// 红线③：只用 stdlib。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// BatchedMemoryScanner 批量游标扫描接口（§10 冻结；设计稿 §3.3.3）。
//
// 分页器协议（按 (after, next) 游标对）：
//   - 每次调用返回**一批**（≤max 条），对 fn 回调该批一次；
//   - next 为下一批起点游标；**next=="" 表示扫描结束**；
//   - fn 返回错误时原样回传**入参** after（失败的批次可重试，不丢不重）；
//   - ctx 取消 → 回传 (after, ctx.Err())。
//
// 调用方（如 C 线 consolidate.RunOnce）自行循环：每批之间可检查 ctx/预算，
// 与「单轮 ctx 截止」「预算用尽优雅让位」天然对齐。
type BatchedMemoryScanner interface {
	ScanAllBatched(ctx context.Context, max int, after string, fn func([]*Memory) error) (next string, err error)
}

// 编译期断言：两个后端都实现冻结接口（缺一个即编译失败）。
var (
	_ BatchedMemoryScanner = (*sqliteBackend)(nil)
	_ BatchedMemoryScanner = (*pgBackend)(nil)
)

const (
	// DefaultBatchSize 默认批量（§7.1.4「每批 ≤200 条」）。
	DefaultBatchSize = 200
	// MaxBatchSize 防御性上界：调用方/环境变量误配超大值时，防止一批
	// 持锁过久把在线请求饿死（超出部分静默收敛到该值，报告中有说明）。
	MaxBatchSize = 1000
	// batchYieldSleep 批次间让位睡眠（§7.1.4「1-5ms」取中）。
	batchYieldSleep = 2 * time.Millisecond

	// envMaintBatch     批量扫描总开关（§10）。未设/0/空 = 关 = 旧全表路径。
	envMaintBatch = "LIMS_MAINT_BATCH"
	// envMaintBatchSize 批量大小（§10）。未设/非法 = DefaultBatchSize。
	envMaintBatchSize = "LIMS_MAINT_BATCH_SIZE"
)

// MaintBatchEnabled 判定是否启用批量扫描（默认关自证入口）。
// 语义：仅 1/true/yes/on（大小写不敏感）为开；未设/其他值 = 关。
func MaintBatchEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(envMaintBatch))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// MaintBatchSize 读批量大小（未设/非法/越界 → DefaultBatchSize；>MaxBatchSize 收敛到 MaxBatchSize）。
func MaintBatchSize(getenv func(string) string) int {
	if getenv == nil {
		return DefaultBatchSize
	}
	v := strings.TrimSpace(getenv(envMaintBatchSize))
	if v == "" {
		return DefaultBatchSize
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return DefaultBatchSize
	}
	if n > MaxBatchSize {
		return MaxBatchSize
	}
	return n
}

// normalizeBatchSize 收敛批量参数：<=0 → 默认 200；>MaxBatchSize → MaxBatchSize。
func normalizeBatchSize(max int) int {
	if max <= 0 {
		return DefaultBatchSize
	}
	if max > MaxBatchSize {
		return MaxBatchSize
	}
	return max
}

// ------------------------------------------------------------------
// SQLite（Solo 档）：单连接 + 全局 mu，每批一次受锁 keyset 读取
// ------------------------------------------------------------------

// ScanAllBatched keyset 分批扫描一次（§10 冻结签名；修 L-249）。
//
// 分页器协议：每次调用返回一批（≤max），next 为下一批游标（""=扫完）。
// 与 loadAll 级读写的差别只在「批大小 + 批间让位」：批内仍是
// `SELECT memory_id, data … WHERE memory_id > ? ORDER BY memory_id LIMIT ?`
// （memory_id 是 PRIMARY KEY，走索引，无 OFFSET 全表扫）。
// 持锁只包住「读一批」；反序列化、回调、让位都在锁外。
func (b *sqliteBackend) ScanAllBatched(ctx context.Context, max int, after string, fn func([]*Memory) error) (string, error) {
	if fn == nil {
		return after, fmt.Errorf("scan_batched: nil callback")
	}
	max = normalizeBatchSize(max)
	if err := ctx.Err(); err != nil {
		return after, err
	}
	// 受锁读取一批（原子行；反序列化移到锁外，压缩持锁时间）。
	var (
		ids   []string
		datas []string
	)
	b.mu.Lock()
	rows, err := b.db.QueryContext(ctx,
		`SELECT memory_id, data FROM memories WHERE memory_id > ? ORDER BY memory_id LIMIT ?`,
		after, max)
	if err == nil {
		for rows.Next() {
			var id, data string
			if serr := rows.Scan(&id, &data); serr != nil {
				err = serr
				break
			}
			ids = append(ids, id)
			datas = append(datas, data)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close() // 单连接：必须在本批持锁期内关闭，否则下一批拿不到连接
	}
	b.mu.Unlock()
	if err != nil {
		return after, fmt.Errorf("sqlite scan batched: %w", err)
	}
	if len(ids) == 0 {
		return "", nil // 扫完：next 必须为 ""（§10 契约）
	}
	// 出锁后：反序列化 → 回调 → 让位（L-249 的核心：在线请求此刻可抢到 mu）。
	mems := make([]*Memory, 0, len(ids))
	for i, id := range ids {
		var m Memory
		if uerr := json.Unmarshal([]byte(datas[i]), &m); uerr != nil {
			return after, fmt.Errorf("sqlite scan batched unmarshal %s: %w", id, uerr)
		}
		mems = append(mems, &m)
	}
	next := ids[len(ids)-1]
	if err := fn(mems); err != nil {
		return after, err // 本批未消费：回传入参游标，调用方可原样重试
	}
	runtime.Gosched()
	time.Sleep(batchYieldSleep) // 批间让位 1-5ms（§7.1.4）
	return next, nil
}

// ------------------------------------------------------------------
// PG（pgvector 档）：连接池天然并行，批间靠 rows.Close 归还连接 + 让位
// ------------------------------------------------------------------

// pgScanBatchedCols 与 backend_pg.go ScanAll 的列清单逐字一致（只增不改：
// 不新引列、不改列序），仅 WHERE/ORDER BY/LIMIT 改为 keyset。
const pgScanBatchedCols = `memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
	visibility, shared_with_tenants, permission, permission_level,
	memory_kind, fact, reference_frame,
	occurred_at, recorded_at, valid_from, valid_until, superseded_by,
	confidence, source, version, importance, recent_access_score, access_count, expire_at,
	provenance, source_evidence, vector_status, is_historical,
	goal_detail, external_ref, layer`

// ScanAllBatched keyset 分批扫描一次（§10 冻结签名；修 L-249）。
// 分页器协议：每次调用返回一批（≤max），next 为下一批游标（""=扫完）。
// WHERE memory_id > $1 ORDER BY memory_id ASC LIMIT $2（参数化 LIMIT，
// PG 支持；无 OFFSET）。本批 scanMemoryRows 读完后 rows.Close 归还连接，
// 再执行回调与让位。
func (b *pgBackend) ScanAllBatched(ctx context.Context, max int, after string, fn func([]*Memory) error) (string, error) {
	if fn == nil {
		return after, fmt.Errorf("scan_batched: nil callback")
	}
	max = normalizeBatchSize(max)
	if err := ctx.Err(); err != nil {
		return after, err
	}
	query := `SELECT ` + pgScanBatchedCols + `
	FROM ` + b.table + `
	WHERE memory_id > $1
	ORDER BY memory_id ASC LIMIT $2`
	rows, err := b.db.QueryContext(ctx, query, after, max)
	if err != nil {
		return after, fmt.Errorf("pg scan batched: %w", err)
	}
	mems, serr := b.scanMemoryRows(rows)
	cerr := rows.Close() // 先归还连接，再回调
	if serr != nil {
		return after, fmt.Errorf("pg scan batched scan: %w", serr)
	}
	if cerr != nil {
		return after, fmt.Errorf("pg scan batched close: %w", cerr)
	}
	if len(mems) == 0 {
		return "", nil // 扫完：next 必须为 ""（§10 契约）
	}
	next := mems[len(mems)-1].MemoryID
	if err := fn(mems); err != nil {
		return after, err // 本批未消费：回传入参游标，调用方可原样重试
	}
	runtime.Gosched()
	time.Sleep(batchYieldSleep) // 批间让位 1-5ms（§7.1.4）
	return next, nil
}
