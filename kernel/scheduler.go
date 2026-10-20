// 维护调度器（R29-T0 S0 冻结签名 + R29-C 硬化：窗口 / 预算 / 单飞 / 退避）。
//
// 职责边界（任务书 §2.2 / §6 / §10）：
//   - T0（S0 接缝）已冻结签名：Report / Budget / Window / Backoff / MaintenanceJob /
//     MaintenanceConfig / StartMaintenance / Stop / Wait / DefaultMaintenanceJobs；
//     本文件**不改动任何冻结签名**，只在既有结构体上增补可选字段与方法。
//   - C 线硬化（§6.1）：窗口准入（Window.Allows）、单轮预算（Budget.MaxDuration 硬截止，
//     MaxItems/MaxWrites 软核对）、单飞去重（single-flight，未结束跳过并计数）、
//     退避重试（Backoff 1m→2m→4m…封顶 30m，连续失败 ≥MaxFails 告警不重启）。
//   - L-248 修复（§6.1.2）：LIMS_LIFECYCLE_INTERVAL 真起定时器（默认关）；
//     引擎集由 LIMS_LIFECYCLE_ENGINES 配置（默认 budget,forget,core,archive,compress）；
//     LastLifecycleRun() 提供时间戳 getter 供 B 线 /healthz.last_lifecycle_run 承接。
//   - consolidate（§6.1.3）：仍由 main.go 的 cons.Start 以默认 5min 间隔独立驱动
//     （零回归）；本文件提供 Consolidator.MaintenanceJob，需要时把它纳入调度器。
//
// 默认关（红线④）：不设任何 env 时 StartMaintenanceAuto 返回 nil；
// DefaultMaintenanceJobs 在 LifecycleInterval==0 且 Consolidate==false 时返回空列表，
// 不创建任何 job goroutine、不打 job 日志、不改任何数据 —— 与 c203db8 逐位一致。
//
// 指标（§10）：方法名与 B 线冻结的 *ServerMetrics 完全一致，经 SetMaintenanceMetrics
// 接线；未接线时 no-op，不新增指标注册表。
//
// 红线③：只用 stdlib，不引入第三方依赖。
package kernel

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------
// Report（§10 冻结，不变）
// ------------------------------------------------------------------

// Report 一轮维护作业的结果（§10 冻结）。
type Report struct {
	Job        string
	Items      int
	Writes     int
	DurationMS int64
	Notes      []string
}

// String 便于日志/调试。
func (r Report) String() string {
	return fmt.Sprintf("job=%s items=%d writes=%d dur=%dms notes=%v",
		r.Job, r.Items, r.Writes, r.DurationMS, r.Notes)
}

// ------------------------------------------------------------------
// Budget（§10 冻结）
// ------------------------------------------------------------------

// Budget 单轮资源预算（§10 冻结）。
//
// C 线生效口径（§6.1.1）：
//   - MaxDuration 是**硬截止**：调度器以此给每轮 ctx 加 deadline，作业必须尊重 ctx
//     （consolidate / 生命周期引擎都通过 ctx 让位）。
//   - MaxItems / MaxWrites 是**软核对**：作业完成后由调度器核对报告值，超限记入
//     Notes（不中断，因为引擎自身不消费计数预算）。
type Budget struct {
	MaxItems    int
	MaxDuration time.Duration
	MaxWrites   int
}

// ------------------------------------------------------------------
// Window（§10 冻结 + C 线判定方法）
// ------------------------------------------------------------------

// Window 允许运行的时间窗（§10 冻结；nil = 全时段）。
type Window struct {
	Start string
	End   string
	Days  []int
}

// Allows 判定 t 是否落在窗口内（§6.1.4）。
//
// 规则：
//   - Days 非空时，t 的星期（Go 约定 0=Sunday … 6=Saturday）必须在 Days 内；
//   - Start/End 形如 "HH:MM"（也接受 "HH:MM:SS"）；Start>End 视为跨午夜窗口
//     （如 22:00-02:00）；
//   - 端点语义：Start 含、End 不含（[Start, End)），避免相邻窗口重叠。
//   - 窗口配置非法时按全时段处理（fail-open：宁可多跑一轮维护，也不让维护静默停摆）。
func (w *Window) Allows(t time.Time) bool {
	if w == nil {
		return true
	}
	if len(w.Days) > 0 && !containsInt(w.Days, int(t.Weekday())) {
		return false
	}
	s, serr := parseHHMM(w.Start)
	e, eerr := parseHHMM(w.End)
	if serr != nil || eerr != nil {
		return true
	}
	cur := t.Hour()*3600 + t.Minute()*60 + t.Second()
	if s <= e {
		return cur >= s && cur < e
	}
	// 跨午夜：晚于 Start，或早于 End。
	return cur >= s || cur < e
}

// String 人类可读窗口描述（nil = 全时段）。
func (w *Window) String() string {
	if w == nil {
		return "all-hours"
	}
	days := ""
	if len(w.Days) > 0 {
		parts := make([]string, 0, len(w.Days))
		for _, d := range w.Days {
			parts = append(parts, strconv.Itoa(d))
		}
		days = "(" + strings.Join(parts, ",") + ")"
	}
	return w.Start + "-" + w.End + days
}

// parseHHMM 解析 "HH:MM"（或 "HH:MM:SS"）为当天秒数。
func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty time")
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	sec := 0
	if len(parts) == 3 {
		if sec, err = strconv.Atoi(strings.TrimSpace(parts[2])); err != nil || sec < 0 || sec > 59 {
			return 0, fmt.Errorf("bad second in %q", s)
		}
	}
	return h*3600 + m*60 + sec, nil
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ParseWindow 构造显式窗口（Start/End 为 "HH:MM"；days 为星期 0..6）。
func ParseWindow(start, end string, days []int) (*Window, error) {
	if _, err := parseHHMM(start); err != nil {
		return nil, fmt.Errorf("window start: %w", err)
	}
	if _, err := parseHHMM(end); err != nil {
		return nil, fmt.Errorf("window end: %w", err)
	}
	for _, d := range days {
		if d < 0 || d > 6 {
			return nil, fmt.Errorf("window day %d out of range [0,6]", d)
		}
	}
	w := &Window{Start: strings.TrimSpace(start), End: strings.TrimSpace(end)}
	if len(days) > 0 {
		w.Days = append([]int(nil), days...)
	}
	return w, nil
}

// ParseWindowSpec 解析 "HH:MM-HH:MM[,d1,d2...]" 形式的窗口串。
// 空串 → (nil, nil)（全时段）。非法串 → error（调用方回退全时段或报错）。
func ParseWindowSpec(spec string) (*Window, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	tp := strings.SplitN(strings.TrimSpace(parts[0]), "-", 2)
	if len(tp) != 2 {
		return nil, fmt.Errorf("window spec %q: want HH:MM-HH:MM[,d1,d2...]", spec)
	}
	var days []int
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := strconv.Atoi(p)
		if err != nil || d < 0 || d > 6 {
			return nil, fmt.Errorf("window spec %q: bad day %q", spec, p)
		}
		days = append(days, d)
	}
	return ParseWindow(strings.TrimSpace(tp[0]), strings.TrimSpace(tp[1]), days)
}

// ------------------------------------------------------------------
// Backoff（§10 冻结 + C 线生效）
// ------------------------------------------------------------------

// Backoff 退避策略（§10 冻结；零值 = 默认）。
//
// C 线默认值（§6.1.1）：Base=1m，Cap=30m，MaxFails=5。
type Backoff struct {
	Base     time.Duration
	Cap      time.Duration
	MaxFails int
}

func (b Backoff) withDefaults() Backoff {
	if b.Base <= 0 {
		b.Base = time.Minute
	}
	if b.Cap <= 0 {
		b.Cap = 30 * time.Minute
	}
	if b.MaxFails <= 0 {
		b.MaxFails = 5
	}
	if b.Cap < b.Base {
		b.Cap = b.Base
	}
	return b
}

// delayAfter 第 fails 次连续失败后的重试等待：Base·2^(fails-1)，封顶 Cap。
// fails<=0 返回 0（未失败，走正常 Interval）。
func (b Backoff) delayAfter(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	b = b.withDefaults()
	d := b.Base
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= b.Cap {
			d = b.Cap
			break
		}
	}
	if d > b.Cap {
		d = b.Cap
	}
	return d
}

// ------------------------------------------------------------------
// MaintenanceJob / MaintenanceConfig（§10 冻结 + C 线增补可选字段）
// ------------------------------------------------------------------

// MaintenanceJob 一个可调度维护作业（§10 冻结）。
type MaintenanceJob struct {
	Name     string
	Interval time.Duration
	Window   *Window
	Budget   Budget
	Backoff  Backoff
	Run      func(ctx context.Context) (Report, error)
}

// effectiveInterval 取实际间隔（零值 → 1h，避免忙转）。
func (j MaintenanceJob) effectiveInterval() time.Duration {
	if j.Interval <= 0 {
		return time.Hour
	}
	return j.Interval
}

// MaintenanceConfig 调度器配置（§10 冻结 + C 线增补可选依赖字段）。
type MaintenanceConfig struct {
	Consolidate       bool
	ConsolidateWindow *Window
	LifecycleInterval time.Duration
	LifecycleEngines  []string
	Logf              func(string, ...any)

	// R29-C 增补（**仅新增可选字段，不改变既有调用方的冻结签名用法**）：
	// 依赖注入 —— 生命周期引擎集与 consolidate 需要后端 / embedder / 整合器才能真跑。
	Backend          StorageBackend
	Embedder         func(string) ([]float32, error)
	Consolidator     *Consolidator
	Lifecycle        LifecycleConfig
	Tenant           string
	LifecycleWindow  *Window
	LifecycleBudget  Budget
	LifecycleBackoff Backoff
}

// defaultLifecycleEngines 默认引擎集（§6.1.2）。
func defaultLifecycleEngines() []string {
	return []string{EngineBudget, EngineForget, EngineCore, EngineArchive, EngineCompress}
}

// DefaultMaintenanceJobs 返回默认维护作业列表（§10 冻结签名）。
//
// C 线填实（§6.1）：LifecycleInterval>0 且有 Backend → 加生命周期引擎集作业
// （修 L-248，真起定时器）；Consolidate==true 且有 Consolidator → 加整合作业。
// 两者皆默认关 → 返回空列表（与 c203db8 逐位一致，consolidate 仍由 cons.Start 驱动）。
func DefaultMaintenanceJobs(cfg MaintenanceConfig) []MaintenanceJob {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	var jobs []MaintenanceJob
	if cfg.LifecycleInterval > 0 {
		if cfg.Backend == nil {
			cfg.Logf("[maint] lifecycle interval set but no backend wired; lifecycle job disabled")
		} else {
			jobs = append(jobs, lifecycleMaintenanceJob(cfg))
		}
	}
	if cfg.Consolidate && cfg.Consolidator != nil {
		jobs = append(jobs, cfg.Consolidator.MaintenanceJob(
			DefaultConsolidateInterval, cfg.ConsolidateWindow, Budget{}))
	}
	return jobs
}

// lifecycleMaintenanceJob 把生命周期引擎集包装成可调度作业（修 L-248）。
func lifecycleMaintenanceJob(cfg MaintenanceConfig) MaintenanceJob {
	engines := cfg.LifecycleEngines
	if len(engines) == 0 {
		engines = defaultLifecycleEngines()
	}
	lc := cfg.Lifecycle
	tenant := cfg.Tenant
	backend := cfg.Backend
	embedder := cfg.Embedder
	return MaintenanceJob{
		Name:     "lifecycle",
		Interval: cfg.LifecycleInterval,
		Window:   cfg.LifecycleWindow,
		Budget:   cfg.LifecycleBudget,
		Backoff:  cfg.LifecycleBackoff,
		Run: func(ctx context.Context) (Report, error) {
			start := time.Now()
			moved, err := RunLifecycleEngines(ctx, backend, embedder, tenant, engines, lc)
			markLifecycleRun(time.Now())
			if err != nil {
				return Report{Job: "lifecycle", DurationMS: time.Since(start).Milliseconds()}, err
			}
			total := 0
			names := make([]string, 0, len(engines))
			for _, e := range engines {
				n := moved[e]
				total += n
				names = append(names, fmt.Sprintf("%s=%d", e, n))
			}
			return Report{
				Job:        "lifecycle",
				Items:      total,
				Writes:     total,
				DurationMS: time.Since(start).Milliseconds(),
				Notes:      []string{"engines " + strings.Join(names, ",")},
			}, nil
		},
	}
}

// ------------------------------------------------------------------
// Maintenance（§10 冻结签名）
// ------------------------------------------------------------------

// Maintenance 调度器实例（§10 冻结签名；导出供 main.go 持有并调用 Stop/Wait）。
type Maintenance struct {
	jobs     []MaintenanceJob
	logf     func(string, ...any)
	cancel   context.CancelFunc
	stopped  chan struct{}
	stopOnce sync.Once

	// 单飞去重：job 名 → 是否在跑。
	mu      sync.Mutex
	running map[string]bool
}

// jobOutcome 一轮作业的结果分类（用于退避与单飞判定）。
type jobOutcome int

// jobOK 本轮成功；jobErr 本轮失败 → 进入退避；
// jobSkipped 单飞：上一轮未结束 → 按正常间隔再试。
const (
	jobOK jobOutcome = iota
	jobErr
	jobSkipped
)

// StartMaintenance 启动维护调度器（§10 冻结签名）。
// 调用方应在退出序列中调用 Stop()（幂等、不阻塞）。
func StartMaintenance(ctx context.Context, jobs []MaintenanceJob, logf func(string, ...any)) *Maintenance {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mctx, cancel := context.WithCancel(ctx)
	m := &Maintenance{
		jobs:    jobs,
		logf:    logf,
		cancel:  cancel,
		stopped: make(chan struct{}),
		running: make(map[string]bool),
	}
	go m.run(mctx)
	return m
}

// run 调度循环：每个 job 各起一个 goroutine，按自身 Interval/退避触发；
// ctx 取消时退出（stopped 关闭，供 Wait 感知）。
func (m *Maintenance) run(ctx context.Context) {
	defer close(m.stopped)
	if len(m.jobs) == 0 {
		m.logf("[maint] scheduler started jobs=0 (default-off, no job goroutines)")
		<-ctx.Done()
		return
	}
	m.logf("[maint] scheduler started jobs=%d", len(m.jobs))
	var wg sync.WaitGroup
	for _, j := range m.jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.runJobLoop(ctx, j, &wg)
		}()
	}
	<-ctx.Done()
	wg.Wait() // 等所有 job 循环收尾，stopped 才关闭（Wait 的语义）
}

// runJobLoop 单 job 循环：窗口准入 + 单飞去重 + 退避重试（§6.1.1 / §6.1.4）。
//
// 窗口语义（§6.1.4）：tick 到达时判定；不在窗口内 → 本轮不启动（**在跑的一轮会跑完**）；
// 窗口结束后不再启动新轮，直到再次落入窗口。
func (m *Maintenance) runJobLoop(ctx context.Context, j MaintenanceJob, wg *sync.WaitGroup) {
	iv := j.effectiveInterval()
	backoff := j.Backoff.withDefaults()
	fails := 0
	// 异步驱动：tick 到达即启动一轮（独立 goroutine），调度循环不被单轮阻塞。
	// 这正是单飞的成立前提——上一轮仍在跑时，新一轮由 runJob 的 acquire 判重
	// 直接跳过（jobSkipped），"在跑的一轮会跑完"（§6.1.3）。
	// 轮次结果经 lastOut 回传驱动退避；jobSkipped 不计失败。
	var lastMu sync.Mutex
	lastOut := jobOK
	t := time.NewTimer(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !j.Window.Allows(time.Now()) {
				m.logf("[maint] job %s outside window %s (skip)", j.Name, j.Window)
				t.Reset(iv)
				continue
			}
			lastMu.Lock()
			switch lastOut {
			case jobOK:
				fails = 0
			case jobErr:
				fails++
			}
			lastMu.Unlock()
			if fails > 0 {
				if fails >= backoff.MaxFails {
					m.logf("[maint] WARN job %s failing persistently (%d consecutive): keeping scheduler alive, retrying with backoff", j.Name, fails)
				}
				m.logf("[maint] job %s failed (%d consecutive), backing off %s", j.Name, fails, backoff.delayAfter(fails))
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				out := m.runJob(ctx, j)
				if out == jobSkipped {
					return // 单飞跳过不计失败
				}
				lastMu.Lock()
				lastOut = out
				lastMu.Unlock()
			}()
			delay := iv
			if fails > 0 {
				delay = backoff.delayAfter(fails)
			}
			t.Reset(delay)
		}
	}
}

// runJob 执行单个作业并吞掉错误（调度器不因此退出）。
func (m *Maintenance) runJob(ctx context.Context, j MaintenanceJob) jobOutcome {
	if j.Run == nil {
		return jobSkipped
	}
	if !m.acquire(j.Name) {
		incMaintenanceSkipped(j.Name, "single_flight")
		m.logf("[maint] job %s skipped: previous round still running (single_flight)", j.Name)
		return jobSkipped
	}
	defer m.release(j.Name)

	rctx := ctx
	var cancel context.CancelFunc
	if j.Budget.MaxDuration > 0 {
		rctx, cancel = context.WithTimeout(ctx, j.Budget.MaxDuration)
		defer cancel()
	}
	start := time.Now()
	rep, err := j.Run(rctx)
	if err != nil {
		m.logf("[maint] job %s error: %v", j.Name, err)
		incMaintenanceRun(j.Name, "error")
		return jobErr
	}
	if rep.Job == "" {
		rep.Job = j.Name
	}
	if rep.DurationMS <= 0 {
		rep.DurationMS = time.Since(start).Milliseconds()
	}
	if over := budgetExceeded(j.Budget, rep); over != "" {
		rep.Notes = append(rep.Notes, over)
		m.logf("[maint] WARN job %s budget exceeded: %s", j.Name, over)
	}
	m.logf("[maint] job %s done items=%d writes=%d dur=%dms notes=%v",
		rep.Job, rep.Items, rep.Writes, rep.DurationMS, rep.Notes)
	incMaintenanceRun(j.Name, "ok")
	return jobOK
}

// budgetExceeded 软核对 MaxItems/MaxWrites（超限返回描述串，未超返回 ""）。
func budgetExceeded(b Budget, rep Report) string {
	var over []string
	if b.MaxItems > 0 && rep.Items > b.MaxItems {
		over = append(over, fmt.Sprintf("items %d>%d", rep.Items, b.MaxItems))
	}
	if b.MaxWrites > 0 && rep.Writes > b.MaxWrites {
		over = append(over, fmt.Sprintf("writes %d>%d", rep.Writes, b.MaxWrites))
	}
	if len(over) == 0 {
		return ""
	}
	return strings.Join(over, "; ")
}

func (m *Maintenance) acquire(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[name] {
		return false
	}
	m.running[name] = true
	return true
}

func (m *Maintenance) release(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, name)
}

// Stop 幂等、不阻塞地停止调度器（§10 冻结）。
func (m *Maintenance) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.stopOnce.Do(m.cancel)
}

// Wait 等待当前轮次结束（ctx 超时则返回 ctx.Err）（§10 冻结）。
func (m *Maintenance) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LastLifecycleRun 返回最近一次生命周期引擎集运行时间（UTC）；从未运行返回零值。
// 供 B 线 /healthz.last_lifecycle_run 字段承接（§6.1.2）；C 只提供时间戳 getter。
func (m *Maintenance) LastLifecycleRun() time.Time {
	return LastLifecycleRun()
}

// ------------------------------------------------------------------
// 生命周期运行时间戳（包级单一真源，B 线 / B 线的 /healthz 均可读）
// ------------------------------------------------------------------

var lastLifecycleRunUnix atomic.Int64

func markLifecycleRun(t time.Time) {
	lastLifecycleRunUnix.Store(t.UTC().Unix())
}

// LastLifecycleRun 返回最近一次生命周期引擎集运行时间（UTC）；从未运行返回零值。
func LastLifecycleRun() time.Time {
	u := lastLifecycleRunUnix.Load()
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(u, 0).UTC()
}

// ------------------------------------------------------------------
// 指标 hook（方法名与 §10 冻结的 *ServerMetrics 一致；未接线 no-op）
// ------------------------------------------------------------------

// MaintenanceMetrics 调度器上报的指标接口。
// 方法名与任务书 §10 L464-465 冻结的 *ServerMetrics.IncMaintenanceRun /
// IncMaintenanceSkipped 完全一致；B 线在 server_metrics.go 落地后，用
// SetMaintenanceMetrics(metrics) 接线即可（无需改本文件）。未接线时 no-op。
type MaintenanceMetrics interface {
	IncMaintenanceRun(job, result string)
	IncMaintenanceSkipped(job, reason string)
}

var (
	maintenanceMetricsMu sync.RWMutex
	maintenanceMetrics   MaintenanceMetrics
)

// SetMaintenanceMetrics 注入调度器指标接收器（nil = no-op，默认）。
func SetMaintenanceMetrics(m MaintenanceMetrics) {
	maintenanceMetricsMu.Lock()
	maintenanceMetrics = m
	maintenanceMetricsMu.Unlock()
}

func maintenanceMetricsHook() MaintenanceMetrics {
	maintenanceMetricsMu.RLock()
	defer maintenanceMetricsMu.RUnlock()
	return maintenanceMetrics
}

func incMaintenanceRun(job, result string) {
	if h := maintenanceMetricsHook(); h != nil {
		h.IncMaintenanceRun(job, result)
	}
}

func incMaintenanceSkipped(job, reason string) {
	if h := maintenanceMetricsHook(); h != nil {
		h.IncMaintenanceSkipped(job, reason)
	}
}

// ------------------------------------------------------------------
// env 装配（C 线提供；main.go 一行调用）
// ------------------------------------------------------------------

// MaintenanceDeps 调度器所需的外部依赖（R29-C 新增，供 main.go 一行装配）。
type MaintenanceDeps struct {
	Backend      StorageBackend
	Embedder     func(string) ([]float32, error)
	Consolidator *Consolidator
	Lifecycle    LifecycleConfig
	Tenant       string
	Logf         func(string, ...any)
}

// MaintenanceConfigFromEnv 依 env 装配调度配置（**全部默认关/默认现状**）。
//
// 可读开关（未设 = 保持现状，不产生任何调度行为）：
//
//	LIMS_MAINTENANCE_INTERVAL    调度器总开关（时长，>0 启用）
//	LIMS_LIFECYCLE_INTERVAL      生命周期引擎集周期（时长，>0 启用；优先于总开关）
//	LIMS_LIFECYCLE_ENGINES       引擎集（csv，默认 budget,forget,core,archive,compress）
//	LIMS_LIFECYCLE_TENANT        租户作用域（空 = 引擎默认/不限）
//	LIMS_LIFECYCLE_WINDOW        引擎集运行窗（"HH:MM-HH:MM[,d1,d2...]"；空=全时段）
//	LIMS_LIFECYCLE_BUDGET_LIMITS 如 "L0=100,L1=500"（空=budget 引擎不跑）
//	LIMS_LIFECYCLE_FORGET_THRESHOLD / LIMS_LIFECYCLE_CORE_ACCESS_MIN /
//	LIMS_LIFECYCLE_ARCHIVE_IDLE / LIMS_LIFECYCLE_COMPRESS_SIM
//	LIMS_CONSOLIDATE_WINDOW      整合窗（由 main.go 传给 cons.SetWindow）
//	LIMS_MAINT_BUDGET_MAX_ITEMS / LIMS_MAINT_BUDGET_MAX_DURATION /
//	LIMS_MAINT_BUDGET_MAX_WRITES  单轮预算（空=不限）
//	LIMS_MAINT_BACKOFF_BASE / LIMS_MAINT_BACKOFF_CAP / LIMS_MAINT_BACKOFF_MAX_FAILS
func MaintenanceConfigFromEnv(getenv func(string) string, deps MaintenanceDeps) MaintenanceConfig {
	cfg := MaintenanceConfig{
		Logf:         deps.Logf,
		Backend:      deps.Backend,
		Embedder:     deps.Embedder,
		Consolidator: deps.Consolidator,
		Lifecycle:    deps.Lifecycle,
		Tenant:       deps.Tenant,
	}
	iv := durEnv(getenv, "LIMS_LIFECYCLE_INTERVAL", 0)
	if iv <= 0 {
		iv = durEnv(getenv, "LIMS_MAINTENANCE_INTERVAL", 0)
	}
	cfg.LifecycleInterval = iv
	cfg.LifecycleEngines = enginesFromEnv(getenv)
	// 租户作用域：env 优先，未设则沿用 deps.Tenant（保持 main.go 显式传参语义）。
	if tn := getenv("LIMS_LIFECYCLE_TENANT"); tn != "" {
		cfg.Tenant = tn
	}
	// 窗口：解析失败回退全时段（fail-open，与 Allows 口径一致）。
	if w, err := ParseWindowSpec(getenv("LIMS_LIFECYCLE_WINDOW")); err == nil {
		cfg.LifecycleWindow = w
	}
	if w, err := ParseWindowSpec(getenv("LIMS_CONSOLIDATE_WINDOW")); err == nil {
		cfg.ConsolidateWindow = w
	}
	cfg.Lifecycle.BudgetLimits = parseBudgetLimits(getenv("LIMS_LIFECYCLE_BUDGET_LIMITS"))
	cfg.Lifecycle.ForgetThreshold = fenv(getenv, "LIMS_LIFECYCLE_FORGET_THRESHOLD", cfg.Lifecycle.ForgetThreshold)
	cfg.Lifecycle.CoreAccessMin = int64(ienv(getenv, "LIMS_LIFECYCLE_CORE_ACCESS_MIN", int(cfg.Lifecycle.CoreAccessMin)))
	cfg.Lifecycle.ArchiveIdle = durEnv(getenv, "LIMS_LIFECYCLE_ARCHIVE_IDLE", cfg.Lifecycle.ArchiveIdle)
	cfg.Lifecycle.CompressSim = float32(fenv(getenv, "LIMS_LIFECYCLE_COMPRESS_SIM", float64(cfg.Lifecycle.CompressSim)))
	cfg.LifecycleBudget = Budget{
		MaxItems:    ienv(getenv, "LIMS_MAINT_BUDGET_MAX_ITEMS", 0),
		MaxDuration: durEnv(getenv, "LIMS_MAINT_BUDGET_MAX_DURATION", 0),
		MaxWrites:   ienv(getenv, "LIMS_MAINT_BUDGET_MAX_WRITES", 0),
	}
	cfg.LifecycleBackoff = Backoff{
		Base:     durEnv(getenv, "LIMS_MAINT_BACKOFF_BASE", 0),
		Cap:      durEnv(getenv, "LIMS_MAINT_BACKOFF_CAP", 0),
		MaxFails: ienv(getenv, "LIMS_MAINT_BACKOFF_MAX_FAILS", 0),
	}
	// consolidate 默认仍由 main.go 的 cons.Start 独立驱动（零回归）；
	// 需要纳入调度器时由调用方显式置 Consolidate=true。
	cfg.Consolidate = false
	return cfg
}

// StartMaintenanceAuto 由 main.go 一行调用：依进程 env 装配并启动调度器。
// 未设任何开关（或配置不足）→ 返回 nil，**不创建任何维护 goroutine**（默认关）。
func StartMaintenanceAuto(ctx context.Context, deps MaintenanceDeps) *Maintenance {
	return StartMaintenanceAutoEnv(ctx, deps, os.Getenv)
}

// StartMaintenanceAutoEnv 与 StartMaintenanceAuto 等价，但允许注入 getenv（便于单测）。
func StartMaintenanceAutoEnv(ctx context.Context, deps MaintenanceDeps, getenv func(string) string) *Maintenance {
	cfg := MaintenanceConfigFromEnv(getenv, deps)
	if cfg.Logf == nil {
		cfg.Logf = deps.Logf
	}
	jobs := DefaultMaintenanceJobs(cfg)
	if len(jobs) == 0 {
		if cfg.Logf != nil {
			cfg.Logf("[maint] maintenance scheduler not enabled (no switch set)")
		}
		return nil
	}
	m := StartMaintenance(ctx, jobs, cfg.Logf)
	if cfg.LifecycleInterval > 0 {
		cfg.Logf("[boot] maintenance scheduler started: lifecycle interval=%s engines=%v window=%s",
			cfg.LifecycleInterval, cfg.LifecycleEngines, cfg.LifecycleWindow)
	}
	return m
}

// ------------------------------------------------------------------
// env 解析小工具
// ------------------------------------------------------------------

func durEnv(getenv func(string) string, key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

func fenv(getenv func(string) string, key string, def float64) float64 {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func ienv(getenv func(string) string, key string, def int) int {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func enginesFromEnv(getenv func(string) string) []string {
	raw := strings.TrimSpace(getenv("LIMS_LIFECYCLE_ENGINES"))
	if raw == "" {
		return defaultLifecycleEngines()
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultLifecycleEngines()
	}
	return out
}
