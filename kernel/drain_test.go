package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDrainer_RunNoTasksReturnsTrue(t *testing.T) {
	d := NewDrainer(time.Second, nil)
	if !d.Run(context.Background()) {
		t.Fatalf("empty drainer Run should return true (default-off, no os.Exit)")
	}
}

func TestDrainer_AddAndRunSuccess(t *testing.T) {
	d := NewDrainer(time.Second, nil)
	d.Add("stage1", func(ctx context.Context) error { return nil })
	d.Add("stage2", func(ctx context.Context) error { return nil })
	if !d.Run(context.Background()) {
		t.Fatalf("all stages succeeded, Run should return true")
	}
}

func TestDrainer_RunTimeoutReturnsFalse(t *testing.T) {
	d := NewDrainer(30*time.Millisecond, nil)
	d.Add("slow", func(ctx context.Context) error {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if d.Run(context.Background()) {
		t.Fatalf("timeout should make Run return false (→ os.Exit(1), B4)")
	}
}

func TestDrainer_RunStageErrorStillReturnsTrue(t *testing.T) {
	d := NewDrainer(time.Second, nil)
	d.Add("failing", func(ctx context.Context) error { return context.Canceled })
	if !d.Run(context.Background()) {
		t.Fatalf("stage error should not flip timeout result; Run returns true")
	}
}

func TestNewDrainerDefaults(t *testing.T) {
	d := NewDrainer(0, nil)
	if d == nil {
		t.Fatalf("NewDrainer returned nil")
	}
	// timeout 0 应被 Run 内部兜底为 30s，不应 panic
	if !d.Run(context.Background()) {
		t.Fatalf("zero-timeout drainer should still succeed with no tasks")
	}
}

func TestMarkAndClearUnfinished(t *testing.T) {
	dir := t.TempDir()
	if err := MarkUnfinished(dir, "drain-a"); err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	// 标记存在
	if !fileExists(t, dir, "drain-a.drain.unfinished") {
		t.Fatalf("marker not written")
	}
	if err := ClearUnfinished(dir); err != nil {
		t.Fatalf("ClearUnfinished: %v", err)
	}
	if fileExists(t, dir, "drain-a.drain.unfinished") {
		t.Fatalf("marker not cleared")
	}
}

func TestMarkUnfinished_EmptyDataDir(t *testing.T) {
	if err := MarkUnfinished("", "x"); err == nil {
		t.Fatalf("empty dataDir should error")
	}
}

func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// ------------------------------------------------------------------
// R29-D 硬化新增用例（任务书 §7.1 / §7.2 D-unit + R14/R15 库级等价物）
// ------------------------------------------------------------------

// logCapture 并发安全地收集 logf 输出（-race 下断言用）。
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// fakeTimeoutCounter 造假的 DrainTimeoutCounter（B 线指标的测试替身）。
type fakeTimeoutCounter struct {
	mu    sync.Mutex
	count int
}

func (f *fakeTimeoutCounter) IncDrainTimeout() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
}

func (f *fakeTimeoutCounter) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func TestDrainer_TimeoutCounterInvoked(t *testing.T) {
	c := &fakeTimeoutCounter{}
	d := NewDrainer(40*time.Millisecond, nil)
	d.SetTimeoutCounter(c)
	d.Add("slow", func(ctx context.Context) error {
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	if d.Run(context.Background()) {
		t.Fatalf("timeout should make Run return false")
	}
	if c.n() != 1 {
		t.Fatalf("timeout counter = %d, want 1 (drain_timeouts_total)", c.n())
	}
	// 成功路径不计数
	c2 := &fakeTimeoutCounter{}
	d2 := NewDrainer(time.Second, nil)
	d2.SetTimeoutCounter(c2)
	d2.Add("fast", func(ctx context.Context) error { return nil })
	if !d2.Run(context.Background()) || c2.n() != 0 {
		t.Fatalf("success path: Run=true counter=0 expected, got Run=false? counter=%d", c2.n())
	}
	// 未注入计数器（默认关）：超时也不得 panic
	d3 := NewDrainer(20*time.Millisecond, nil)
	d3.Add("slow", func(ctx context.Context) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if d3.Run(context.Background()) {
		t.Fatalf("d3 timeout should return false")
	}
}

func TestDrainer_WaitingLogFormat(t *testing.T) {
	lc := &logCapture{}
	d := NewDrainer(time.Second, lc.logf)
	d.Add("consolidate", func(ctx context.Context) error { return nil })
	d.Add("access-flush", func(ctx context.Context) error { return nil })
	if !d.Run(context.Background()) {
		t.Fatalf("Run should succeed")
	}
	out := lc.joined()
	// R14 证据串：整理跑到一半 SIGTERM 必须出现 "[drain] waiting …"
	for _, want := range []string{"[drain] waiting consolidate", "[drain] waiting access-flush", "all 2 stages completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("drain log missing %q; got:/n%s", want, out)
		}
	}
}

func TestDrainer_StageCtxCanceledOnTimeout(t *testing.T) {
	d := NewDrainer(50*time.Millisecond, nil)
	observed := make(chan struct{})
	d.Add("watcher", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			close(observed)
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	})
	if d.Run(context.Background()) {
		t.Fatalf("timeout should make Run return false")
	}
	select {
	case <-observed:
		// 阶段在超时前后观察到 ctx 取消（Run 返回与阶段唤醒之间有微小时窗，
		// 给 500ms 等待而不是非阻塞断言，避免测试自身竞态）
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("stage did not observe ctx cancellation on timeout")
	}
}

func TestDrainer_TimeoutKeepsMarkersUntouched(t *testing.T) {
	dir := t.TempDir()
	if err := MarkUnfinished(dir, "shutdown"); err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	d := NewDrainer(30*time.Millisecond, nil)
	d.Add("slow", func(ctx context.Context) error {
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	if d.Run(context.Background()) {
		t.Fatalf("timeout should return false")
	}
	// 超时路径必须**保留**标记（§7.1：供 E 线启动自检读取）；Drainer 自身
	// 也无权清标记——清理只在正常关闭路径由 main.go 调 ClearUnfinished。
	if !fileExists(t, dir, "shutdown.drain.unfinished") {
		t.Fatalf("marker must survive drain timeout")
	}
}

func TestMarkUnfinished_NoTmpResidue(t *testing.T) {
	dir := t.TempDir()
	if err := MarkUnfinished(dir, "job-x"); err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "job-x.drain.unfinished" {
		t.Fatalf("dir should hold exactly the marker (tmp 必须已 rename 掉), got %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "job-x.drain.unfinished"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(raw) != "job-x" {
		t.Fatalf("marker content = %q, want job-x", string(raw))
	}
}

func TestClearUnfinished_OnlyDrainSuffix(t *testing.T) {
	dir := t.TempDir()
	// 三个文件：两个 drain 标记 + 一个无关 .unfinished；外加一个同名子目录
	for _, name := range []string{"a.drain.unfinished", "b.drain.unfinished", "other.unfinished"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "c.drain.unfinished"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := ClearUnfinished(dir); err != nil {
		t.Fatalf("ClearUnfinished: %v", err)
	}
	if fileExists(t, dir, "a.drain.unfinished") || fileExists(t, dir, "b.drain.unfinished") {
		t.Fatalf("drain markers not cleared")
	}
	if !fileExists(t, dir, "other.unfinished") {
		t.Fatalf("non-drain .unfinished must NOT be removed")
	}
	if fi, err := os.Stat(filepath.Join(dir, "c.drain.unfinished")); err != nil || !fi.IsDir() {
		t.Fatalf("directory with drain suffix must be skipped")
	}
}

// TestDrainWaitsForInFlightRoundBeforeClose 复刻 main.go 预埋请求的接线形态：
// 后台轮次 goroutine（WaitGroup）+ Drainer 等待阶段。证明「在途轮次跑完之前
// 不 Close」——R14「无 sql: database is closed / upsert failed」的库级等价物。
func TestDrainWaitsForInFlightRoundBeforeClose(t *testing.T) {
	b, err := newSQLiteBackend(context.Background(), &StorageConfig{
		SQLite: &SQLiteConfig{Path: filepath.Join(t.TempDir(), "drain.db")},
	})
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 模拟一轮慢整合：分批 upsert（与 consolidate RunOnce 同形态）
		for i := 0; i < 10; i++ {
			time.Sleep(10 * time.Millisecond)
			m := &Memory{
				MemoryID: "round-" + pad3(i), IdempotencyKey: "round-" + pad3(i),
				TenantID: "t1", UserID: "u1",
				Visibility: VisPrivate, MemoryKind: KindFact,
				Fact: "in-flight round write",
			}
			if err := b.Upsert(context.Background(), m); err != nil {
				t.Errorf("in-flight upsert failed: %v", err)
				return
			}
		}
	}()

	drainer := NewDrainer(3*time.Second, nil)
	drainer.Add("inflight-round", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if !drainer.Run(context.Background()) {
		t.Fatalf("drain should succeed within soft timeout")
	}
	wg.Wait()
	// 轮次全部写完后，后端必须仍然可用（证明没有 use-after-close）
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("backend unusable after drain: %v", err)
	}
	sc, ok := b.(MemoryScanner)
	if !ok {
		t.Fatalf("backend %s does not implement MemoryScanner", b.Name())
	}
	ms, err := sc.ScanAll(context.Background(), 5000)
	if err != nil {
		t.Fatalf("ScanAll after drain: %v", err)
	}
	if len(ms) != 10 {
		t.Fatalf("in-flight round wrote %d rows, want 10 (全生效)", len(ms))
	}
}

// TestDrainTimeoutPath_CallerMustNotClose 反证 B4 的禁令面：超时后
// Drainer 不持有后端、不 Close；调用方（main.go）按协议 os.Exit(1)，
// 而不是「Close 后仍让 goroutine 跑」。这里断言超时返回 false 且后端仍可用
// ——把「谁有权 Close」留在调用方，从机制上杜绝 use-after-close。
func TestDrainTimeoutPath_CallerMustNotClose(t *testing.T) {
	b, err := newSQLiteBackend(context.Background(), &StorageConfig{
		SQLite: &SQLiteConfig{Path: filepath.Join(t.TempDir(), "drain2.db")},
	})
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(400 * time.Millisecond) // 轮次远超排水预算
	}()
	drainer := NewDrainer(40*time.Millisecond, nil)
	drainer.Add("slow-round", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if drainer.Run(context.Background()) {
		t.Fatalf("timeout should return false (→ caller os.Exit(1), B4)")
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Drainer must not close backend on timeout: %v", err)
	}
	wg.Wait()
}
