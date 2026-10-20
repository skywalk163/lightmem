package kernel

import (
	"context"
	"os"
	"path/filepath"
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
