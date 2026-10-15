// R11-B 包记忆 E2E 集成测试（任务书 B.4）。
//
// 覆盖两条验收：
//  1. 装包 -> 抽包记忆 -> 查询「这个包是干什么的」命中抽取的包记忆；
//  2. 错误安装顺序（含循环依赖）-> 拓扑排序报错提示。
//
// 方式：内存 SQLite 后端 + kernel.PackageLifecycle + kernel.InstallOrder +
//
//	kernel.ExtractPackageMemory，走真实 kernel 公共 API 与既有语义。
//
// 红线遵守：
//   - 内存 SQLite（:memory:），不依赖真实 PG / 网络 / 外部服务；
//   - 不调 LLM / 外部 embedder；全部确定性；
//   - 只调用 kernel 既有公共 API，不改 kernel Schema/融合/既有 SQL/对外格式。
//
// manifest 目录定位：eval/packages/manifests（相对本文件上溯 3 层到仓库根）。
package packages

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lightmem/kernel"
)

// repoRoot 返回 lightmem 仓库根目录绝对路径（基于本测试文件位置）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// 从本文件（eval/e2e/packages/package_e2e_test.go）逐级上溯，直到找到 go.mod。
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("cannot locate repo root (go.mod) from %s", thisFile)
		}
		dir = parent
	}
}

// loadManifests 从 eval/packages/manifests/*.json 装载全部包 manifest。
func loadManifests(t *testing.T) map[string]*kernel.PackageManifest {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "eval", "packages", "manifests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read manifests dir %s: %v", dir, err)
	}
	out := map[string]*kernel.PackageManifest{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var mf kernel.PackageManifest
		if err := json.Unmarshal(data, &mf); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Name(), err)
		}
		out[mf.Name] = &mf
	}
	return out
}

// newMemBackend 装配内存 SQLite 后端。
func newMemBackend(t *testing.T) kernel.StorageBackend {
	t.Helper()
	b, err := kernel.NewStorageBackend(context.Background(), &kernel.Profile{
		Profile:        kernel.TierSolo,
		StorageBackend: kernel.BackendSQLite,
		Storage:        &kernel.StorageConfig{SQLite: &kernel.SQLiteConfig{Path: ":memory:"}},
	})
	if err != nil {
		t.Fatalf("NewStorageBackend: %v", err)
	}
	return b
}

// assertTopoValid 校验 order 为合法拓扑序（依赖先行）。
func assertTopoValid(t *testing.T, pkgs map[string]*kernel.PackageManifest, order []string) {
	t.Helper()
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	for name, mf := range pkgs {
		pi, ok := pos[name]
		if !ok {
			t.Fatalf("package %s missing from order", name)
		}
		for _, dep := range mf.Dependencies {
			di, ok := pos[dep]
			if !ok {
				t.Fatalf("dep %s of %s missing from order", dep, name)
			}
			if di >= pi {
				t.Fatalf("dep %s of %s must be installed BEFORE it", dep, name)
			}
		}
	}
}

// TestPackageE2E_InstallQueryRecall：装包 -> 抽包记忆 -> 查询命中（验收 1）。
func TestPackageE2E_InstallQueryRecall(t *testing.T) {
	ctx := context.Background()
	b := newMemBackend(t)
	defer b.Close()

	pkgs := loadManifests(t)
	if len(pkgs) != 8 {
		t.Fatalf("expected 8 manifests, got %d", len(pkgs))
	}

	// 1) 拓扑排序
	order, err := kernel.InstallOrder(pkgs)
	if err != nil {
		t.Fatalf("InstallOrder: %v", err)
	}
	if len(order) != 8 {
		t.Fatalf("expected 8 in order, got %d: %v", len(order), order)
	}
	assertTopoValid(t, pkgs, order)

	// 2) 全链路装包（安装 -> 配置 -> 启用 -> 升级），按拓扑序
	lc := kernel.NewPackageLifecycle(b)
	root := repoRoot(t)
	for _, name := range order {
		manifestPath := filepath.Join(root, "eval", "packages", "manifests", name+".json")
		if err := lc.Install(ctx, manifestPath, "r11b-e2e"); err != nil {
			t.Fatalf("install %s: %v", name, err)
		}
		cfg := pkgs[name].Config
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{}`)
		}
		if err := lc.Configure(ctx, name, cfg, "r11b-e2e"); err != nil {
			t.Fatalf("configure %s: %v", name, err)
		}
		if err := lc.Enable(ctx, name, "r11b-e2e"); err != nil {
			t.Fatalf("enable %s: %v", name, err)
		}
		if err := lc.Upgrade(ctx, name, "1.1.0", "r11b-e2e"); err != nil {
			t.Fatalf("upgrade %s: %v", name, err)
		}
	}
	if got := len(lc.ListPackages()); got != 8 {
		t.Fatalf("expected 8 enabled packages, got %d: %v", got, lc.ListPackages())
	}

	// 3) 抽包记忆：每包 3 条
	total := 0
	for _, name := range order {
		n, err := kernel.ExtractPackageMemory(ctx, b, name)
		if err != nil {
			t.Fatalf("ExtractPackageMemory(%s): %v", name, err)
		}
		if n != 3 {
			t.Fatalf("package %s: expected 3 memories, got %d", name, n)
		}
		total += n
	}
	if total != 24 {
		t.Fatalf("expected 24 total package memories, got %d", total)
	}

	// 4) 查询「这个包是干什么的」命中 registry 包记忆（元数据事实）
	res, err := b.Query(ctx, kernel.QueryParams{
		TenantID: "system",
		Viewer:   "system",
		Text:     "这个包是干什么的 包注册中心",
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	hit := false
	for _, r := range res {
		if strings.Contains(r.Fact, "registry") && strings.Contains(r.Fact, "包注册中心") {
			hit = true
			t.Logf("命中包记忆: %s", r.Fact)
			break
		}
	}
	if !hit {
		t.Fatalf("expected recall to hit registry package memory, got %v", res)
	}
}

// TestPackageE2E_WrongOrder_Error：错误安装顺序 -> 拓扑排序报错提示（验收 2）。
func TestPackageE2E_WrongOrder_Error(t *testing.T) {
	// a 依赖 b，却把 a 排在 b 前（错误顺序）。InstallOrder 内部按依赖 DFS，
	// 合法图总能给出合法拓扑序；这里构造循环依赖验证报错，并演示「依赖未安装即安装」被拦。
	ctx := context.Background()
	b := newMemBackend(t)
	defer b.Close()

	// 循环依赖：webui -> search -> registry -> webui
	cycle := map[string]*kernel.PackageManifest{
		"webui":    {Name: "webui", Version: "1.0.0", Dependencies: []string{"search"}},
		"search":   {Name: "search", Version: "1.0.0", Dependencies: []string{"registry"}},
		"registry": {Name: "registry", Version: "1.0.0", Dependencies: []string{"webui"}},
	}
	_, err := kernel.InstallOrder(cycle)
	if err == nil {
		t.Fatal("expected cycle error for cyclic dependency, got nil")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Fatalf("expected 'cycle detected' in error, got: %v", err)
	}
	t.Logf("循环依赖报错提示: %v", err)

	// 依赖未注册：webui 依赖 ghost（不存在）
	unknown := map[string]*kernel.PackageManifest{
		"webui": {Name: "webui", Version: "1.0.0", Dependencies: []string{"ghost"}},
	}
	_, err = kernel.InstallOrder(unknown)
	if err == nil {
		t.Fatal("expected error for unknown dependency, got nil")
	}
	if !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("expected 'unknown dependency' in error, got: %v", err)
	}
	t.Logf("未知依赖报错提示: %v", err)

	// 依赖未安装即 install 被 PackageLifecycle 拦（模拟错误安装顺序下的操作拦截）
	lc := kernel.NewPackageLifecycle(b)
	root := repoRoot(t)
	manifestPath := filepath.Join(root, "eval", "packages", "manifests", "webui.json")
	// webui 依赖 registry/search/gitea，未先装这三个依赖，Install 必须报缺失依赖
	err = lc.Install(ctx, manifestPath, "r11b-e2e")
	if err == nil {
		t.Fatal("expected install webui to fail (missing deps registry/search/gitea), got nil")
	}
	if !strings.Contains(err.Error(), "missing dependency") {
		t.Fatalf("expected 'missing dependency' in error, got: %v", err)
	}
	t.Logf("依赖未安装即安装被拦: %v", err)

	// 依赖未启用即 enable 被 PackageLifecycle 拦
	// 先按拓扑序装齐 webui 的全部依赖（registry -> gitea -> search），
	// 但只 enable registry，验证 enable webui 因其余依赖未启用被拦。
	for _, dep := range []string{"registry", "gitea", "search"} {
		depPath := filepath.Join(root, "eval", "packages", "manifests", dep+".json")
		if err := lc.Install(ctx, depPath, "r11b-e2e"); err != nil {
			t.Fatalf("install %s: %v", dep, err)
		}
	}
	if err := lc.Enable(ctx, "registry", "r11b-e2e"); err != nil {
		t.Fatalf("enable registry: %v", err)
	}
	// 依赖已装齐，webui 可安装
	if err := lc.Install(ctx, manifestPath, "r11b-e2e"); err != nil {
		t.Fatalf("install webui after deps installed: %v", err)
	}
	// 但 search/gitea 未 enable，enable webui 必须被拦
	if err := lc.Enable(ctx, "webui", "r11b-e2e"); err == nil {
		t.Fatal("expected enable webui to fail (deps search/gitea not enabled), got nil")
	} else {
		t.Logf("依赖未启用被拦: %v", err)
	}
}

// TestPackageE2E_FullLifecycle：全链路 装->配->启->升->卸（验收 B.1 的生命周期部分）。
func TestPackageE2E_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	b := newMemBackend(t)
	defer b.Close()

	pkgs := loadManifests(t)
	order, err := kernel.InstallOrder(pkgs)
	if err != nil {
		t.Fatalf("InstallOrder: %v", err)
	}
	lc := kernel.NewPackageLifecycle(b)
	root := repoRoot(t)

	// 全链路：装->配->启->升，最后全部卸载，验证无残留
	for _, name := range order {
		manifestPath := filepath.Join(root, "eval", "packages", "manifests", name+".json")
		if err := lc.Install(ctx, manifestPath, "r11b-e2e"); err != nil {
			t.Fatalf("install %s: %v", name, err)
		}
		cfg := pkgs[name].Config
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{}`)
		}
		if err := lc.Configure(ctx, name, cfg, "r11b-e2e"); err != nil {
			t.Fatalf("configure %s: %v", name, err)
		}
		if err := lc.Enable(ctx, name, "r11b-e2e"); err != nil {
			t.Fatalf("enable %s: %v", name, err)
		}
		if err := lc.Upgrade(ctx, name, "1.1.0", "r11b-e2e"); err != nil {
			t.Fatalf("upgrade %s: %v", name, err)
		}
	}
	// 卸载顺序应与安装相反（先卸被依赖多的）
	rev := make([]string, len(order))
	for i, n := range order {
		rev[len(order)-1-i] = n
	}
	for _, name := range rev {
		if err := lc.Uninstall(ctx, name, "r11b-e2e"); err != nil {
			t.Fatalf("uninstall %s: %v", name, err)
		}
	}
	if left := lc.ListPackages(); len(left) != 0 {
		t.Fatalf("expected 0 packages after uninstall all, got %v", left)
	}
}
