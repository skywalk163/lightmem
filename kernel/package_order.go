// 包安装顺序（拓扑排序 + 循环依赖检测）
// 真源：lightmem.md §6.2 应用包生态；R11-B 任务书 B.3
// 光明语言：以声明式依赖图（PackageManifest.Dependencies）驱动安装次序。
//
// 设计要点（相对 package.go 初版的无环 DFS）：
//   - 采用三色标记 DFS（WHITE=未访问 / GRAY=递归栈中 / BLACK=已完成），
//     命中 GRAY 即检测到回路，返回包含回路路径的错误（便于 E2E "错误安装顺序报错提示"）。
//   - 输出拓扑序：依赖在前、被依赖在后，可直接按序安装。
//   - 未知依赖（引用未注册包）同样报错，避免静默漏装。
//   - 纯新增，不改动 PackageManifest / PackageLifecycle 既有语义。
package kernel

import (
	"fmt"
	"sort"
	"strings"
)

// color 三色标记
type color int

const (
	colorWhite color = iota // 未访问
	colorGray               // 递归栈中（正在处理）
	colorBlack              // 处理完成
)

// InstallOrder 根据包依赖图输出拓扑安装顺序。
//   - pkgs: name -> *PackageManifest；每个 manifest 的 Dependencies 是它依赖的包名。
//   - 返回的 order 中，任意包的依赖都排在它之前。
//   - 依赖缺失或存在循环依赖时返回 error，error 信息包含具体出错依赖/回路路径。
//
// 语义与 package.go 初版一致（依赖先行、DFS 拓扑序），仅新增循环依赖与未知依赖的显式报错。
func InstallOrder(pkgs map[string]*PackageManifest) ([]string, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}

	colorOf := make(map[string]color, len(pkgs))
	// 递归栈：记录当前 DFS 路径上的节点名，用于还原回路路径
	stack := make([]string, 0, len(pkgs))
	onStack := make(map[string]bool, len(pkgs))
	order := make([]string, 0, len(pkgs))

	// 确定性遍历顺序：对包名排序，避免 map 迭代顺序导致结果不稳定
	names := sortedKeys(pkgs)

	var dfs func(name string) error
	dfs = func(name string) error {
		switch colorOf[name] {
		case colorGray:
			// 命中递归栈 = 回路：从栈里截取回路路径
			return cycleError(name, stack, onStack)
		case colorBlack:
			return nil
		}

		mf, ok := pkgs[name]
		if !ok {
			return fmt.Errorf("unknown package: %s", name)
		}

		colorOf[name] = colorGray
		stack = append(stack, name)
		onStack[name] = true

		deps := append([]string(nil), mf.Dependencies...)
		// 依赖自身：直接判为自回路
		for _, dep := range deps {
			if dep == name {
				colorOf[name] = colorBlack
				delete(onStack, name)
				stack = stack[:len(stack)-1]
				return fmt.Errorf("cycle detected: package %s depends on itself", name)
			}
			if _, ok := pkgs[dep]; !ok {
				colorOf[name] = colorBlack
				delete(onStack, name)
				stack = stack[:len(stack)-1]
				return fmt.Errorf("unknown dependency: package %s depends on unregistered package %s", name, dep)
			}
			if err := dfs(dep); err != nil {
				colorOf[name] = colorBlack
				delete(onStack, name)
				stack = stack[:len(stack)-1]
				return err
			}
		}

		colorOf[name] = colorBlack
		delete(onStack, name)
		stack = stack[:len(stack)-1]
		order = append(order, name)
		return nil
	}

	for _, name := range names {
		if err := dfs(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// cycleError 从递归栈中截取从 name 到栈顶的回路路径，并构造报错。
// 调用方保证 name 在当前 stack 内（即 onStack[name]==true）。
func cycleError(name string, stack []string, onStack map[string]bool) error {
	idx := -1
	for i, s := range stack {
		if s == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 理论上不可达：GRAY 必然在栈上。退化为通用回路报错。
		return fmt.Errorf("cycle detected: package %s (self or transitive)", name)
	}
	// 回路路径 = stack[idx:] + name（闭环）
	cycle := append(append([]string(nil), stack[idx:]...), name)
	return fmt.Errorf("cycle detected: %s", strings.Join(cycle, " -> "))
}

// sortedKeys 返回 map 的所有 key，按字典序排序，保证拓扑序确定性。
func sortedKeys(pkgs map[string]*PackageManifest) []string {
	keys := make([]string, 0, len(pkgs))
	for k := range pkgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}