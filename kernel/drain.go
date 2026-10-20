// 优雅排水器（R29-T0 S0 接缝落骨架；**R29-D 硬化**，任务书 §7.1）。
//
// 职责边界（任务书 §2.2 / §10）：
//   - T0 落 Drainer 结构与 Run/Add 最小实现（main.go 只调这两个）。
//   - D 线硬化：waiting 结构化日志（R14 证据串）、超时计数器注入（B 线
//     drain_timeouts_total，§10「C/D 只调用，不得自行加注册表」——经
//     DrainTimeoutCounter 接口注入，仅被 *ServerMetrics 满足）、
//     MarkUnfinished 落盘前 fsync、ClearUnfinished 收紧到 .drain.unfinished 后缀。
//
// 两阶段排水协议（设计稿 §3.3.4；main.go 编排，D 只提供机制）：
//
//	① server.Shutdown          停收新请求（main.go 既有）
//	② maint.Stop()             不再启新轮（C 线调度器；由 main.go 在 Run 前调用）
//	③ cancel(bgCtx) + Run(soft) 等整合/生命周期/落库 goroutine 返回
//	   ├─ 正常 → 清理未完成标记 → checkpoint（若启用）→ backend.Close()
//	   └─ 超时 → 保留标记 + 本器计时 + 返回 false → main.go os.Exit(1)（B4）
//	**禁止**：超时后先 Close() 仍让 goroutine 跑（use-after-close/panic）。
//
// 默认关（红线④）：main.go 默认不向 Drainer 注册任何任务，Run 立即返回 true，
// 不会触发 os.Exit(1)。B4 行为变更（超时 os.Exit(1)）只在 D 线注册了会超时的
// 排水任务且真的超时才生效；超时计数器同理，未注入 = 不计数。
//
// 红线③：只用 stdlib。
package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Drainer 优雅排水器（§10 冻结）。
type Drainer struct {
	timeout time.Duration
	logf    func(string, ...any)
	mu      sync.Mutex
	tasks   []drainTask

	// timeoutCounter 可选：超时发生时调用一次（对接 B 线
	// server_metrics.go 的 IncDrainTimeout / drain_timeouts_total）。
	// nil = 不计数（默认关）；注入见 SetTimeoutCounter。
	timeoutCounter DrainTimeoutCounter
}

// DrainTimeoutCounter 排水超时计数接口（§10：B 线提供 IncDrainTimeout，
// C/D 只调用、不得自行加注册表）。*ServerMetrics 满足之；测试可造假实现。
type DrainTimeoutCounter interface {
	IncDrainTimeout()
}

type drainTask struct {
	name string
	wait func(ctx context.Context) error
}

// NewDrainer 构造排水器（§10 冻结签名）。timeout 为整体排水时限。
func NewDrainer(timeout time.Duration, logf func(string, ...any)) *Drainer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Drainer{timeout: timeout, logf: logf}
}

// SetTimeoutCounter 注入超时计数器（默认关：不注入 = 不计数）。
// main.go 接线示例（T0 预埋请求）：drainer.SetTimeoutCounter(metrics)。
func (d *Drainer) SetTimeoutCounter(c DrainTimeoutCounter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.timeoutCounter = c
}

// Add 注册一个排水阶段（§10 冻结签名）。默认关：main.go 不调用即无任务。
// wait 为 nil 视为装配错误：记录并不再执行该阶段（不 panic，保持关闭路径健壮）。
func (d *Drainer) Add(name string, wait func(ctx context.Context) error) {
	if wait == nil {
		d.logf("[drain] Add(%q) ignored: nil wait func", name)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, drainTask{name: name, wait: wait})
}

// Run 执行所有已注册排水阶段（§10 冻结签名）。
//   - 无任务 → 立即返回 true（默认关，不触发 os.Exit）。
//   - 全部在 timeout 内完成 → 返回 true。
//   - 超时 → 返回 false（main.go 据此 os.Exit(1)，即 B4 行为变更）；
//     同时（若已注入）计一次 drain_timeouts_total，并明确日志
//     「未完成标记保留」——标记由 MarkUnfinished 在关闭开始前落盘，
//     超时路径**不得**清理（供 E 线启动自检读取，见 §7.1）。
func (d *Drainer) Run(ctx context.Context) bool {
	d.mu.Lock()
	tasks := make([]drainTask, len(d.tasks))
	copy(tasks, d.tasks)
	counter := d.timeoutCounter
	d.mu.Unlock()

	if len(tasks) == 0 {
		return true
	}

	deadline := d.timeout
	if deadline <= 0 {
		deadline = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(tasks))
	for _, t := range tasks {
		wg.Add(1)
		go func(tk drainTask) {
			defer wg.Done()
			// R14 证据串：整理跑到一半 SIGTERM 时必须出现 "[drain] waiting …"。
			start := time.Now()
			d.logf("[drain] waiting %s (soft timeout %s)", tk.name, deadline)
			if err := tk.wait(runCtx); err != nil {
				errCh <- fmt.Errorf("%s: %w", tk.name, err)
				d.logf("[drain] stage %s returned error after %s: %v",
					tk.name, time.Since(start).Round(time.Millisecond), err)
				return
			}
			d.logf("[drain] stage %s completed in %s",
				tk.name, time.Since(start).Round(time.Millisecond))
		}(t)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		select {
		case err := <-errCh:
			d.logf("[drain] completed with error: %v", err)
		default:
			d.logf("[drain] all %d stages completed", len(tasks))
		}
		return true
	case <-runCtx.Done():
		d.logf("[drain] TIMEOUT after %s — not all stages finished; "+
			"unfinished markers are KEPT (E-line boot report will read them)", deadline)
		if counter != nil {
			counter.IncDrainTimeout()
		}
		return false
	}
}

// MarkUnfinished 原子写入「未完成的排水任务」标记（§10 冻结签名）。
// dataDir 与记忆同卷（非 /tmp）；写入 <dataDir>/<job>.drain.unfinished。
// 原子性：写 tmp → fsync → rename（rename 在 POSIX 同卷内原子）；
// fsync 保证断电（非仅 kill -9）时标记内容不丢——R17 断电实测虽出范围，
// 落盘语义仍按真原子写实现。
func MarkUnfinished(dataDir, job string) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, job+".drain.unfinished")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(job); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 内容先落盘，再 rename
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// 目录项也 fsync（best-effort：失败不影响标记已 rename 的事实）
	if df, derr := os.Open(dataDir); derr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// ClearUnfinished 清除「未完成的排水任务」标记（§10 冻结签名）。
// 只清 *.drain.unfinished（与 MarkUnfinished / bootreport.go 的后缀契约一致），
// 不误删其他 .unfinished 文件；跳过子目录。正常关闭路径调用（§7.1 ④）；
// 超时路径**不得**调用（标记要留给 E 线自检）。
func ClearUnfinished(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir empty")
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".drain.unfinished") {
			_ = os.Remove(filepath.Join(dataDir, name))
		}
	}
	return nil
}
