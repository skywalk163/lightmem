// R24-D 测试：L3 引用式存储（refstore）。
//
// 覆盖 §7.2 全部七条判据：
//  1. 往返一致（含中文、1MB 大文本、100 条随机）
//  2. digest 校验（篡改后 Hydrate 报错，不返回正文）
//  3. 零回归（ExternalRef 为空的既有记忆读写路径逐位一致）
//  4. 默认关（threshold<=0 或 archive.path 未配时不落盘）
//  5. fail-closed（backend=s3 时构造 store 返回错误）
//  6. 合规风险显式化（报告里写，测试验证 PurgeRefsForMemories 真能删）
//  7. 测试规模 ≥12
package kernel

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
)

// ------------------------------------------------------------------
// 辅助函数
// ------------------------------------------------------------------

func tempRefStoreRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func newTestLocalStore(t *testing.T) *LocalRefStore {
	t.Helper()
	root := tempRefStoreRoot(t)
	store, err := NewLocalRefStore(&ArchiveConfig{Path: root, Backend: "local"})
	if err != nil {
		t.Fatalf("NewLocalRefStore failed: %v", err)
	}
	return store
}

func makeMemory(fact string, externalRef *string) *Memory {
	return &Memory{
		MemoryID:       "01JTEST0001",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Fact:           fact,
		ExternalRef:    externalRef,
		SourceEvidence: &SourceEvidence{Origin: "test", Snippet: "evidence snippet"},
	}
}

// ------------------------------------------------------------------
// 判据 5：fail-closed（backend != "local" 时构造报错）
// ------------------------------------------------------------------

func TestNewLocalRefStore_NilConfig(t *testing.T) {
	_, err := NewLocalRefStore(nil)
	if err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error should mention nil config, got: %v", err)
	}
}

func TestNewLocalRefStore_EmptyPath(t *testing.T) {
	_, err := NewLocalRefStore(&ArchiveConfig{Path: "", Backend: "local"})
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("error should mention path, got: %v", err)
	}
}

func TestNewLocalRefStore_S3Backend_FailClosed(t *testing.T) {
	_, err := NewLocalRefStore(&ArchiveConfig{Path: "/tmp/archive", Backend: "s3"})
	if err == nil {
		t.Fatal("expected fail-closed error for s3 backend, got nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error should say not implemented, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no silent fallback") {
		t.Errorf("error should explicitly say no silent fallback, got: %v", err)
	}
}

func TestNewLocalRefStore_LocalBackend_Success(t *testing.T) {
	root := tempRefStoreRoot(t)
	store, err := NewLocalRefStore(&ArchiveConfig{Path: root, Backend: "local"})
	if err != nil {
		t.Fatalf("expected success for local backend, got: %v", err)
	}
	if store.root != root {
		t.Errorf("store.root = %q, want %q", store.root, root)
	}
	// 根目录应已创建
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Errorf("archive root should be created as directory")
	}
}

// ------------------------------------------------------------------
// 判据 1：往返一致（基础 / 中文 / 1MB 大文本）
// ------------------------------------------------------------------

func TestLocalRefStore_PutGet_BasicRoundTrip(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	original := []byte("hello world — lightmem refstore round trip test")
	ref, digest, err := store.Put(ctx, "test-key", original)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if !strings.HasPrefix(ref, "lims://local/") {
		t.Errorf("ref should start with lims://local/, got: %s", ref)
	}
	if len(digest) != 64 {
		t.Errorf("digest should be 64 hex chars, got %d: %s", len(digest), digest)
	}

	readBack, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(original, readBack) {
		t.Errorf("round trip mismatch:\n original: %q\n readback:  %q", original, readBack)
	}
}

func TestLocalRefStore_PutGet_ChineseContent(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	original := []byte("光明记忆系统的中文测试正文。这是一段包含 UTF-8 多字节字符的内容，用于验证往返一致性。")
	ref, _, err := store.Put(ctx, "cn-key", original)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	readBack, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(original, readBack) {
		t.Errorf("Chinese round trip mismatch")
	}
}

func TestLocalRefStore_PutGet_Large1MB(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	// 生成 1MB 随机内容
	original := make([]byte, 1024*1024)
	if _, err := rand.Read(original); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}
	ref, _, err := store.Put(ctx, "large-key", original)
	if err != nil {
		t.Fatalf("Put 1MB failed: %v", err)
	}
	readBack, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get 1MB failed: %v", err)
	}
	if !bytes.Equal(original, readBack) {
		t.Errorf("1MB round trip mismatch (lengths: original=%d, readback=%d)", len(original), len(readBack))
	}
}

func TestLocalRefStore_PutGet_100RandomEntries(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		// 每条生成随机长度（10B ~ 5KB）的随机内容
		size := 10 + (i*537)%5000
		original := make([]byte, size)
		if _, err := rand.Read(original); err != nil {
			t.Fatalf("rand.Read %d failed: %v", i, err)
		}
		ref, _, err := store.Put(ctx, "", original)
		if err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
		readBack, err := store.Get(ctx, ref)
		if err != nil {
			t.Fatalf("Get %d failed: %v", i, err)
		}
		if !bytes.Equal(original, readBack) {
			t.Errorf("entry %d round trip mismatch (size=%d)", i, size)
		}
	}
}

// ------------------------------------------------------------------
// Stat / Delete / GetNotFound
// ------------------------------------------------------------------

func TestLocalRefStore_Stat_ReturnsCorrectMeta(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	original := []byte("stat test content — should match size and digest")
	ref, expectedDigest, err := store.Put(ctx, "stat-key", original)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	meta, err := store.Stat(ctx, ref)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if meta.Size != int64(len(original)) {
		t.Errorf("Stat.Size = %d, want %d", meta.Size, len(original))
	}
	// digest 应与 Put 返回的 digest 前 32 位一致（ref 只存 32 位前缀）
	if !strings.HasPrefix(expectedDigest, meta.Digest[:32]) {
		t.Errorf("Stat.Digest = %q, should start with Put digest prefix %q", meta.Digest, expectedDigest[:32])
	}
	if meta.CreatedAt.IsZero() {
		t.Error("Stat.CreatedAt should not be zero")
	}
}

func TestLocalRefStore_Delete_Idempotent(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	ref, _, err := store.Put(ctx, "del-key", []byte("to be deleted"))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	// 第一次删
	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("first Delete failed: %v", err)
	}
	// 第二次删（幂等，不报错）
	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("second Delete (idempotent) failed: %v", err)
	}
	// Get 应报错
	if _, err := store.Get(ctx, ref); err == nil {
		t.Error("Get after delete should error")
	}
}

func TestLocalRefStore_Get_NotFound(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	// 构造一个不存在的 ref（digest 全 0）
	nonExistentRef := "lims://local/00000000000000000000000000000000"
	_, err := store.Get(ctx, nonExistentRef)
	if err == nil {
		t.Error("Get non-existent ref should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say not found, got: %v", err)
	}
}

// ------------------------------------------------------------------
// 判据 4：默认关（threshold <= 0 / store nil 时不落盘）
// ------------------------------------------------------------------

func TestArchiveWithRef_Disabled_ThresholdZero(t *testing.T) {
	ctx := context.Background()
	m := makeMemory(strings.Repeat("x", 10000), nil) // 10KB，远超阈值

	// threshold = 0 → 禁用
	result, err := ArchiveWithRef(ctx, newTestLocalStore(t), m, 0)
	if err != nil {
		t.Fatalf("ArchiveWithRef with threshold=0 should not error: %v", err)
	}
	if result.ExternalRef != nil {
		t.Error("threshold=0 should not set ExternalRef (disabled)")
	}
	if len(result.Fact) != 10000 {
		t.Errorf("threshold=0 should not truncate Fact, len=%d, want 10000", len(result.Fact))
	}
}

func TestArchiveWithRef_UnderThreshold_NoChange(t *testing.T) {
	ctx := context.Background()
	m := makeMemory(strings.Repeat("y", 1000), nil) // 1KB，低于默认 4KB

	result, err := ArchiveWithRef(ctx, newTestLocalStore(t), m, DefaultArchiveThreshold)
	if err != nil {
		t.Fatalf("ArchiveWithRef under threshold should not error: %v", err)
	}
	if result.ExternalRef != nil {
		t.Error("under threshold should not externalize")
	}
	if len(result.Fact) != 1000 {
		t.Errorf("under threshold should not truncate, len=%d", len(result.Fact))
	}
}

func TestArchiveWithRef_OverThreshold_Externalized(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	longFact := strings.Repeat("z", 5000) + " — this is a long fact that should be externalized"
	m := makeMemory(longFact, nil)

	result, err := ArchiveWithRef(ctx, store, m, DefaultArchiveThreshold)
	if err != nil {
		t.Fatalf("ArchiveWithRef over threshold failed: %v", err)
	}
	if result.ExternalRef == nil {
		t.Fatal("over threshold should set ExternalRef")
	}
	if !strings.HasPrefix(*result.ExternalRef, "lims://local/") {
		t.Errorf("ExternalRef should be lims://local/..., got: %s", *result.ExternalRef)
	}
	// Fact 应被截断为 200 字符 + "…"
	if len(result.Fact) > ExternalRefSummaryLen+3 {
		t.Errorf("Fact should be truncated to ~200 chars, got len=%d: %q...", len(result.Fact), result.Fact[:50])
	}
	if !strings.HasSuffix(result.Fact, "…") {
		t.Errorf("truncated Fact should end with …, got: %q", result.Fact)
	}
	// Snippet 应被清空（避免密级泄露）
	if result.SourceEvidence.Snippet != "" {
		t.Errorf("Snippet should be cleared after externalization, got: %q", result.SourceEvidence.Snippet)
	}
}

// ------------------------------------------------------------------
// 判据 3：零回归（ExternalRef 为空时 Hydrate 不改 m）
// ------------------------------------------------------------------

func TestHydrate_EmptyExternalRef_NoChange(t *testing.T) {
	ctx := context.Background()
	originalFact := "this is a normal fact, not externalized"
	m := makeMemory(originalFact, nil) // ExternalRef = nil

	result, err := Hydrate(ctx, newTestLocalStore(t), m)
	if err != nil {
		t.Fatalf("Hydrate with nil ExternalRef should not error: %v", err)
	}
	if result.Fact != originalFact {
		t.Errorf("Hydrate should not change Fact when ExternalRef is nil, got: %q", result.Fact)
	}
}

func TestHydrate_StoreNil_EmptyRef_NoError(t *testing.T) {
	ctx := context.Background()
	m := makeMemory("fact with no external ref", nil)

	// store = nil, ExternalRef = nil → 应该直接返回，不报错
	result, err := Hydrate(ctx, nil, m)
	if err != nil {
		t.Fatalf("Hydrate with nil store and nil ref should not error: %v", err)
	}
	if result.Fact != "fact with no external ref" {
		t.Errorf("Fact should be unchanged")
	}
}

// ------------------------------------------------------------------
// 判据 2：digest 校验（篡改后 Hydrate 报错）
// ------------------------------------------------------------------

func TestHydrate_Success_RestoresFact(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	originalFact := strings.Repeat("w", 5000) + " — full original content"
	originalSnippet := "original evidence snippet that should also be restored"
	m := &Memory{
		MemoryID:       "01JTEST0001",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Fact:           originalFact,
		SourceEvidence: &SourceEvidence{Origin: "test", Snippet: originalSnippet},
	}

	// 先外置
	archived, err := ArchiveWithRef(ctx, store, m, DefaultArchiveThreshold)
	if err != nil {
		t.Fatalf("ArchiveWithRef failed: %v", err)
	}
	if archived.ExternalRef == nil {
		t.Fatal("expected ExternalRef after archiving")
	}
	// 外置后 Snippet 应被清空
	if archived.SourceEvidence.Snippet != "" {
		t.Errorf("after archiving, Snippet should be cleared, got: %q", archived.SourceEvidence.Snippet)
	}

	// 再水合
	hydrated, err := Hydrate(ctx, store, archived)
	if err != nil {
		t.Fatalf("Hydrate failed: %v", err)
	}
	if hydrated.Fact != originalFact {
		t.Errorf("Hydrate should restore original Fact:\n original len=%d\n hydrated len=%d",
			len(originalFact), len(hydrated.Fact))
	}
	// Snippet 也应被恢复
	if hydrated.SourceEvidence == nil || hydrated.SourceEvidence.Snippet != originalSnippet {
		t.Errorf("Hydrate should restore original Snippet: expected %q, got %+v", originalSnippet, hydrated.SourceEvidence)
	}
}

func TestHydrate_TamperedFile_DigestMismatch(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	originalFact := strings.Repeat("v", 5000) + " — will be tampered"
	m := makeMemory(originalFact, nil)

	archived, err := ArchiveWithRef(ctx, store, m, DefaultArchiveThreshold)
	if err != nil {
		t.Fatalf("ArchiveWithRef failed: %v", err)
	}

	// 篡改存储文件：直接写一个不同的内容进去
	ref := *archived.ExternalRef
	path, err := store.refToPath(ref)
	if err != nil {
		t.Fatalf("refToPath failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("TAMPERED CONTENT — original digest no longer matches"), 0o644); err != nil {
		t.Fatalf("write tampered file failed: %v", err)
	}

	// Hydrate 应报错（digest mismatch），且不返回摘要冒充正文
	_, err = Hydrate(ctx, store, archived)
	if err == nil {
		t.Fatal("expected digest mismatch error after tampering, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error should mention digest mismatch, got: %v", err)
	}
}

// ------------------------------------------------------------------
// 判据 6：PurgeRefsForMemories 真能删
// ------------------------------------------------------------------

func TestPurgeRefsForMemories_DeletesExternalBodies(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	// 造 3 条记忆，2 条有外置正文，1 条没有
	m1 := makeMemory(strings.Repeat("a", 5000), nil)
	m2 := makeMemory(strings.Repeat("b", 5000), nil)
	m3 := makeMemory("small fact", nil)

	ArchiveWithRef(ctx, store, m1, DefaultArchiveThreshold)
	ArchiveWithRef(ctx, store, m2, DefaultArchiveThreshold)

	if m1.ExternalRef == nil || m2.ExternalRef == nil {
		t.Fatal("expected both m1 and m2 to have ExternalRef after archiving")
	}

	// 先确认两个正文都存在
	if _, err := store.Get(ctx, *m1.ExternalRef); err != nil {
		t.Fatalf("m1 body should exist before purge: %v", err)
	}

	// 批量删
	errs := PurgeRefsForMemories(ctx, store, []*Memory{m1, m2, m3})
	if len(errs) != 0 {
		t.Errorf("PurgeRefsForMemories should not error, got: %v", errs)
	}

	// 确认已删
	if _, err := store.Get(ctx, *m1.ExternalRef); err == nil {
		t.Error("m1 body should be deleted after purge")
	}
	if _, err := store.Get(ctx, *m2.ExternalRef); err == nil {
		t.Error("m2 body should be deleted after purge")
	}
}

// ------------------------------------------------------------------
// 边界：ref 格式校验
// ------------------------------------------------------------------

func TestLocalRefStore_InvalidRefScheme(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "http://example.com/file.txt")
	if err == nil {
		t.Error("Get with wrong scheme should error")
	}
	if !strings.Contains(err.Error(), "invalid ref scheme") {
		t.Errorf("error should say invalid ref scheme, got: %v", err)
	}
}

func TestLocalRefStore_PutIdempotent_SameDigest(t *testing.T) {
	store := newTestLocalStore(t)
	ctx := context.Background()

	data := []byte("idempotent put test — same digest should not duplicate")
	ref1, digest1, err := store.Put(ctx, "", data)
	if err != nil {
		t.Fatalf("first Put failed: %v", err)
	}
	ref2, digest2, err := store.Put(ctx, "", data)
	if err != nil {
		t.Fatalf("second Put failed: %v", err)
	}
	if ref1 != ref2 {
		t.Errorf("same content should produce same ref:\n ref1=%s\n ref2=%s", ref1, ref2)
	}
	if digest1 != digest2 {
		t.Errorf("same content should produce same digest")
	}
}
