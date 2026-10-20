// 健康探针（R29-B 线：L2 健康硬化）。
//
// 职责边界（与 B/C/D/E 线的分工，见任务书 §2.2 / §10 / 设计稿 §3.2）：
//   - LivezHandler：存活探针，只回答「进程在不在」，不碰任何依赖（最快）。liveness 语义。
//   - ReadyzHandler：就绪探针，回答「现在能不能接流量」。判据 =
//     backend.Ping + 轻量读探针（SELECT 1 FROM memories LIMIT 1，空表合法）
//     并带 TTL 缓存（HTTP 只读缓存，实际探测由后台 goroutine 周期执行），
//     失败 503。响应最小化：仅 {status, check}。
//
// R29-B 相对 T0-S0 的最小实现（见 _R29_T0交付报告.md）硬化点：
//  1. 读探针：Ping 之后追加 SELECT 1 FROM memories LIMIT 1（修 L-244：PG 缺表时不再 200）；
//  2. TTL 缓存 1s + 后台周期探测（避免 LB 每 1s 探针抢 Solo 档全局 mu）；
//  3. 探针预算 ≤2s（ctx 超时，与 E 线 busy_timeout 对齐口径，见交付报告）；
//  4. 响应最小化 {"status":"ok|degraded","check":"..."}，不输出 profile/version/DSN/后端名；
//  5. 探针耗时与失败分类落指标（backend_probe_ms / readyz_failures_total{check}）。
//
// 红线：本文件只用 stdlib（含 database/sql 类型断言），不引入第三方依赖（红线③）。
// 只读：读探针为纯 SELECT，不写库；不改 StorageBackend 契约（红线⑦）。
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// ── 就绪探针时序常量（设计稿 §3.2.2）──

const (
	// readyzTTL 是就绪结果 TTL 缓存窗口（1–2s）。HTTP 只读缓存；后台按该周期重探。
	readyzTTL = 1 * time.Second
	// readyzBudget 是单次探针预算上限（≤2s，与 server_metrics.go HealthzHandler 的 2s 一致）。
	readyzBudget = 2 * time.Second
	// readyzStaleFactor：缓存年龄超过 readyzTTL*factor 视为失效，HTTP 侧同步兜底重探，
	// 避免后台 goroutine 异常停摆时向 LB 谎报新鲜状态。
	readyzStaleFactor = 2
)

// LivezHandler 存活探针：立即返回 200。不触达数据库/嵌入器，零依赖、最低延迟。
func LivezHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "alive"})
	})
}

// ReadinessProbe 就绪检查契约（§10 冻结）。
type ReadinessProbe interface {
	Check(ctx context.Context) error
}

// backendProbe 实现就绪探测：Ping + 轻量读探针（R29-B）。
type backendProbe struct {
	backend StorageBackend
	name    string
}

// NewBackendProbe 构造后端就绪探针（§10 冻结签名）。
func NewBackendProbe(backend StorageBackend, name string) ReadinessProbe {
	return &backendProbe{backend: backend, name: name}
}

// Check 探活：Ping 不通 → 未就绪；Ping 通后追加轻量读探针。
// 返回的 error 为 *probeCheckError（携带失败分类，供 readyz_failures_total{check} 使用）。
func (p *backendProbe) Check(ctx context.Context) error {
	if p.backend == nil {
		return errNilBackend
	}
	if err := p.backend.Ping(ctx); err != nil {
		return &probeCheckError{check: "ping", err: err}
	}
	if err := readProbe(ctx, p.backend); err != nil {
		return &probeCheckError{check: "read", err: err}
	}
	return nil
}

// readProbe 轻量读探针：SELECT 1 FROM memories LIMIT 1。
//
// 语义（设计稿 §3.2.2）：
//   - 空表 → sql.ErrNoRows → 视为合法（返回 nil，端点 200）；
//   - 表缺失 / 连接断 → 返回 err（端点 503，修 L-244、复现 R7）；
//   - 不扫全表（只 LIMIT 1，避开 backend_sqlite.go loadAll）。
//
// 后端访问：仅 sqliteBackend / pgBackend 有本表的 *sql.DB；其余后端（含 test 桩）
// 无读探针，退化为仅 Ping（返回 nil）。sqlite 为单连接 + 全局 mu，读探针须持 mu
// 与业务串行（TTL 缓存保证不会按 LB 频率抢锁）。
func readProbe(ctx context.Context, backend StorageBackend) error {
	var db *sql.DB
	switch b := backend.(type) {
	case *sqliteBackend:
		if b == nil {
			return nil
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		db = b.db
	case *pgBackend:
		if b == nil {
			return nil
		}
		db = b.db
	default:
		return nil
	}
	if db == nil {
		return nil
	}
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM memories LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 空表合法
	}
	return err
}

// errNilBackend 在探针构造时传入 nil 后端时返回。
var errNilBackend = &probeError{msg: "backend is nil"}

// errNilProbe 在就绪探针为空时返回。
var errNilProbe = &probeError{msg: "readiness probe is nil"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }

// probeCheckError 携带失败分类（check）的探针错误。
type probeCheckError struct {
	check string
	err   error
}

func (e *probeCheckError) Error() string { return e.check + ": " + e.err.Error() }
func (e *probeCheckError) Unwrap() error { return e.err }

// probeCheckName 提取失败分类；无法识别时返回 "unknown"。
func probeCheckName(err error) string {
	var pce *probeCheckError
	if errors.As(err, &pce) && pce.check != "" {
		return pce.check
	}
	switch {
	case errors.Is(err, errNilBackend):
		return "backend"
	case errors.Is(err, errNilProbe):
		return "probe"
	}
	return "unknown"
}

// ── TTL 缓存（R29-B）──

// readyzCache 把就绪探测结果缓存 ttl，并由后台 goroutine 周期重探。
// HTTP handler 只读缓存；缓存失效时同步兜底重探一次（§3.2.2）。
type readyzCache struct {
	probe  ReadinessProbe
	ttl    time.Duration
	budget time.Duration

	mu       sync.RWMutex
	probed   bool
	ok       bool
	check    string
	probedAt time.Time

	stop     chan struct{}
	stopOnce sync.Once
}

// newReadyzCache 构造缓存并启动后台探测循环；构造时先同步探一次，保证首个请求有值。
func newReadyzCache(p ReadinessProbe, ttl, budget time.Duration) *readyzCache {
	if ttl <= 0 {
		ttl = readyzTTL
	}
	if budget <= 0 {
		budget = readyzBudget
	}
	c := &readyzCache{probe: p, ttl: ttl, budget: budget, stop: make(chan struct{})}
	c.runOnce(context.Background())
	go c.loop()
	return c
}

// loop 后台周期探测（TTL 间隔）。ticker 在探测进行中到达的 tick 会被丢弃（不重入）。
func (c *readyzCache) loop() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.runOnce(context.Background())
		}
	}
}

// stopLoop 停止后台探测循环（幂等，测试/生命周期用）。
func (c *readyzCache) stopLoop() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// runOnce 执行一次探测（带预算超时），更新缓存并落指标。
func (c *readyzCache) runOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, c.budget)
	defer cancel()

	start := time.Now()
	var err error
	if c.probe == nil {
		err = errNilProbe
	} else {
		err = c.probe.Check(ctx)
	}
	elapsedMS := float64(time.Since(start).Microseconds()) / 1000.0
	DefaultServerMetrics().ObserveBackendProbe(elapsedMS)

	c.mu.Lock()
	c.probed = true
	c.probedAt = time.Now()
	if err != nil {
		c.ok = false
		c.check = err.Error()
		c.mu.Unlock()
		DefaultServerMetrics().IncReadyzFailure(probeCheckName(err))
		return
	}
	c.ok = true
	c.check = ""
	c.mu.Unlock()
}

// status 读取缓存状态；fresh=false 表示从未探测或缓存已过期（调用方应同步兜底重探）。
func (c *readyzCache) status(now time.Time) (ok bool, check string, fresh bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.probed {
		return false, "", false
	}
	return c.ok, c.check, now.Sub(c.probedAt) <= c.ttl*readyzStaleFactor
}

// ReadyzHandler 就绪探针 HTTP 端点（§10 冻结签名）。
// 判据 = Ping + 读探针（TTL 缓存）；失败 → 503；响应体仅 {status, check}。
func ReadyzHandler(p ReadinessProbe) http.Handler {
	c := newReadyzCache(p, readyzTTL, readyzBudget)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, check, fresh := c.status(time.Now())
		if !fresh {
			// 缓存未就绪或已过期：同步兜底探测一次（预算内），不向 LB 谎报状态。
			c.runOnce(r.Context())
			ok, check, _ = c.status(time.Now())
		}
		if ok {
			writeReadyz(w, http.StatusOK, "ok", "")
			return
		}
		writeReadyz(w, http.StatusServiceUnavailable, "degraded", check)
	})
}

// writeReadyz 输出最小化就绪响应（仅 status 与 check 两个字段）。
func writeReadyz(w http.ResponseWriter, code int, status, check string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "check": check})
}
