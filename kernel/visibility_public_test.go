package kernel

import (
	"context"
	"testing"
)

// R23.2 · D5 社区公开：public 记忆跨租户可读，任何带身份的 viewer 都能看到。
func TestCanViewPublic(t *testing.T) {
	m := &Memory{MemoryID: "m-pub", TenantID: "tenant-a", UserID: "alice", Visibility: VisPublic}

	if !m.canView("bob", "tenant-b") {
		t.Error("public memory: viewer of another tenant must see it, got false")
	}
	if !m.canView("bob", "") {
		t.Error("public memory: viewer without tenant must still see it, got false")
	}
	if m.canView("", "tenant-b") {
		t.Error("public memory: anonymous viewer (empty identity) must NOT see it, got true")
	}

	// 既有 4 值语义零回归：private 仍只本人可见。
	priv := &Memory{MemoryID: "m-p", TenantID: "tenant-a", UserID: "alice", Visibility: VisPrivate}
	if priv.canView("bob", "tenant-a") {
		t.Error("private memory leaked to another viewer")
	}
	// tenant 仍限本租户。
	ten := &Memory{MemoryID: "m-t", TenantID: "tenant-a", UserID: "alice", Visibility: VisTenant}
	if ten.canView("bob", "tenant-b") {
		t.Error("tenant memory leaked cross-tenant")
	}
}

// R23.2：public 虽可被任意人读，但只有 owner 可写/改/删 —— 防止借 public 越权改删。
func TestEnforcePrivateOwnerPublic(t *testing.T) {
	ctx := withIdentity(context.Background(), Identity{UserID: "skywalk"})

	if err := EnforcePrivateOwner(ctx, "agent-dsh", "public"); err == nil {
		t.Error("public memory owned by another user: want error, got nil")
	}
	if err := EnforcePrivateOwner(ctx, "skywalk", "public"); err != nil {
		t.Errorf("public memory owned by self: want nil, got %v", err)
	}
	if err := EnforcePrivateOwner(ctx, "", "public"); err != nil {
		t.Errorf("public with empty owner: want nil (falls back to token), got %v", err)
	}
	// 未认证（透传路径）不拦截（保持既有行为）
	if err := EnforcePrivateOwner(context.Background(), "agent-dsh", "public"); err != nil {
		t.Errorf("unauthenticated public: want nil, got %v", err)
	}
	// 既有 non-private/ten-public 之外的类型仍不拦截（零回归）。
	if err := EnforcePrivateOwner(ctx, "agent-dsh", "tenant"); err != nil {
		t.Errorf("tenant visibility: want nil, got %v", err)
	}
}
