// 包安装顺序（拓扑排序 + 循环依赖检测）单测（R11-B 任务书 B.3）。
// 全部确定性，不依赖 map 迭代顺序（实现内部对包名排序）。
package kernel

import (
	"reflect"
	"strings"
	"testing"
)

// mkPkg 构造一个只含 Name/Dependencies 的最小 manifest，便于测试。
func mkPkg(name string, deps ...string) *PackageManifest {
	return &PackageManifest{Name: name, Version: "1.0.0", Dependencies: deps}
}

// assertValidTopo 校验 order 是合法拓扑序：任意包的所有依赖都排在其之前。
func assertValidTopo(t *testing.T, pkgs map[string]*PackageManifest, order []string) {
	t.Helper()
	pos := make(map[string]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	for name, mf := range pkgs {
		pi, ok := pos[name]
		if !ok {
			t.Fatalf("package %s missing from order %v", name, order)
		}
		for _, dep := range mf.Dependencies {
			di, ok := pos[dep]
			if !ok {
				t.Fatalf("dependency %s of %s missing from order %v", dep, name, order)
			}
			if di >= pi {
				t.Fatalf("dependency %s of %s must come BEFORE it: order=%v", dep, name, order)
			}
		}
	}
}

func TestInstallOrder_ValidDAG(t *testing.T) {
	pkgs := map[string]*PackageManifest{
		"a": mkPkg("a", "b", "c"),
		"b": mkPkg("b", "c"),
		"c": mkPkg("c"),
		"d": mkPkg("d"),
	}
	order, err := InstallOrder(pkgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("want 4 packages in order, got %v", order)
	}
	assertValidTopo(t, pkgs, order)
}

func TestInstallOrder_SimpleChain(t *testing.T) {
	pkgs := map[string]*PackageManifest{
		"webui":    mkPkg("webui", "registry"),
		"registry": mkPkg("registry", "gitea"),
		"gitea":    mkPkg("gitea"),
	}
	order, err := InstallOrder(pkgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertValidTopo(t, pkgs, order)
	// 链式依赖：gitea -> registry -> webui
	want := []string{"gitea", "registry", "webui"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order mismatch: want %v, got %v", want, order)
	}
}

func TestInstallOrder_CycleDetected(t *testing.T) {
	pkgs := map[string]*PackageManifest{
		"a": mkPkg("a", "b"),
		"b": mkPkg("b", "c"),
		"c": mkPkg("c", "a"), // a->b->c->a 形成回路
	}
	_, err := InstallOrder(pkgs)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Fatalf("error should mention cycle detected, got: %v", err)
	}
	t.Logf("cycle error: %v", err)
}

func TestInstallOrder_SelfDependency(t *testing.T) {
	pkgs := map[string]*PackageManifest{
		"a": mkPkg("a", "a"),
	}
	_, err := InstallOrder(pkgs)
	if err == nil {
		t.Fatal("expected self-dependency error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Fatalf("error should mention cycle detected, got: %v", err)
	}
}

func TestInstallOrder_UnknownDependency(t *testing.T) {
	pkgs := map[string]*PackageManifest{
		"a": mkPkg("a", "ghost"),
	}
	_, err := InstallOrder(pkgs)
	if err == nil {
		t.Fatal("expected unknown-dependency error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("error should mention unknown dependency, got: %v", err)
	}
}

func TestInstallOrder_UnknownPackage(t *testing.T) {
	// 空 map：直接返回 nil
	order, err := InstallOrder(nil)
	if err != nil {
		t.Fatalf("empty map should be valid, got err %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("empty map should return empty order, got %v", order)
	}
}

func TestInstallOrder_Deterministic(t *testing.T) {
	// 同图多次调用结果必须一致（不依赖 map 迭代顺序）
	pkgs := map[string]*PackageManifest{
		"z": mkPkg("z", "x", "y"),
		"x": mkPkg("x"),
		"y": mkPkg("y", "x"),
	}
	first, err := InstallOrder(pkgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := InstallOrder(pkgs)
		if err != nil {
			t.Fatalf("unexpected error on iter %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("nondeterministic: first=%v again=%v", first, again)
		}
	}
}
