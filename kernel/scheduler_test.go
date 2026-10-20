package kernel

import (
	"context"
	"testing"
	"time"
)

func TestDefaultMaintenanceJobs_ReturnsEmptyByDefault(t *testing.T) {
	jobs := DefaultMaintenanceJobs(MaintenanceConfig{})
	if len(jobs) != 0 {
		t.Fatalf("default jobs = %d, want 0 (S0 占位，C 线填实)", len(jobs))
	}
}

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
	// 默认关路径：main.go 中 maint 可能为 nil，Stop 必须安全。
	var m *Maintenance
	m.Stop() // 不应 panic
}

func TestMaintenance_StopTwiceSafe(t *testing.T) {
	m := StartMaintenance(context.Background(), nil, nil)
	m.Stop()
	m.Stop() // 第二次不应 panic
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
}

func TestMaintenanceConfigDefaults(t *testing.T) {
	cfg := MaintenanceConfig{}
	if cfg.Consolidate || cfg.LifecycleInterval != 0 {
		t.Fatalf("MaintenanceConfig zero value should be default-off")
	}
}
