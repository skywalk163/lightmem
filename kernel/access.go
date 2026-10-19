// R24-C：访问模式确定性 + 成本感知晋升（3FS 分层）。
//
// 真源：lightmem.md 任务书 R24 §6「C 线」。
//
// 目标：补上 §0.1 第 2 条缺失的观测量——`AccessCount` / `RecentAccessScore`
// 在修复前**全仓只有读、没有写**（唯一赋值在测试夹具），导致
// `value = 0.5*importance + 0.3*0 + 0.2*confidence` 的访问维度完全失效。
//
// 三条设计原则：
//  1. **recall 不许变慢**：`Record` 纯内存原子计数，绝不碰数据库；写库只在
//     `Flush`（定时器 / 显式端点）时批量发生。
//  2. **recent_access_score 必须衰减**，不能单调累加——否则"访问模式"退化成
//     "存在时长"（红线）。
//  3. **默认关、零回归**：`WAccessDeterminism=0` 且生命周期定时器未启用时，
//     既有 `NextLifecycleByValue` 输出逐位不变。
//
// 红线遵守：**不改 `lifecycle.go` / `temperature.go`**（保护名单）。
// 晋升增强放在本文件的新函数里，不碰既有 `PromotionPolicy` 定义。
package kernel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------
// C2 · 访问模式分类（"确定性"的量化定义）
// ------------------------------------------------------------------

// AccessStats 单条记忆的访问统计。
type AccessStats struct {
	Total      int64     `json:"total"`
	FastHits   int64     `json:"fast_hits"` // 查找表快车道命中
	SlowHits   int64     `json:"slow_hits"` // 融合路径命中
	LastAccess time.Time `json:"last_access"`
}

// AccessMode 访问模式分类。
type AccessMode string

const (
	// AccessDeterministic 确定性查找型：以查找表命中为主（适合静态快车道）。
	AccessDeterministic AccessMode = "deterministic"
	// AccessAssociative 稀疏联想型：以融合路径命中为主。
	AccessAssociative AccessMode = "associative"
	// AccessSparse 低频：样本不足，不足以判定。
	AccessSparse AccessMode = "sparse"
)

// AccessPolicy 分类阈值（可测、确定性）。
type AccessPolicy struct {
	// MinSamples 判定所需最小样本数，默认 5。Total < MinSamples → sparse。
	MinSamples int64
	// DeterministicRatio 快车道命中占比阈值，默认 0.8。
	// FastHits/Total ≥ 该值且 Total ≥ MinSamples → deterministic。
	DeterministicRatio float64
	// Tau 近期访问分衰减时间常数，默认 7 天。
	Tau time.Duration
}

// DefaultAccessPolicy 返回默认策略（与任务书 §6.1 C2 一致）。
func DefaultAccessPolicy() AccessPolicy {
	return AccessPolicy{MinSamples: 5, DeterministicRatio: 0.8, Tau: 7 * 24 * time.Hour}
}

// ClassifyAccessMode 确定性分类：
//
//	Total < MinSamples            → sparse
//	FastHits/Total ≥ ratio        → deterministic
//	否则                          → associative
func ClassifyAccessMode(stats AccessStats, p AccessPolicy) AccessMode {
	if p.MinSamples > 0 && stats.Total < p.MinSamples {
		return AccessSparse
	}
	if stats.Total <= 0 {
		return AccessSparse
	}
	ratio := p.DeterministicRatio
	if ratio <= 0 {
		ratio = 0.8
	}
	if float64(stats.FastHits)/float64(stats.Total) >= ratio {
		return AccessDeterministic
	}
	return AccessAssociative
}

// decayScore 衰减公式（红线：必须衰减，不许单调累加）：
//
//	new = old * exp(-Δt/τ) + 1
//
// τ <= 0 时退化为 new = old + 1（无衰减），仅供显式关闭衰减的场景。
// Δt <= 0 时视为同一时刻：new = old + 1。
func decayScore(old float64, dt, tau time.Duration) float64 {
	if tau <= 0 {
		return old + 1
	}
	if dt <= 0 {
		return old + 1
	}
	return old*math.Exp(-float64(dt)/float64(tau)) + 1
}

// ------------------------------------------------------------------
// C1 · 访问追踪器（L-223）
// ------------------------------------------------------------------

// AccessTracker 纯内存访问追踪器；写库只在 Flush 时批量发生。
//
// 并发模型：`Record` 与 `Flush` 可并发——Flush 只**取走快照**（swap 出新 map），
// 不阻塞 Record；因此 Flush 期间到达的计数不会丢（留到下一次 Flush）。
type AccessTracker struct {
	mu       sync.Mutex
	flushMu  sync.Mutex // 串行化 Flush 的 read-modify-write
	capacity int
	tau      time.Duration

	counts map[string]int64     // memory_id -> 待落库的访问次数
	last   map[string]time.Time // memory_id -> 最近一次访问时刻
	stats  map[string]*AccessStats
}

// NewAccessTracker 构造追踪器。capacity<=0 表示不限制待落库条目数。
func NewAccessTracker(capacity int) *AccessTracker {
	return &AccessTracker{
		capacity: capacity,
		tau:      DefaultAccessPolicy().Tau,
		counts:   make(map[string]int64),
		last:     make(map[string]time.Time),
		stats:    make(map[string]*AccessStats),
	}
}

// SetTau 设置衰减时间常数（默认 7 天）。
func (t *AccessTracker) SetTau(tau time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tau = tau
}

// Record 记录一次访问（**纯内存，绝不碰数据库**）。
func (t *AccessTracker) Record(memoryID string, now time.Time) {
	if t == nil || memoryID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.capacity > 0 && len(t.counts) >= t.capacity {
		// 容量上限：丢弃最早的记录（按 last 升序）以防无界增长。
		t.evictOldestLocked(1)
	}
	t.counts[memoryID]++
	t.last[memoryID] = now
	st := t.stats[memoryID]
	if st == nil {
		st = &AccessStats{}
		t.stats[memoryID] = st
	}
	st.Total++
	st.SlowHits++
	st.LastAccess = now
}

// MarkFast 把一批 id 的命中记为**快车道**命中（供 C2 分类用）。
//
// 接缝说明：A 线在 `RecallAndFuse` 的两个返回点都调用 `s.NoteRecall(ids)`，
// 无法区分快/慢车道（`router_mem.go` 归 A 线独占，C 线不得改）。因此默认经
// `NoteRecall` 的命中记为 SlowHits；若未来 A 线把快车道那一行改为调用
// `MarkFast`，本追踪器即可自动区分。该限制已在交付报告登记（非静默缺陷）。
func (t *AccessTracker) MarkFast(ids []string) {
	if t == nil {
		return
	}
	now := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		st := t.stats[id]
		if st == nil {
			st = &AccessStats{}
			t.stats[id] = st
		}
		st.Total++
		st.FastHits++
		st.LastAccess = now
		t.counts[id]++
		t.last[id] = now
	}
}

// NoteRecall 满足 A 线预留的接缝（`APIServer.NoteRecall` 会对
// `interface{ NoteRecall([]string) }` 做断言并转发到这里）。
func (t *AccessTracker) NoteRecall(ids []string) {
	if t == nil {
		return
	}
	now := time.Now().UTC()
	for _, id := range ids {
		t.Record(id, now)
	}
}

// Pending 返回待落库的 memory_id 数（判据：Record 不写库）。
func (t *AccessTracker) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.counts)
}

// StatsOf 返回某条记忆的访问统计（副本）。
func (t *AccessTracker) StatsOf(memoryID string) AccessStats {
	if t == nil {
		return AccessStats{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.stats[memoryID]; st != nil {
		return *st
	}
	return AccessStats{}
}

// ModeOf 返回某条记忆的访问模式分类。
func (t *AccessTracker) ModeOf(memoryID string, p AccessPolicy) AccessMode {
	return ClassifyAccessMode(t.StatsOf(memoryID), p)
}

// flushSnapshot 取走待落库快照（swap），保证 Flush 不阻塞 Record。
func (t *AccessTracker) flushSnapshot() (map[string]int64, map[string]time.Time, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts, last := t.counts, t.last
	t.counts = make(map[string]int64, len(counts))
	t.last = make(map[string]time.Time, len(last))
	return counts, last, t.tau
}

// Flush 把待落库计数写回后端：批量 Get → 改 → Upsert。
// 返回成功写回的条数。并发安全：
//   - 与 Record 并发：只取快照（swap），不阻塞 Record，Flush 期间到达的计数留到下次；
//   - **Flush 之间**：必须串行（flushMu）。否则多个 Flush 会各自做
//     read-modify-write，后者的 Upsert 覆盖前者 → 计数丢失（实测丢 25%，
//     由 TestAccessConcurrentRecordAndFlush 抓到）。真实场景虽是单定时器单
//     实例，但显式端点 + 定时器可能同时触发，故在此强制串行。
func (t *AccessTracker) Flush(ctx context.Context, backend StorageBackend, tenant string) (int, error) {
	if t == nil || backend == nil {
		return 0, fmt.Errorf("access flush: nil tracker or backend")
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	counts, last, tau := t.flushSnapshot()
	if len(counts) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 确定性顺序（便于测试与日志比对）

	now := time.Now().UTC()
	flushed := 0
	for _, id := range ids {
		m, err := backend.Get(ctx, id, "", tenant)
		if err != nil || m == nil {
			continue
		}
		n := counts[id]
		if n <= 0 {
			continue
		}
		m.AccessCount += n
		// 衰减：以「上次访问 → 现在」的间隔衰减一次，再 += n。
		// 多次访问在同一次 Flush 内合并：先按首次间隔衰减，再累加 n 次 +1。
		dt := now.Sub(m.RecordedAt)
		if at, ok := last[id]; ok && !at.IsZero() {
			dt = now.Sub(at)
		}
		score := decayScore(m.RecentAccessScore, dt, tau)
		// 同一批内 n>1：其余 n-1 次视为同一时刻（Δt=0）累加。
		for i := int64(1); i < n; i++ {
			score = decayScore(score, 0, tau)
		}
		m.RecentAccessScore = score
		// 说明：Memory 无 last_accessed_at 字段（只增不改 Schema，红线），
		// 最近访问时刻由追踪器内存侧维护（t.last），衰减以它为 Δt 起点。
		if err := backend.Upsert(ctx, m); err != nil {
			continue
		}
		flushed++
	}
	return flushed, nil
}

// evictOldestLocked 丢弃 last 最早的 n 条（调用方持锁）。
func (t *AccessTracker) evictOldestLocked(n int) {
	type kv struct {
		id   string
		when time.Time
	}
	all := make([]kv, 0, len(t.last))
	for id, w := range t.last {
		all = append(all, kv{id, w})
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].when.Equal(all[j].when) {
			return all[i].when.Before(all[j].when)
		}
		return all[i].id < all[j].id
	})
	for i := 0; i < n && i < len(all); i++ {
		delete(t.counts, all[i].id)
		delete(t.last, all[i].id)
	}
}

// SetAccessTracker 注入访问追踪器（C 线 setter，写在 access.go，不改 router.go）。
// 注入后 A 线预留的 `s.NoteRecall(...)` 即由本追踪器承接。
func (s *APIServer) SetAccessTracker(t *AccessTracker) { s.access = t }

// ------------------------------------------------------------------
// C3 · 成本感知晋升（不碰 lifecycle.go）
// ------------------------------------------------------------------

// AccessAwarePolicy 成本感知晋升的附加权重（C3）。
//
// 为什么不在既有 `PromotionPolicy` 上加字段：红线规定**不许改 `lifecycle.go`**
// （保护名单 + 既有语义）。故本线在自己的新文件里定义附加策略，
// `WAccessDeterminism == 0` 时增强函数逐位等于原 `NextLifecycleByValue`。
type AccessAwarePolicy struct {
	// WAccessDeterminism 确定性权重，默认 0（**零回归**）。
	// >0 时对 deterministic 记忆降低晋升阈值（更容易升层、更难降层）。
	WAccessDeterminism float64
	// PromoteBonus 对 deterministic 记忆的晋升阈值下调量（默认 0.1）。
	PromoteBonus float64
	// DemoteGuard 对 deterministic 记忆的降层阈值下调量（默认 0.1，即更难降）。
	DemoteGuard float64
}

// DefaultAccessAwarePolicy 默认全 0 → 逐位等于既有行为。
func DefaultAccessAwarePolicy() AccessAwarePolicy { return AccessAwarePolicy{} }

// NextLifecycleByValueWithAccess 在既有 `NextLifecycleByValue` 之上叠加
// 确定性偏袒；`WAccessDeterminism == 0` 时**逐位等于**原函数。
func NextLifecycleByValueWithAccess(m *Memory, p PromotionPolicy, ap AccessAwarePolicy, mode AccessMode, now time.Time) (from, to MemoryLayer, reason string) {
	bf, bt, br := NextLifecycleByValue(m, p, now)
	if ap.WAccessDeterminism == 0 || mode != AccessDeterministic || m == nil {
		return bf, bt, br
	}
	// 对确定性记忆：降低晋升阈值（更易升）、降低降层阈值（更难降）。
	eased := p
	if ap.PromoteBonus > 0 {
		eased.ValueL0ToL1 -= ap.PromoteBonus
		eased.ValueL1ToL2 -= ap.PromoteBonus
	}
	if ap.DemoteGuard > 0 {
		eased.ValueL2Demote -= ap.DemoteGuard
		eased.ValueL2ToL3Low -= ap.DemoteGuard
	}
	gf, gt, gr := NextLifecycleByValue(m, eased, now)
	if gf == bf && gt == bt && gr == br {
		return bf, bt, br
	}
	return gf, gt, gr
}

// ------------------------------------------------------------------
// C3 · 死引擎接线（默认关）
// ------------------------------------------------------------------

// LifecycleEngines 可单独触发的引擎名（任务书 §6.1 C3）。
const (
	EngineBudget      = "budget"
	EngineForget      = "forget"
	EngineCore        = "core"
	EngineArchive     = "archive"
	EngineCompress    = "compress"
	EngineAccessFlush = "access_flush"
)

// LifecycleConfig 死引擎运行参数（零值 = 关）。
type LifecycleConfig struct {
	BudgetLimits    map[MemoryLayer]int
	ForgetThreshold float64
	CoreAccessMin   int64
	ArchiveIdle     time.Duration
	CompressSim     float32
}

// RunLifecycleEngines 按名字调用既有引擎（**它们此前零生产调用点**）。
// 返回 引擎名 -> 影响条数。默认不启用任何定时器，只由本函数（显式端点/定时器）触发。
func RunLifecycleEngines(ctx context.Context, backend StorageBackend, embedder func(string) ([]float32, error), tenant string, engines []string, cfg LifecycleConfig) (map[string]int, error) {
	out := make(map[string]int)
	if backend == nil {
		return nil, fmt.Errorf("lifecycle: backend is nil")
	}
	for _, name := range engines {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case EngineBudget:
			if cfg.BudgetLimits == nil {
				out[EngineBudget] = 0
				continue
			}
			n, err := BudgetManager(ctx, backend, cfg.BudgetLimits, tenant)
			if err != nil {
				return out, fmt.Errorf("budget: %w", err)
			}
			out[EngineBudget] = n
		case EngineForget:
			if cfg.ForgetThreshold <= 0 {
				out[EngineForget] = 0
				continue
			}
			n, err := ForgettingEngine(ctx, backend, cfg.ForgetThreshold, tenant)
			if err != nil {
				return out, fmt.Errorf("forget: %w", err)
			}
			out[EngineForget] = n
		case EngineCore:
			if cfg.CoreAccessMin <= 0 {
				out[EngineCore] = 0
				continue
			}
			n, err := CoreMemoryManager(ctx, backend, cfg.CoreAccessMin, tenant)
			if err != nil {
				return out, fmt.Errorf("core: %w", err)
			}
			out[EngineCore] = n
		case EngineArchive:
			if cfg.ArchiveIdle <= 0 {
				out[EngineArchive] = 0
				continue
			}
			n, err := ArchiveEngine(ctx, backend, cfg.ArchiveIdle, tenant)
			if err != nil {
				return out, fmt.Errorf("archive: %w", err)
			}
			out[EngineArchive] = n
		case EngineCompress:
			if embedder == nil || cfg.CompressSim <= 0 {
				out[EngineCompress] = 0
				continue
			}
			n, err := CompressionEngine(ctx, backend, embedder, cfg.CompressSim, tenant)
			if err != nil {
				return out, fmt.Errorf("compress: %w", err)
			}
			out[EngineCompress] = n
		default:
			return out, fmt.Errorf("unknown lifecycle engine %q", name)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------
// C3 · 路由自注册（默认不启用）
// ------------------------------------------------------------------

var lifecycleServer *APIServer

// RegisterLifecycleRoutes 注册 `POST /admin/lifecycle/run?engines=...&tenant_id=...`。
// 与 B 线同理：走 T0 预埋的 `RegisterRoutes` 自注册，**不改 router.go**。
func RegisterLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/lifecycle/run", func(w http.ResponseWriter, r *http.Request) {
		s := lifecycleServer
		if s == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lifecycle routes not bound to an APIServer"})
			return
		}
		q := r.URL.Query()
		engines := strings.Split(q.Get("engines"), ",")
		tenant := q.Get("tenant_id")
		cfg := LifecycleConfig{
			BudgetLimits:    parseBudgetLimits(q.Get("budget_limits")),
			ForgetThreshold: atofOr(q.Get("forget_threshold"), 0),
			CoreAccessMin:   int64(atoiOr(q.Get("core_access_min"), 0)),
			ArchiveIdle:     parseDurationOr(q.Get("archive_idle"), 0),
			CompressSim:     float32(atofOr(q.Get("compress_sim"), 0)),
		}
		rep, err := RunLifecycleEngines(r.Context(), s.backend, s.embedder, tenant, engines, cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"moved": rep, "tenant": tenant})
	})
}

// EnableLifecycleRoutes 启用生命周期端点（**默认不启用**）。
// 由 `cmd/lightmem-kernel/main.go`（C 线独占域）按 `LIMS_LIFECYCLE_INTERVAL`
// 或显式开关调用。
func EnableLifecycleRoutes(s *APIServer) {
	if s == nil {
		return
	}
	lifecycleServer = s
	RegisterRoutes(RegisterLifecycleRoutes)
}

// parseBudgetLimits 解析形如 "L0=100,L1=500" 的层上限；空串返回 nil（引擎不跑）。
func parseBudgetLimits(s string) map[MemoryLayer]int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := make(map[MemoryLayer]int)
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[MemoryLayer(strings.TrimSpace(kv[0]))] = atoiOr(kv[1], 0)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseDurationOr 解析时长（如 "8760h"），失败返回 def。
func parseDurationOr(s string, def time.Duration) time.Duration {
	if strings.TrimSpace(s) == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// atofOr / atoiOr 复用包内既有 atoi，这里补一个浮点版与带默认值版。
func atofOr(s string, def float64) float64 {
	if strings.TrimSpace(s) == "" {
		return def
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return def
	}
	return v
}

func atoiOr(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return atoi(s, def)
}
