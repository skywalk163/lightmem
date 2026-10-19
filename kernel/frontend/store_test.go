package frontend

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreCreateAndGetCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	err := s.CreateCredential(ctx, Credential{
		Username: "alice", PasswordHash: "pbkdf2$x", TenantID: "dshteam", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	got, err := s.GetCredential(ctx, "alice")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if got.Username != "alice" || got.TenantID != "dshteam" || got.DisplayName != "Alice" {
		t.Fatalf("got = %+v", got)
	}
	if got.DisabledAt != "" {
		t.Fatalf("new credential should not be disabled, got %q", got.DisabledAt)
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := Credential{Username: "bob", PasswordHash: "h", TenantID: "t"}
	if err := s.CreateCredential(ctx, c); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := s.CreateCredential(ctx, c)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create err = %v, want ErrConflict", err)
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetCredential(context.Background(), "nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCredential(missing) err = %v, want ErrNotFound", err)
	}
}

func TestStoreMultipleTenants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, u := range []string{"a", "b", "c"} {
		if err := s.CreateCredential(ctx, Credential{Username: u, PasswordHash: "h", TenantID: "t" + u}); err != nil {
			t.Fatalf("create %s: %v", u, err)
		}
	}
	for _, u := range []string{"a", "b", "c"} {
		got, err := s.GetCredential(ctx, u)
		if err != nil {
			t.Fatalf("get %s: %v", u, err)
		}
		if got.TenantID != "t"+u {
			t.Fatalf("tenant for %s = %q, want t%s", u, got.TenantID, u)
		}
	}
}

func TestStoreRecordLogin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordLogin(ctx, "alice", "127.0.0.1", "go-test"); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
	if err := s.RecordLogin(ctx, "bob", "10.0.0.1", "curl"); err != nil {
		t.Fatalf("RecordLogin2: %v", err)
	}
	if err := s.MarkSessionSeen(ctx, "alice"); err != nil {
		t.Fatalf("MarkSessionSeen: %v", err)
	}
}

func TestStoreCloseNilSafe(t *testing.T) {
	var s *Store
	if err := s.Close(); err != nil {
		t.Fatalf("nil Close err = %v", err)
	}
}
