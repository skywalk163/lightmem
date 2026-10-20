// 维护调度器（R29-T0 S0 接缝，最小可用实现）。
//
// 职责边界（任务书 §2.2 / §10）：
//   - T0 落最小 ticker 框架 + 冻结签名（MaintenanceJob / MaintenanceConfig /
//     StartMaintenance / Stop / Wait / DefaultMaintenanceJobs）。
//   - C 线硬化：窗口（Window）/ 预算（Budget）/ 退避（Backoff）的实际生效逻辑。
//
// 默认关（红线④）：本文件不自动启动任何 goroutine；main.go 仅在显式开关
// （LIMS_MAINTENANCE_INTERVAL）下调用 StartMaintenance。即便启动，
// DefaultMaintenanceJobs 默认返回空 job 列表，不重复运行既有 consolidate
// （consolidate 由 main.go 的 cons.Start 单独管理）。
//
// 红线③：只用 stdlib，不引入第三方依赖。
package kernel

import (
	"context"
	"fmt"
	"time"
)

// Report 一轮维护作业的结果（§10 冻结）。
type Report struct {
	Job        string
	Items      int
	Writes     int
	DurationMS int64
	Notes      []string
}

// Budget 单轮资源预算（§10 冻结；S0 仅持有，C 线生效）。
type Budget struct {
	MaxItems    int
	MaxDuration time.Duration
	MaxWrites   int
}

// Window 允许运行的时间窗（nil = 全时段）（§10 冻结；S0 仅持有）。
type Window struct {
	Start string
	End   string
	Days  []int
}

// Backoff 退避策略（§10 冻结；S0 仅持有）。
type Backoff struct {
	Base     time.Duration
	Cap      time.Duration
	MaxFails int
}

// MaintenanceJob 一个可调度维护作业（§10 冻结）。
type MaintenanceJob struct {
	Name     string
	Interval time.Duration
	Window   *Window
	Budget   Budget
	Backoff  Backoff
	Run      func(ctx context.Context) (Report, error)
}

// MaintenanceConfig 调度器配置（§10 冻结）。
type MaintenanceConfig struct {
	Consolidate       bool
	ConsolidateWindow *Window
	LifecycleInterval time.Duration
	LifecycleEngines  []string
	Logf              func(string, ...any)
}

// Maintenance 调度器实例（§10 冻结签名；导出供 main.go 持有 *kernel.Maintenance 并调用 Stop/Wait）。
type Maintenance struct {
	jobs     []MaintenanceJob
	logf     func(string, ...any)
	cancel   context.CancelFunc
	stopped  chan struct{}
	stopOnce chan struct{}
}

// DefaultMaintenanceJobs 返回默认维护作业列表（§10 冻结签名）。
//
// S0 默认返回空列表（最小可用接缝）：C 线在此填充 consolidate / lifecycle /
// forget / archive 等具体作业，并按 MaintenanceConfig 落实窗口/预算/退避。
func DefaultMaintenanceJobs(cfg MaintenanceConfig) []MaintenanceJob {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	// S0 占位：返回空 job 列表，不重复运行既有 consolidate。
	return nil
}

// StartMaintenance 启动维护调度器（§10 冻结签名）。
// 返回 *Maintenance；调用方应在退出序列中调用 Stop()（幂等、不阻塞）。
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
	}
	go m.run(mctx)
	return m
}

// run 调度循环：每个 job 各起一个 goroutine，按自身 Interval ticker 触发；
// ctx 取消时统一退出（stopped 关闭，供 Wait 感知）。
func (m *Maintenance) run(ctx context.Context) {
	defer close(m.stopped)
	if len(m.jobs) == 0 {
		m.logf("[maint] scheduler started with 0 jobs (no-op until C line populates)")
		<-ctx.Done()
		return
	}
	m.logf("[maint] scheduler started jobs=%d", len(m.jobs))
	for _, j := range m.jobs {
		go m.runJobLoop(ctx, j)
	}
	<-ctx.Done()
}

// runJobLoop 单 job 的 ticker 循环（独立 goroutine）。
func (m *Maintenance) runJobLoop(ctx context.Context, j MaintenanceJob) {
	iv := j.Interval
	if iv <= 0 {
		iv = time.Hour
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.runJob(ctx, j)
		}
	}
}

// runJob 执行单个作业并吞掉错误（调度器不因此退出）。
func (m *Maintenance) runJob(ctx context.Context, j MaintenanceJob) {
	if j.Run == nil {
		return
	}
	rep, err := j.Run(ctx)
	if err != nil {
		m.logf("[maint] job %s error: %v", j.Name, err)
		return
	}
	m.logf("[maint] job %s done items=%d writes=%d dur=%dms", j.Name, rep.Items, rep.Writes, rep.DurationMS)
}

// Stop 幂等、不阻塞地停止调度器。
func (m *Maintenance) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
}

// Wait 等待当前轮次结束（ctx 超时则返回 ctx.Err）。
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

// String 便于日志/调试。
func (r Report) String() string {
	return fmt.Sprintf("job=%s items=%d writes=%d dur=%dms notes=%v",
		r.Job, r.Items, r.Writes, r.DurationMS, r.Notes)
}
