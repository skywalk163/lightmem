package kernel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// 测试小工具
// ------------------------------------------------------------------

// metricsRecorder 记录调度器上报的指标（替代尚未落地的 *ServerMetrics）。
// runs 键为 "job|result"，skips 键为 "job|reason"。
type metricsRecorder struct {
	mu    sync.Mutex
	runs  map[string]int
	skips map[string]int
}

func newMetricsRecorder() *metricsRecorder {
	return &metricsRecorder{runs: map[string]int{}, skips: map[string]int{}}
}

func (r *metricsRecorder) IncMaintenanceRun(job, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[job+"|"+result]++
}

func (r *metricsRecorder) IncMaintenanceSkipped(job, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skips[job+"|"+reason]++
}

func (r *metricsRecorder) runCount(job, result string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[job+"|"+result]
}

func (r *metricsRecorder) skipCount(job, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skips[job+"|"+reason]
}

// logBuf 线程安全日志缓冲（用于断言日志内容）。
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format, a...)
	l.b.WriteByte('\n')
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hm 取 HH:MM（窗口串用）。
func hm(t time.Time) string {
	return fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute())
}

// nowPlusWindow 生成一个**必然包含 now**的窗口（±1h，容跨午夜）。
func nowPlusWindow(t time.Time) *Window {
	w, err := ParseWindow(hm(t.Add(-1*time.Hour)), hm(t.Add(1*time.Hour)), nil)
	if err != nil {
		panic(err)
	}
	return w
}

// futureWindow 生成一个**必然排除 now**的窗口（now+2h 到 now+3h）。
func futureWindow(t time.Time) *Window {
	w, err := ParseWindow(hm(t.Add(2*time.Hour)), hm(t.Add(3*time.Hour)), nil)
	if err != nil {
		panic(err)
	}
	return w
}

// ------------------------------------------------------------------
// 默认关 / 装配
// ------------------------------------------------------------------

func TestDefaultMaintenanceJobs_ReturnsEmptyByDefault(t *testing.T) {
	jobs := DefaultMaintenanceJobs(MaintenanceConfig{})
	if len(jobs) != 0 {
		t.Fatalf("default jobs = %d, want 0 (默认关：不设开关 = 无 job goroutine)", len(jobs))
	}
}

// R12（§6.2）：不设任何新开关 → 无 job、无 goroutine、StartMaintenanceAuto 返回 nil。
func TestLifecycleTimer_DefaultOff(t *testing.T) {
	cfg := MaintenanceConfigFromEnv(func(string) string { return "" }, MaintenanceDeps{})
	if cfg.LifecycleInterval != 0 {
		t.Fatalf("default lifecycle interval = %s, want 0", cfg.LifecycleInterval)
	}
	if len(DefaultMaintenanceJobs(cfg)) != 0 {
		t.Fatalf("default jobs should be empty")
	}
	m := StartMaintenanceAutoEnv(context.Background(), MaintenanceDeps{Logf: func(string, ...any) {}},
		func(string) string { return "" })
	if m != nil {
		t.Fatalf("StartMaintenanceAutoEnv with no env should return nil, got %v", m)
	}
}

func TestMaintenanceConfigFromEnv_ParsesSwitches(t *testing.T) {
	env := map[string]string{
		"LIMS_LIFECYCLE_INTERVAL":        "90s",
		"LIMS_LIFECYCLE_ENGINES":         "budget,archive",
		"LIMS_LIFECYCLE_TENANT":          "t1",
		"LIMS_LIFECYCLE_BUDGET_LIMITS":   "L0=100,L1=500",
		"LIMS_LIFECYCLE_WINDOW":          "02:00-04:00,0,6",
		"LIMS_CONSOLIDATE_WINDOW":        "03:00-05:00",
		"LIMS_MAINT_BUDGET_MAX_ITEMS":    "50",
		"LIMS_MAINT_BUDGET_MAX_DURATION": "10s",
		"LIMS_MAINT_BUDGET_MAX_WRITES":   "20",
		"LIMS_MAINT_BACKOFF_BASE":        "5s",
		"LIMS_MAINT_BACKOFF_CAP":         "1m",
		"LIMS_MAINT_BACKOFF_MAX_FAILS":   "3",
	}
	getenv := func(k string) string { return env[k] }
	cfg := MaintenanceConfigFromEnv(getenv, MaintenanceDeps{})
	if cfg.LifecycleInterval != 90*time.Second {
		t.Fatalf("interval = %s", cfg.LifecycleInterval)
	}
	if cfg.Tenant != "t1" {
		t.Fatalf("tenant = %q", cfg.Tenant)
	}
	if len(cfg.LifecycleEngines) != 2 || cfg.LifecycleEngines[0] != EngineBudget {
		t.Fatalf("engines = %v", cfg.LifecycleEngines)
	}
	if cfg.Lifecycle.BudgetLimits[MemoryLayer("L1")] != 500 {
		t.Fatalf("budget limits = %v", cfg.Lifecycle.BudgetLimits)
	}
	if cfg.LifecycleWindow == nil || cfg.LifecycleWindow.Start != "02:00" || len(cfg.LifecycleWindow.Days) != 2 {
		t.Fatalf("lifecycle window = %v", cfg.LifecycleWindow)
	}
	if cfg.ConsolidateWindow == nil || cfg.ConsolidateWindow.Start != "03:00" {
		t.Fatalf("consolidate window = %v", cfg.ConsolidateWindow)
	}
	if cfg.LifecycleBudget.MaxItems != 50 || cfg.LifecycleBudget.MaxDuration != 10*time.Second || cfg.LifecycleBudget.MaxWrites != 20 {
		t.Fatalf("budget = %+v", cfg.LifecycleBudget)
	}
	if cfg.LifecycleBackoff.Base != 5*time.Second || cfg.LifecycleBackoff.Cap != time.Minute || cfg.LifecycleBackoff.MaxFails != 3 {
		t.Fatalf("backoff = %+v", cfg.LifecycleBackoff)
	}
	if cfg.Consolidate {
		t.Fatalf("Consolidate should default false (cons.Start 独立驱动)")
	}
}

func TestMaintenanceConfigFromEnv_DefaultsEngines(t *testing.T) {
	cfg := MaintenanceConfigFromEnv(func(string) string { return "" }, MaintenanceDeps{})
	want := []string{EngineBudget, EngineForget, EngineCore, EngineArchive, EngineCompress}
	if strings.Join(cfg.LifecycleEngines, ",") != strings.Join(want, ",") {
		t.Fatalf("default engines = %v, want %v", cfg.LifecycleEngines, want)
	}
}

// R11（§6.2）：LIMS_LIFECYCLE_INTERVAL 真起定时器，≥2 次运行 + last_lifecycle_run 更新。
func TestLifecycleTimer_FiresAndUpdatesTimestamp(t *testing.T) {
	lastLifecycleRunUnix.Store(0)
	t.Cleanup(func() { lastLifecycleRunUnix.Store(0) })

	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	rec := newMetricsRecorder()
	SetMaintenanceMetrics(rec)
	t.Cleanup(func() { SetMaintenanceMetrics(nil) })

	cfg := MaintenanceConfig{
		Logf:              func(string, ...any) {},
		LifecycleInterval: 30 * time.Millisecond,
		Backend:           b,
	}
	jobs := DefaultMaintenanceJobs(cfg)
	if len(jobs) != 1 || jobs[0].Name != "lifecycle" {
		t.Fatalf("jobs = %v, want 1 lifecycle job", len(jobs))
	}
	m := StartMaintenance(context.Background(), jobs, func(string, ...any) {})
	defer m.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// 两个条件都满足才停：只在 LastLifecycleRun 已更新但还不到 2 轮时提前 break
		// 会让断言 runCount>=2 必然失败。
		if !LastLifecycleRun().IsZero() && rec.runCount("lifecycle", "ok") >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if LastLifecycleRun().IsZero() {
		t.Fatalf("last lifecycle run not updated (L-248 未关闭)")
	}
	if n := rec.runCount("lifecycle", "ok"); n < 2 {
		t.Fatalf("lifecycle runs = %d, want >=2 (L-248 未关闭)", n)
	}
	if got := m.LastLifecycleRun(); got.IsZero() {
		t.Fatalf("Maintenance.LastLifecycleRun() should be non-zero")
	}
}

func TestDefaultMaintenanceJobs_LifecycleNoBackend(t *testing.T) {
	// LIMS_LIFECYCLE_INTERVAL 设了但没接线后端：不得创建空转 job。
	jobs := DefaultMaintenanceJobs(MaintenanceConfig{LifecycleInterval: time.Minute})
	if len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0 (no backend → disabled)", len(jobs))
	}
}

// ------------------------------------------------------------------
// 窗口
// ------------------------------------------------------------------

func TestWindow_NilAllowsAll(t *testing.T) {
	var w *Window
	if !w.Allows(time.Now()) {
		t.Fatalf("nil window must allow all hours")
	}
	if w.String() != "all-hours" {
		t.Fatalf("nil window string = %q", w.String())
	}
}

func TestWindow_AllowsInRange(t *testing.T) {
	w := nowPlusWindow(time.Now())
	if !w.Allows(time.Now()) {
		t.Fatalf("window %s should allow now", w)
	}
}

func TestWindow_AllowsOutOfRange(t *testing.T) {
	w := futureWindow(time.Now())
	if w.Allows(time.Now()) {
		t.Fatalf("window %s should NOT allow now", w)
	}
}

func TestWindow_AllowsExactRange(t *testing.T) {
	w, err := ParseWindow("10:00", "12:00", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 窗口语义：[Start, End)，Start 含、End 不含，跨天回绕。
	cases := []struct {
		t    time.Time
		want bool
	}{
		{time.Date(2026, 1, 5, 9, 59, 59, 0, time.UTC), false},
		{time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 1, 5, 11, 30, 0, 0, time.UTC), true},
		{time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := w.Allows(c.t); got != c.want {
			t.Errorf("Allows(%s) = %v, want %v", c.t.Format(time.RFC3339), got, c.want)
		}
	}
}

func TestWindow_AllowsWrapMidnight(t *testing.T) {
	w, err := ParseWindow("22:00", "02:00", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.Allows(time.Date(2026, 1, 5, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("23:00 should be inside 22:00-02:00")
	}
	if !w.Allows(time.Date(2026, 1, 5, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("01:00 should be inside 22:00-02:00 (跨午夜)")
	}
	if w.Allows(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("12:00 should be outside 22:00-02:00")
	}
}

func TestWindow_AllowsDayFilter(t *testing.T) {
	w, err := ParseWindow("00:00", "23:59", []int{0, 6}) // 仅周末
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2026-01-04 是周日（0）；2026-01-05 是周一（1）。
	if !w.Allows(time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Sunday should be allowed")
	}
	if w.Allows(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Monday should be excluded")
	}
}

func TestParseWindowSpec_Valid(t *testing.T) {
	w, err := ParseWindowSpec("02:00-04:00,0,6")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if w.Start != "02:00" || w.End != "04:00" || len(w.Days) != 2 {
		t.Fatalf("window = %+v", w)
	}
	if w.String() != "02:00-04:00(0,6)" {
		t.Fatalf("string = %q", w.String())
	}
	empty, err := ParseWindowSpec("")
	if err != nil || empty != nil {
		t.Fatalf("empty spec should be (nil,nil), got %v,%v", empty, err)
	}
}

func TestParseWindowSpec_Invalid(t *testing.T) {
	if _, err := ParseWindowSpec("10:00"); err == nil {
		t.Fatalf("want error for missing '-'")
	}
	if _, err := ParseWindowSpec("25:00-26:00"); err == nil {
		t.Fatalf("want error for bad hour")
	}
	if _, err := ParseWindowSpec("10:00-12:00,7"); err == nil {
		t.Fatalf("want error for day out of range")
	}
}

// ------------------------------------------------------------------
// 退避 / 预算 / 单飞
// ------------------------------------------------------------------

func TestBackoff_DoublesAndCaps(t *testing.T) {
	b := Backoff{Base: time.Minute, Cap: 30 * time.Minute, MaxFails: 5}
	cases := []struct {
		fails int
		want  time.Duration
	}{
		{0, 0},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 30 * time.Minute}, // 封顶
		{20, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := b.delayAfter(c.fails); got != c.want {
			t.Errorf("delayAfter(%d) = %s, want %s", c.fails, got, c.want)
		}
	}
}

func TestBackoff_ZeroDefaults(t *testing.T) {
	b := Backoff{}
	if got := b.delayAfter(3); got != 4*time.Minute {
		t.Fatalf("default delayAfter(3) = %s, want 4m (Base=1m)", got)
	}
	if got := b.delayAfter(10); got != 30*time.Minute {
		t.Fatalf("default delayAfter(10) = %s, want 30m (Cap=30m)", got)
	}
}

func TestMaintenance_SingleFlightSkip(t *testing.T) {
	rec := newMetricsRecorder()
	SetMaintenanceMetrics(rec)
	t.Cleanup(func() { SetMaintenanceMetrics(nil) })

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	jobs := []MaintenanceJob{{
		Name:     "slow",
		Interval: 15 * time.Millisecond,
		Run: func(ctx context.Context) (Report, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
				return Report{Job: "slow"}, ctx.Err()
			}
			return Report{Job: "slow", Items: 1}, nil
		},
	}}
	m := StartMaintenance(context.Background(), jobs, func(string, ...any) {})
	defer m.Stop()

	<-started
	time.Sleep(200 * time.Millisecond) // 期间多个 tick 应因单飞被跳过
	close(release)

	if n := rec.skipCount("slow", "single_flight"); n < 2 {
		t.Fatalf("single_flight skips = %d, want >=2", n)
	}
}

func TestMaintenance_BudgetMaxDurationEnforced(t *testing.T) {
	rec := newMetricsRecorder()
	SetMaintenanceMetrics(rec)
	t.Cleanup(func() { SetMaintenanceMetrics(nil) })

	jobs := []MaintenanceJob{{
		Name:     "blocker",
		Interval: 20 * time.Millisecond,
		Budget:   Budget{MaxDuration: 40 * time.Millisecond},
		Run: func(ctx context.Context) (Report, error) {
			<-ctx.Done() // 尊重 ctx：硬截止到达即让位
			return Report{Job: "blocker"}, ctx.Err()
		},
	}}
	m := StartMaintenance(context.Background(), jobs, func(string, ...any) {})
	defer m.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.runCount("blocker", "error") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.runCount("blocker", "error") < 1 {
		t.Fatalf("budget MaxDuration not enforced: no ctx-deadline errors")
	}
}

func TestMaintenance_SoftBudgetExceededNoted(t *testing.T) {
	logs := &logBuf{}
	jobs := []MaintenanceJob{{
		Name:     "over",
		Interval: 10 * time.Millisecond,
		Budget:   Budget{MaxItems: 1, MaxWrites: 1},
		Run: func(ctx context.Context) (Report, error) {
			return Report{Job: "over", Items: 5, Writes: 3}, nil
		},
	}}
	m := StartMaintenance(context.Background(), jobs, logs.logf)
	// 不能在首个 tick 之前就 Stop（否则本轮根本没跑）；先等软预算告警出现。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "budget exceeded") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = m.Wait(ctx)
	if !strings.Contains(logs.String(), "budget exceeded") {
		t.Fatalf("expected soft-budget warning, got logs:\n%s", logs.String())
	}
}

func TestMaintenance_BackoffSlowsAndAlerts(t *testing.T) {
	logs := &logBuf{}
	var attempts int
	var mu sync.Mutex
	jobs := []MaintenanceJob{{
		Name:     "flaky",
		Interval: 5 * time.Millisecond,
		Backoff:  Backoff{Base: 10 * time.Millisecond, Cap: 40 * time.Millisecond, MaxFails: 2},
		Run: func(ctx context.Context) (Report, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return Report{Job: "flaky"}, fmt.Errorf("boom")
		},
	}}
	m := StartMaintenance(context.Background(), jobs, logs.logf)
	time.Sleep(200 * time.Millisecond)
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = m.Wait(ctx)

	mu.Lock()
	n := attempts
	mu.Unlock()
	// 5ms 间隔无退避应 ~40 次；退避（10/20/40ms 封顶）应显著更少。
	if n > 15 {
		t.Fatalf("backoff not effective: %d attempts in 200ms", n)
	}
	if !strings.Contains(logs.String(), "failing persistently") {
		t.Fatalf("expected persistent-failure WARN, got:\n%s", logs.String())
	}
}

func TestMaintenance_WindowGatesRuns(t *testing.T) {
	rec := newMetricsRecorder()
	SetMaintenanceMetrics(rec)
	t.Cleanup(func() { SetMaintenanceMetrics(nil) })

	jobs := []MaintenanceJob{{
		Name:     "gated",
		Interval: 10 * time.Millisecond,
		Window:   futureWindow(time.Now()),
		Run: func(ctx context.Context) (Report, error) {
			return Report{Job: "gated", Items: 1}, nil
		},
	}}
	m := StartMaintenance(context.Background(), jobs, func(string, ...any) {})
	defer m.Stop()
	time.Sleep(120 * time.Millisecond)
	if rec.runCount("gated", "ok") != 0 {
		t.Fatalf("job ran outside window: %d ok runs", rec.runCount("gated", "ok"))
	}
}

func TestMaintenance_WindowAllowsRuns(t *testing.T) {
	rec := newMetricsRecorder()
	SetMaintenanceMetrics(rec)
	t.Cleanup(func() { SetMaintenanceMetrics(nil) })

	jobs := []MaintenanceJob{{
		Name:     "gated",
		Interval: 15 * time.Millisecond,
		Window:   nowPlusWindow(time.Now()),
		Run: func(ctx context.Context) (Report, error) {
			return Report{Job: "gated", Items: 1}, nil
		},
	}}
	m := StartMaintenance(context.Background(), jobs, func(string, ...any) {})
	defer m.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.runCount("gated", "ok") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.runCount("gated", "ok") < 2 {
		t.Fatalf("job should run inside window: %d ok runs", rec.runCount("gated", "ok"))
	}
}

// ------------------------------------------------------------------
// Stop / Wait（保持 S0 语义）
// ------------------------------------------------------------------

func TestStartMaintenance_StopNoBlock(t *testing.T) {
	m := StartMaintenance(context.Background(), nil, nil)
	if m == nil {
		t.Fatalf("StartMaintenance returned nil")
	}
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop() blocked")
	}
}

func TestMaintenance_StopNilSafe(t *testing.T) {
	var m *Maintenance
	m.Stop() // 不应 panic
}

func TestMaintenance_StopTwiceSafe(t *testing.T) {
	m := StartMaintenance(context.Background(), nil, nil)
	m.Stop()
	m.Stop()
}

func TestMaintenance_WaitReturnsOnStop(t *testing.T) {
	m := StartMaintenance(context.Background(), nil, nil)
	go func() { time.Sleep(10 * time.Millisecond); m.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
}

func TestMaintenance_StartWithJobRuns(t *testing.T) {
	ran := make(chan string, 1)
	jobs := []MaintenanceJob{{
		Name:     "test-job",
		Interval: 10 * time.Millisecond,
		Run: func(ctx context.Context) (Report, error) {
			ran <- "ok"
			return Report{Job: "test-job", Items: 1}, nil
		},
	}}
	m := StartMaintenance(context.Background(), jobs, nil)
	defer m.Stop()
	select {
	case v := <-ran:
		if v != "ok" {
			t.Fatalf("job result = %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("job did not run within timeout")
	}
}

func TestMaintenanceJobStructFields(t *testing.T) {
	j := MaintenanceJob{
		Name:     "x",
		Interval: time.Second,
		Budget:   Budget{MaxItems: 10, MaxWrites: 5, MaxDuration: time.Minute},
		Window:   &Window{Start: "02:00", End: "04:00", Days: []int{0, 6}},
		Backoff:  Backoff{Base: time.Second, Cap: time.Minute, MaxFails: 3},
	}
	if j.Budget.MaxItems != 10 || j.Backoff.MaxFails != 3 || j.Window == nil {
		t.Fatalf("MaintenanceJob struct fields not carried")
	}
	if j.effectiveInterval() != time.Second {
		t.Fatalf("effectiveInterval = %s", j.effectiveInterval())
	}
	if (MaintenanceJob{}).effectiveInterval() != time.Hour {
		t.Fatalf("zero interval should default to 1h")
	}
}

func TestMaintenanceConfigDefaults(t *testing.T) {
	cfg := MaintenanceConfig{}
	if cfg.Consolidate || cfg.LifecycleInterval != 0 {
		t.Fatalf("MaintenanceConfig zero value should be default-off")
	}
}

func TestReport_String(t *testing.T) {
	rep := Report{Job: "x", Items: 1, Writes: 2, DurationMS: 3}
	s := rep.String()
	if !strings.Contains(s, "job=x") || !strings.Contains(s, "items=1") {
		t.Fatalf("Report.String = %q", s)
	}
}
