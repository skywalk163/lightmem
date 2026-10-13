// 应用包生态（P9 完整实现）
// 真源：lightmem.md §6.2 应用包生态、docs/光明记忆应用包生态与差异化定位_v1.0.md
// 光明语言：用声明式 manifest 表达包能力
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// PackageTrust 包信任分级
type PackageTrust string

const (
	TrustOfficial  PackageTrust = "official"
	TrustVerified  PackageTrust = "verified"
	TrustCommunity PackageTrust = "community"
)

// PackageType 包类型
type PackageType string

const (
	PkgService   PackageType = "service"
	PkgConnector PackageType = "connector"
	PkgSkill     PackageType = "skill"
	PkgPolicy    PackageType = "policy"
	PkgPack      PackageType = "pack"
	PkgScheduler PackageType = "scheduler"
)

// PackageManifest 包清单（manifest.json 对应结构）
type PackageManifest struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Description  string          `json:"description"`
	Author       string          `json:"author"`
	Trust        PackageTrust    `json:"trust"`
	Type         PackageType     `json:"type"`
	Entrypoint   string          `json:"entrypoint"`
	Config       json.RawMessage `json:"config,omitempty"`
	Permissions  []string        `json:"permissions,omitempty"`
	MemoryTypes  []string        `json:"memory_types,omitempty"`
	Dependencies []string        `json:"dependencies,omitempty"`
}

// PackageState 包状态
type PackageState string

const (
	PkgInstalled PackageState = "installed"
	PkgEnabled   PackageState = "enabled"
	PkgDisabled  PackageState = "disabled"
	PkgFailed    PackageState = "failed"
)

// PackageLifecycle 包生命周期（P9 完整实现）
type PackageLifecycle struct {
	backend StorageBackend
	pkgs    map[string]*PackageManifest
	states  map[string]PackageState
	configs map[string]json.RawMessage
}

func NewPackageLifecycle(backend StorageBackend) *PackageLifecycle {
	return &PackageLifecycle{
		backend: backend,
		pkgs:    map[string]*PackageManifest{},
		states:  map[string]PackageState{},
		configs: map[string]json.RawMessage{},
	}
}

// Install 安装包
func (p *PackageLifecycle) Install(ctx context.Context, manifestPath, actor string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var mf PackageManifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return err
	}
	if mf.Name == "" {
		return fmt.Errorf("package name required")
	}
	if mf.Version == "" {
		mf.Version = "1.0.0"
	}

	// 检查依赖
	for _, dep := range mf.Dependencies {
		if _, ok := p.pkgs[dep]; !ok {
			return fmt.Errorf("missing dependency: %s", dep)
		}
	}

	p.pkgs[mf.Name] = &mf
	p.states[mf.Name] = PkgInstalled
	GetAuditLogger().Log("package_install", mf.Name, "", "", actor, mf.Name)
	log.Printf("[package] %s v%s installed (type=%s trust=%s)", mf.Name, mf.Version, mf.Type, mf.Trust)
	return nil
}

// Configure 配置包
func (p *PackageLifecycle) Configure(ctx context.Context, name string, cfg json.RawMessage, actor string) error {
	_, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	p.configs[name] = cfg
	GetAuditLogger().Log("package_configure", name, "", "", actor, string(cfg))
	return nil
}

// Enable 启用包
func (p *PackageLifecycle) Enable(ctx context.Context, name, actor string) error {
	mf, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	if p.states[name] == PkgFailed {
		return fmt.Errorf("package in failed state, need to reinstall")
	}

	// 检查依赖是否启用
	for _, dep := range mf.Dependencies {
		if p.states[dep] != PkgEnabled {
			return fmt.Errorf("dependency not enabled: %s", dep)
		}
	}

	p.states[name] = PkgEnabled
	GetAuditLogger().Log("package_enable", name, "", "", actor, "")
	log.Printf("[package] %s enabled", name)
	return nil
}

// Disable 禁用包
func (p *PackageLifecycle) Disable(ctx context.Context, name, actor string) error {
	if _, ok := p.pkgs[name]; !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	p.states[name] = PkgDisabled
	GetAuditLogger().Log("package_disable", name, "", "", actor, "")
	return nil
}

// Upgrade 升级包
func (p *PackageLifecycle) Upgrade(ctx context.Context, name, newVersion, actor string) error {
	mf, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	oldVersion := mf.Version
	mf.Version = newVersion
	GetAuditLogger().Log("package_upgrade", name, "", "", actor, fmt.Sprintf("%s->%s", oldVersion, newVersion))
	return nil
}

// Uninstall 卸载包
func (p *PackageLifecycle) Uninstall(ctx context.Context, name, actor string) error {
	_, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	// 检查是否被其他包依赖
	for n, m := range p.pkgs {
		if n != name {
			for _, dep := range m.Dependencies {
				if dep == name {
					return fmt.Errorf("package %s depends on %s", n, name)
				}
			}
		}
	}
	delete(p.pkgs, name)
	delete(p.states, name)
	delete(p.configs, name)
	GetAuditLogger().Log("package_uninstall", name, "", "", actor, "")
	return nil
}

// Health 包健康检查
func (p *PackageLifecycle) Health(ctx context.Context, name string) (PackageState, error) {
	state, ok := p.states[name]
	if !ok {
		return "", fmt.Errorf("package not found: %s", name)
	}
	return state, nil
}

// ListPackages 列出已安装包
func (p *PackageLifecycle) ListPackages() []string {
	var out []string
	for name, mf := range p.pkgs {
		out = append(out, fmt.Sprintf("%s v%s (%s) %s", name, mf.Version, p.states[name], mf.Type))
	}
	return out
}

// PackageRegistry 包目录
type PackageRegistry struct {
	pkgs map[string]*PackageManifest
}

func NewPackageRegistry() *PackageRegistry {
	return &PackageRegistry{pkgs: map[string]*PackageManifest{}}
}

func (r *PackageRegistry) Register(mf *PackageManifest) {
	if mf != nil {
		r.pkgs[mf.Name] = mf
	}
}

func (r *PackageRegistry) Get(name string) *PackageManifest {
	return r.pkgs[name]
}

func (r *PackageRegistry) List() []string {
	var out []string
	for name, mf := range r.pkgs {
		out = append(out, fmt.Sprintf("%s v%s trust=%s type=%s", name, mf.Version, mf.Trust, mf.Type))
	}
	return out
}

// 首批包清单（P9 完整实现）
var FirstPartyPackages = map[string]PackageManifest{
	"gitea": {
		Name:        "gitea",
		Version:     "1.0.0",
		Description: "Gitea 代码托管连接器",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		Permissions: []string{"read:repo", "write:repo"},
		MemoryTypes: []string{string(KindFact), string(KindProcedure)},
	},
	"git-connector": {
		Name:        "git-connector",
		Version:     "1.0.0",
		Description: "Git 仓库增量同步",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgConnector,
		Permissions: []string{"read:repo"},
		MemoryTypes: []string{string(KindFact)},
	},
	"registry": {
		Name:        "registry",
		Version:     "1.0.0",
		Description: "包注册中心",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
	"backup": {
		Name:        "backup",
		Version:     "1.0.0",
		Description: "ZFS 快照备份",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
	"search": {
		Name:        "search",
		Version:     "1.0.0",
		Description: "BM25 全文检索",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
	"llm-gateway": {
		Name:        "llm-gateway",
		Version:     "1.0.0",
		Description: "LLM 网关（token 计费与限流）",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
	"mesh-sync": {
		Name:        "mesh-sync",
		Version:     "1.0.0",
		Description: "集合体节点同步",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
	"webui": {
		Name:        "webui",
		Version:     "1.0.0",
		Description: "Web 管理界面",
		Author:      "lightmem",
		Trust:       TrustOfficial,
		Type:        PkgService,
		MemoryTypes: []string{string(KindFact)},
	},
}

// ExtractPackageMemory 从包抽取记忆（P9 完整实现）
func ExtractPackageMemory(ctx context.Context, backend StorageBackend, pkgName string) (int, error) {
	mf, ok := FirstPartyPackages[pkgName]
	if !ok {
		return 0, fmt.Errorf("unknown package: %s", pkgName)
	}

	// 根据包类型提取记忆
	extracted := 0
	switch mf.Type {
	case PkgConnector:
		// 连接器包：提取连接配置和同步状态
		m := &Memory{
			MemoryID:       newMemoryID(),
			IdempotencyKey: "pkg:" + pkgName + ":config",
			TenantID:       "system",
			UserID:         "system",
			MemoryKind:     KindFact,
			Fact:           fmt.Sprintf("包 %s 配置: %s", pkgName, mf.Description),
			Source:         "package:" + pkgName,
			RecordedAt:     time.Now().UTC(),
			Confidence:     0.8,
			Version:        1,
			Layer:          LayerL2,
		}
		if err := backend.Upsert(ctx, m); err != nil {
			return extracted, err
		}
		extracted++
	case PkgService:
		// 服务包：提取服务状态和配置
		m := &Memory{
			MemoryID:       newMemoryID(),
			IdempotencyKey: "pkg:" + pkgName + ":status",
			TenantID:       "system",
			UserID:         "system",
			MemoryKind:     KindFact,
			Fact:           fmt.Sprintf("包 %s 服务状态: 已安装 v%s", pkgName, mf.Version),
			Source:         "package:" + pkgName,
			RecordedAt:     time.Now().UTC(),
			Confidence:     0.9,
			Version:        1,
			Layer:          LayerL2,
		}
		if err := backend.Upsert(ctx, m); err != nil {
			return extracted, err
		}
		extracted++
	}

	log.Printf("[package] extracted %d memories from %s", extracted, pkgName)
	return extracted, nil
}

// PackageDir 包目录路径
func PackageDir(base string) string {
	return filepath.Join(base, "packages")
}

// PackageManifestPath 包清单路径
func PackageManifestPath(base, pkgName string) string {
	return filepath.Join(PackageDir(base), pkgName, "manifest.json")
}

// ValidateManifest 验证包清单（光明语言：声明式校验）
func ValidateManifest(mf *PackageManifest) error {
	if mf.Name == "" {
		return fmt.Errorf("name required")
	}
	if mf.Version == "" {
		return fmt.Errorf("version required")
	}
	if mf.Trust == "" {
		mf.Trust = TrustCommunity
	}
	if mf.Type == "" {
		mf.Type = PkgService
	}
	// 名称格式校验：只允许小写字母、数字、连字符
	for _, r := range mf.Name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return fmt.Errorf("invalid name: %s (only lowercase, digits, hyphen)", mf.Name)
		}
	}
	return nil
}

// ScanPackageDir 扫描包目录
func ScanPackageDir(base string) ([]string, error) {
	dir := PackageDir(base)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, e := range entries {
		if e.IsDir() {
			manifestPath := filepath.Join(dir, e.Name(), "manifest.json")
			if _, err := os.Stat(manifestPath); err == nil {
				pkgs = append(pkgs, e.Name())
			}
		}
	}
	return pkgs, nil
}

// LoadPackageManifest 加载包清单
func LoadPackageManifest(base, pkgName string) (*PackageManifest, error) {
	path := PackageManifestPath(base, pkgName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mf PackageManifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, err
	}
	if err := ValidateManifest(&mf); err != nil {
		return nil, err
	}
	return &mf, nil
}

// 包信任分级检查
func (t PackageTrust) Level() int {
	switch t {
	case TrustOfficial:
		return 3
	case TrustVerified:
		return 2
	case TrustCommunity:
		return 1
	default:
		return 0
	}
}

// CanAccess 检查包是否有指定权限
func (mf *PackageManifest) CanAccess(perm string) bool {
	for _, p := range mf.Permissions {
		if p == perm || p == "*" {
			return true
		}
	}
	return false
}

// 包依赖解析
func ResolveDependencies(pkgs map[string]*PackageManifest) (map[string][]string, error) {
	deps := map[string][]string{}
	for name, mf := range pkgs {
		for _, dep := range mf.Dependencies {
			if _, ok := pkgs[dep]; !ok {
				return nil, fmt.Errorf("package %s depends on unknown package %s", name, dep)
			}
			deps[name] = append(deps[name], dep)
		}
	}
	return deps, nil
}

// 字符串辅助
func hasString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// 包安装顺序（拓扑排序）
func InstallOrder(pkgs map[string]*PackageManifest) ([]string, error) {
	visited := map[string]bool{}
	order := []string{}

	var dfs func(string) error
	dfs = func(name string) error {
		if visited[name] {
			return nil
		}
		visited[name] = true
		mf, ok := pkgs[name]
		if !ok {
			return fmt.Errorf("unknown package: %s", name)
		}
		for _, dep := range mf.Dependencies {
			if err := dfs(dep); err != nil {
				return err
			}
		}
		order = append(order, name)
		return nil
	}

	for name := range pkgs {
		if err := dfs(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}
