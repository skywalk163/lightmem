// 优雅排水器（R29-T0 S0 接缝，最小可用实现）。
//
// 职责边界（任务书 §2.2 / §10）：
//   - T0 落 Drainer 结构与 Run/Add 最小实现（main.go 只调这两个）。
//   - D 线硬化：批量扫描（scan_batched.go）、各阶段的 wait 函数体、退避与超时细化。
//
// 默认关（红线④）：main.go 默认不向 Drainer 注册任何任务，Run 立即返回 true，
// 不会触发 os.Exit(1)。B4 行为变更（超时 os.Exit(1)）只在 D 线注册了会超时的
// 排水任务且真的超时才生效。
//
// 红线③：只用 stdlib。
package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Drainer 优雅排水器（§10 冻结）。
type Drainer struct {
	timeout time.Duration
	logf    func(string, ...any)
	mu      sync.Mutex
	tasks   []drainTask
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

// Add 注册一个排水阶段（§10 冻结签名）。默认关：main.go 不调用即无任务。
func (d *Drainer) Add(name string, wait func(ctx context.Context) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, drainTask{name: name, wait: wait})
}

// Run 执行所有已注册排水阶段（§10 冻结签名）。
//   - 无任务 → 立即返回 true（默认关，不触发 os.Exit）。
//   - 全部在 timeout 内完成 → 返回 true。
//   - 超时 → 返回 false（main.go 据此 os.Exit(1)，即 B4 行为变更）。
func (d *Drainer) Run(ctx context.Context) bool {
	d.mu.Lock()
	tasks := make([]drainTask, len(d.tasks))
	copy(tasks, d.tasks)
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
			d.logf("[drain] running %s", tk.name)
			if err := tk.wait(runCtx); err != nil {
				errCh <- fmt.Errorf("%s: %w", tk.name, err)
			}
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
		d.logf("[drain] TIMEOUT after %s — not all stages finished", deadline)
		return false
	}
}

// MarkUnfinished 原子写入「未完成的排水任务」标记（§10 冻结签名）。
// dataDir 与记忆同卷（非 /tmp）；写入 <dataDir>/<job>.drain.unfinished。
func MarkUnfinished(dataDir, job string) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, job+".drain.unfinished")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(job), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClearUnfinished 清除「未完成的排水任务」标记（§10 冻结签名）。
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
		if filepath.Ext(e.Name()) == ".unfinished" {
			_ = os.Remove(filepath.Join(dataDir, e.Name()))
		}
	}
	return nil
}
