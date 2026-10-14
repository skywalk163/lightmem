// 包记忆抽取单测（R11-B 任务书 B.2）。
// 在内存 SQLite 后端上验证：8 包各抽取 3 条结构化记忆（元数据/配置/变更记录）、
// 事实文本可被检索命中、幂等重抽不重复写入。
// 全部确定性，不依赖 wall clock 断言具体时间值。
package kernel

import (
	"context"
	"strings"
	"testing"
)

// newPkgMemBackend 构造内存 SQLite 后端用于包记忆测试。
func newPkgMemBackend(t *testing.T) StorageBackend {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	return b
}

func TestExtractPackageMemory_AllEight(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	if len(FirstPartyPackages) != 8 {
		t.Fatalf("expect 8 first-party packages, got %d", len(FirstPartyPackages))
	}
	expectedNames := []string{"gitea", "git-connector", "registry", "backup", "search", "llm-gateway", "mesh-sync", "webui"}
	for _, name := range expectedNames {
		if _, ok := FirstPartyPackages[name]; !ok {
			t.Fatalf("missing expected first-party package: %s", name)
		}
	}

	total := 0
	for name := range FirstPartyPackages {
		n, err := ExtractPackageMemory(ctx, b, name)
		if err != nil {
			t.Fatalf("ExtractPackageMemory(%s): %v", name, err)
		}
		if n != 3 {
			t.Fatalf("package %s: want 3 memories extracted, got %d", name, n)
		}
		total += n
	}
	if total != 24 {
		t.Fatalf("want 24 total memories (8 x 3), got %d", total)
	}

	// fact 计数：metadata+config = 16，changelog(event) = 8
	facts, err := b.CountByKind(ctx, KindFact, "system")
	if err != nil {
		t.Fatalf("CountByKind(fact): %v", err)
	}
	if facts != 16 {
		t.Fatalf("want 16 fact memories, got %d", facts)
	}
	events, err := b.CountByKind(ctx, KindEvent, "system")
	if err != nil {
		t.Fatalf("CountByKind(event): %v", err)
	}
	if events != 8 {
		t.Fatalf("want 8 event memories, got %d", events)
	}
}

func TestExtractPackageMemory_Unknown(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	_, err := ExtractPackageMemory(ctx, b, "no-such-package")
	if err == nil {
		t.Fatal("expected error for unknown package, got nil")
	}
	if !strings.Contains(err.Error(), "unknown package") {
		t.Fatalf("error should mention unknown package, got: %v", err)
	}
}

func TestExtractPackageMemory_Idempotent(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	if _, err := ExtractPackageMemory(ctx, b, "gitea"); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	countAfterFirst, err := b.CountByKind(ctx, KindFact, "system")
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}

	// 重抽应幂等：idempotency_key 稳定，不新增行
	if _, err := ExtractPackageMemory(ctx, b, "gitea"); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	countAfterSecond, err := b.CountByKind(ctx, KindFact, "system")
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}
	if countAfterSecond != countAfterFirst {
		t.Fatalf("idempotency broken: count before=%d after=%d", countAfterFirst, countAfterSecond)
	}
}

func TestExtractPackageMemory_RecallHits(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	// 抽 gitea 包记忆后，「这个包是干什么的」类查询应命中其元数据事实
	if _, err := ExtractPackageMemory(ctx, b, "gitea"); err != nil {
		t.Fatalf("extract gitea: %v", err)
	}

	// 直接用 Query 文本检索验证事实可被命中（B.4 验收核心）
	res, err := b.Query(ctx, QueryParams{
		TenantID: "system",
		Viewer:   "system",
		Text:     "这个包是干什么的 gitea 代码托管",
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least one hit for gitea package memory, got none")
	}
	hit := false
	for _, r := range res {
		if strings.Contains(strings.ToLower(r.Fact), "gitea") &&
			strings.Contains(r.Fact, "官方") {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("expected a gitea metadata fact in top-%d results, got %v", len(res), res)
	}
	t.Logf("top hit fact: %s", res[0].Fact)
}

func TestExtractPackageMemoryFromManifest(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	mf := &PackageManifest{
		Name:         "community-tool",
		Version:      "2.1.0",
		Description:  "社区连接器",
		Author:       "someone",
		Trust:        TrustCommunity,
		Type:         PkgConnector,
		Entrypoint:   "bin/tool",
		Permissions:  []string{"read:data"},
		MemoryTypes:  []string{string(KindFact)},
		Dependencies: []string{"registry"},
	}
	n, err := ExtractPackageMemoryFromManifest(ctx, b, mf)
	if err != nil {
		t.Fatalf("extract from manifest: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 memories from community manifest, got %d", n)
	}

	res, err := b.Query(ctx, QueryParams{
		TenantID: "system",
		Viewer:   "system",
		Text:     "community-tool 社区连接器",
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected hit for community-tool package memory")
	}
}

func TestExtractPackageMemory_NilManifest(t *testing.T) {
	ctx := context.Background()
	b := newPkgMemBackend(t)
	defer b.Close()

	_, err := ExtractPackageMemoryFromManifest(ctx, b, nil)
	if err == nil {
		t.Fatal("expected error for nil manifest")
	}
}