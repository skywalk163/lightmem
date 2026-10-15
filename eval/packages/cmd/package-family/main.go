// R11-B 首批 8 包 Family 档装配助手（package-family）
// 用途：install_family.sh 的 Go 驱动。从 eval/packages/manifests/*.json 装载 8 包 manifest，
//
//	用 kernel.InstallOrder 计算拓扑安装顺序，用 kernel.PackageLifecycle 跑通
//	安装 -> 配置 -> 启用 -> 升级 -> 卸载全链路，并用 kernel.ExtractPackageMemory 抽取包记忆。
//
// 红线遵守：
//   - 内存 SQLite 后端（:memory:），不依赖真实 PG / 网络 / 外部服务；
//   - 不调 LLM；全部确定性；
//   - 只调用 kernel 既有公共 API（NewPackageLifecycle / InstallOrder / ExtractPackageMemory），不改 kernel 语义。
//
// 用法（由 install_family.sh 调用）：
//
//	go run ./eval/packages/cmd/package-family order      # 打印拓扑安装顺序 + 循环依赖检测
//	go run ./eval/packages/cmd/package-family lifecycle  # 全链路：装->配->启->升->卸
//	go run ./eval/packages/cmd/package-family memory     # 抽取 8 包包记忆并验证可召回
//	go run ./eval/packages/cmd/package-family all        # order + lifecycle + memory
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lightmem/kernel"
)

const manifestsDir = "eval/packages/manifests"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: package-family {order|lifecycle|memory|all}")
		os.Exit(2)
	}
	sub := os.Args[1]

	pkgs, err := loadManifests()
	if err != nil {
		fatal(err)
	}

	switch sub {
	case "order":
		doOrder(pkgs)
	case "lifecycle":
		doLifecycle(pkgs)
	case "memory":
		doMemory(pkgs)
	case "all":
		doOrder(pkgs)
		doLifecycle(pkgs)
		doMemory(pkgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", sub)
		os.Exit(2)
	}
}

// loadManifests 装载 manifests 目录下全部 manifest.json。
func loadManifests() (map[string]*kernel.PackageManifest, error) {
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return nil, fmt.Errorf("read manifests dir: %w", err)
	}
	out := map[string]*kernel.PackageManifest{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var mf kernel.PackageManifest
		if err := json.Unmarshal(data, &mf); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", e.Name(), err)
		}
		if err := kernel.ValidateManifest(&mf); err != nil {
			return nil, fmt.Errorf("validate %s: %w", e.Name(), err)
		}
		out[mf.Name] = &mf
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no manifests loaded from %s", manifestsDir)
	}
	return out, nil
}

func doOrder(pkgs map[string]*kernel.PackageManifest) {
	fmt.Printf("=== 拓扑安装顺序（%d 包）===\n", len(pkgs))
	order, err := kernel.InstallOrder(pkgs)
	if err != nil {
		fmt.Printf("InstallOrder ERROR: %v\n", err)
		os.Exit(1)
	}
	for i, name := range order {
		fmt.Printf("  %d. %s\n", i+1, name)
	}
}

func doLifecycle(pkgs map[string]*kernel.PackageManifest) {
	ctx := context.Background()
	b, err := kernel.NewStorageBackend(ctx, &kernel.Profile{
		Profile:        kernel.TierSolo,
		StorageBackend: kernel.BackendSQLite,
		Storage:        &kernel.StorageConfig{SQLite: &kernel.SQLiteConfig{Path: ":memory:"}},
	})
	if err != nil {
		fatal(fmt.Errorf("new backend: %w", err))
	}
	defer b.Close()

	order, err := kernel.InstallOrder(pkgs)
	if err != nil {
		fatal(err)
	}
	lc := kernel.NewPackageLifecycle(b)

	fmt.Printf("=== 全链路生命周期（%d 包，按拓扑序）===\n", len(order))
	for _, name := range order {
		mf := pkgs[name]
		manifestPath := filepath.Join(manifestsDir, name+".json")

		// 1) 安装
		if err := lc.Install(ctx, manifestPath, "r11b"); err != nil {
			fatal(fmt.Errorf("install %s: %w", name, err))
		}
		// 2) 配置
		cfg := mf.Config
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{}`)
		}
		if err := lc.Configure(ctx, name, cfg, "r11b"); err != nil {
			fatal(fmt.Errorf("configure %s: %w", name, err))
		}
		// 3) 启用
		if err := lc.Enable(ctx, name, "r11b"); err != nil {
			fatal(fmt.Errorf("enable %s: %w", name, err))
		}
		// 4) 升级
		if err := lc.Upgrade(ctx, name, "1.1.0", "r11b"); err != nil {
			fatal(fmt.Errorf("upgrade %s: %w", name, err))
		}
		// 5) 卸载
		if err := lc.Uninstall(ctx, name, "r11b"); err != nil {
			fatal(fmt.Errorf("uninstall %s: %w", name, err))
		}
		fmt.Printf("  %s: install -> configure -> enable -> upgrade -> uninstall  OK\n", name)
	}

	// 校验：卸载后应无残留
	left := lc.ListPackages()
	if len(left) != 0 {
		fatal(fmt.Errorf("expected 0 packages after full lifecycle, got %v", left))
	}
	fmt.Println("全链路通过：8 包 装->配->启->升->卸 全部成功，无残留。")
}

func doMemory(pkgs map[string]*kernel.PackageManifest) {
	ctx := context.Background()
	b, err := kernel.NewStorageBackend(ctx, &kernel.Profile{
		Profile:        kernel.TierSolo,
		StorageBackend: kernel.BackendSQLite,
		Storage:        &kernel.StorageConfig{SQLite: &kernel.SQLiteConfig{Path: ":memory:"}},
	})
	if err != nil {
		fatal(fmt.Errorf("new backend: %w", err))
	}
	defer b.Close()

	fmt.Println("=== 包记忆抽取（每包 3 条：元数据/配置/变更记录）===")
	names := make([]string, 0, len(pkgs))
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names)

	total := 0
	for _, name := range names {
		n, err := kernel.ExtractPackageMemoryFromManifest(ctx, b, pkgs[name])
		if err != nil {
			fatal(fmt.Errorf("extract %s: %w", name, err))
		}
		total += n
		fmt.Printf("  %s: +%d\n", name, n)
	}
	fmt.Printf("抽取合计：%d 条\n", total)

	// 验证召回：查询「这个包是干什么的」应命中 registry 包元数据事实
	res, err := b.Query(ctx, kernel.QueryParams{
		TenantID: "system",
		Viewer:   "system",
		Text:     "这个包是干什么的 包注册中心",
		TopK:     5,
	})
	if err != nil {
		fatal(fmt.Errorf("query: %w", err))
	}
	if len(res) == 0 {
		fatal(fmt.Errorf("expected recall hit for package memory, got none"))
	}
	fmt.Printf("召回验证：查询「这个包是干什么的」Top1 fact = %s\n", res[0].Fact)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
