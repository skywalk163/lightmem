package kernel

import (
	"context"
	"testing"
)

func TestRunBootReport_NilBackend(t *testing.T) {
	rep, err := RunBootReport(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("RunBootReport(nil backend) error: %v", err)
	}
	if rep == nil {
		t.Fatalf("RunBootReport returned nil report")
	}
	if rep.Backend != "" {
		t.Fatalf("nil backend should yield empty Backend, got %q", rep.Backend)
	}
}

func TestRunBootReport_EmptyDataDir(t *testing.T) {
	b := &mockBackend{}
	rep, err := RunBootReport(context.Background(), b, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Backend != "mock" {
		t.Fatalf("Backend = %q, want mock", rep.Backend)
	}
	if rep.Unfinished != "" {
		t.Fatalf("Unfinished should be empty with empty dataDir")
	}
}

func TestRunBootReport_DetectsUnfinishedMarker(t *testing.T) {
	dir := t.TempDir()
	b := &mockBackend{}
	if err := MarkUnfinished(dir, "drain-x"); err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Unfinished != "drain-x" {
		t.Fatalf("Unfinished = %q, want drain-x", rep.Unfinished)
	}
}

func TestRunBootReport_ClearUnfinished(t *testing.T) {
	dir := t.TempDir()
	b := &mockBackend{}
	_ = MarkUnfinished(dir, "drain-y")
	if err := ClearUnfinished(dir); err != nil {
		t.Fatalf("ClearUnfinished: %v", err)
	}
	rep, err := RunBootReport(context.Background(), b, dir)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if rep.Unfinished != "" {
		t.Fatalf("Unfinished should be cleared, got %q", rep.Unfinished)
	}
}

func TestBootReportStructFields(t *testing.T) {
	rep := BootReport{
		Backend:      "pgvector",
		Table:        "memories",
		Columns:      "id,tenant_id",
		Rows:         100,
		VersionMatch: true,
		Unfinished:   "",
		RestorePoint: "2026-10-20T00:00:00Z",
	}
	if rep.Rows != 100 || !rep.VersionMatch || rep.Backend != "pgvector" {
		t.Fatalf("BootReport fields not carried")
	}
}

func TestBootReportMarkerPath(t *testing.T) {
	p := bootReportMarkerPath("/data", "job1")
	if p != "/data/job1.drain.unfinished" {
		t.Fatalf("marker path = %q", p)
	}
}
