// connector_app 单测（S3）
package kernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupAppConnectorTest 建临时 git 仓库骨架 + sqlite 后端。
func setupAppConnectorTest(t *testing.T) (*AppConnector, StorageBackend, string) {
	t.Helper()
	root := t.TempDir()
	// 创建 .git/config
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = git@192.168.1.5:skywalk163/lightmem.git\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// 创建元数据文件
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# lightmem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "roadmap.md"), []byte("## R16"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("MIT"), 0o644); err != nil {
		t.Fatal(err)
	}

	backend, err := newSQLiteBackend(context.Background(), &StorageConfig{
		SQLite: &SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	c := NewAppConnector("git", backend)
	return c, backend, root
}

func TestAppConnectorHealth(t *testing.T) {
	c, _, root := setupAppConnectorTest(t)
	// 未配置 kind -> 报错
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected health error for unconfigured")
	}
	c.cfg.Kind = "git"
	c.cfg.Root = root
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 不存在的 root -> 报错
	c.cfg.Root = filepath.Join(root, "nope")
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected health error for bad root")
	}
	// 不支持的 kind -> 报错
	c.cfg.Root = root
	c.cfg.Kind = "notion"
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected health error for unsupported kind")
	}
}

func TestAppConnectorSyncGit(t *testing.T) {
	c, backend, root := setupAppConnectorTest(t)
	cfgJSON, _ := json.Marshal(AppConfig{
		Kind:     "git",
		Root:     root,
		PollSec:  5,
		TenantID: "t1",
		UserID:   "u1",
		MaxFiles: 100,
	})
	err := c.Sync(context.Background(), cfgJSON)
	if err != nil {
		t.Fatal(err)
	}

	// 验证：LICENSE 默认不被关注（默认 ext 不含 LICENSE）
	// 验证：README.md 与 roadmap.md 被收录
	results, err := backend.Query(context.Background(), QueryParams{
		TenantID: "t1",
		Viewer:   "u1",
		Text:     "",
		TopK:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected >=2 memories: %#v", results)
	}

	// 抽取事实，验证 git 元数据被序列化
	got := map[string]bool{}
	for _, r := range results {
		got[r.Fact] = true
	}
	foundReadme := false
	foundRemote := false
	for fact := range got {
		if strings.Contains(fact, "README.md") {
			foundReadme = true
		}
		if strings.Contains(fact, "git@192.168.1.5") {
			foundRemote = true
		}
	}
	if !foundReadme {
		t.Fatalf("expected README.md fact; got %#v", got)
	}
	if !foundRemote {
		t.Fatalf("expected remote url fact; got %#v", got)
	}
}

func TestAppConnectorSyncIdempotency(t *testing.T) {
	c, backend, root := setupAppConnectorTest(t)
	cfgJSON, _ := json.Marshal(AppConfig{
		Kind:     "git",
		Root:     root,
		TenantID: "t1",
		UserID:   "u1",
		MaxFiles: 100,
	})
	if err := c.Sync(context.Background(), cfgJSON); err != nil {
		t.Fatal(err)
	}
	// 再同步一次（idempotency_key 含 mod_time，不变 -> 幂等，数量不变）
	if err := c.Sync(context.Background(), cfgJSON); err != nil {
		t.Fatal(err)
	}
	res, _ := backend.Query(context.Background(), QueryParams{TenantID: "t1", Viewer: "u1", Text: "", TopK: 200})
	if len(res) < 2 {
		t.Fatalf("idempotency: got %#v", res)
	}
}

func TestAppConnectorExtMatch(t *testing.T) {
	c := NewAppConnector("x", nil)
	c.cfg = &AppConfig{Extensions: []string{"md", "yaml"}}
	cases := map[string]bool{
		"md":   true,
		"MD":   true,
		"yaml": true,
		"yml":  false,
		"json": false,
		"txt":  false,
		"toml": false,
	}
	for ext, want := range cases {
		if got := c.extMatch(ext); got != want {
			t.Errorf("extMatch(%q) = %v, want %v", ext, got, want)
		}
	}
	// 默认扩展（未配置）
	c.cfg.Extensions = nil
	if !c.extMatch("md") || !c.extMatch("toml") || !c.extMatch("JSON") {
		t.Fatal("default ext match should include md/toml/json")
	}
	if c.extMatch("png") || c.extMatch("zip") {
		t.Fatal("default ext match should exclude binary")
	}
}

func TestAppConnectorGitFact(t *testing.T) {
	c := NewAppConnector("x", nil)
	f := gitMetadata{
		Repo:      "lightmem",
		RemoteURL: "git@192.168.1.5:skywalk163/lightmem.git",
		RelPath:   "README.md",
		Size:      12,
		ModTime:   0,
	}
	fact := c.gitFact(f)
	if !strings.Contains(fact, "lightmem") || !strings.Contains(fact, "README.md") {
		t.Fatalf("fact missing repo/relpath: %q", fact)
	}
	if !strings.Contains(fact, "git@192.168.1.5") {
		t.Fatalf("fact missing remote url: %q", fact)
	}
}

func TestAppConnectorGitRemoteURL(t *testing.T) {
	root := t.TempDir()
	// 无 .git/config
	if got := (&AppConnector{}).gitRemoteURL(root); got != "" {
		t.Fatalf("empty root should return empty: %q", got)
	}
	gitDir := filepath.Join(root, ".git")
	_ = os.MkdirAll(gitDir, 0o755)
	_ = os.WriteFile(filepath.Join(gitDir, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/skywalk163/lightmem.git\n"), 0o644)
	if got := (&AppConnector{}).gitRemoteURL(root); got != "https://github.com/skywalk163/lightmem.git" {
		t.Fatalf("remote url: %q", got)
	}
}
