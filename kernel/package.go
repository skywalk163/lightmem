// 应用包生态（P9）
// 真源：lightmem.md §6.2 应用包生态、docs/光明记忆应用包生态与差异化定位_v1.0.md
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

// PackageManifest 包清单（manifest.yaml 对应结构）
type PackageManifest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Author      string            `json:"author"`
	Trust       PackageTrust      `json:"trust"`
	Entrypoint  string            `json:"entrypoint"`
	Config      json.RawMessage   `json:"config,omitempty"`
	Permissions []string          `json:"permissions,omitempty"`
	MemoryTypes []string          `json:"memory_types,omitempty"`
}

// PackageLifecycle 包生命周期
type PackageLifecycle struct {
	backend StorageBackend
	pkgs    map[string]*PackageManifest
	states  map[string]string
}

func NewPackageLifecycle(backend StorageBackend) *PackageLifecycle {
	return &PackageLifecycle{
		backend: backend,
		pkgs:    map[string]*PackageManifest{},
		states:  map[string]string{},
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
	p.pkgs[mf.Name] = &mf
	p.states[mf.Name] = "installed"
	GetAuditLogger().Log("package_install", mf.Name, "", "", actor, mf.Name)
	log.Printf("[package] %s v%s installed", mf.Name, mf.Version)
	return nil
}

// Configure 配置包
func (p *PackageLifecycle) Configure(ctx context.Context, name string, cfg json.RawMessage, actor string) error {
	mf, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	mf.Config = cfg
	GetAuditLogger().Log("package_configure", name, "", "", actor, string(cfg))
	return nil
}

// Enable 启用包
func (p *PackageLifecycle) Enable(ctx context.Context, name, actor string) error {
	if _, ok := p.pkgs[name]; !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	p.states[name] = "enabled"
	GetAuditLogger().Log("package_enable", name, "", "", actor, "")
	return nil
}

// Disable 禁用包
func (p *PackageLifecycle) Disable(ctx context.Context, name, actor string) error {
	if _, ok := p.pkgs[name]; !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	p.states[name] = "disabled"
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
	mf, ok := p.pkgs[name]
	if !ok {
		return fmt.Errorf("package not found: %s", name)
	}
	delete(p.pkgs, name)
	delete(p.states, name)
	GetAuditLogger().Log("package_uninstall", name, "", "", actor, "")
	return nil
}

// Health 包健康检查
func (p *PackageLifecycle) Health(ctx context.Context, name string) (string, error) {
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
		out = append(out, fmt.Sprintf("%s v%s (%s)", name, mf.Version, p.states[name]))
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
		out = append(out, fmt.Sprintf("%s v%s trust=%s", name, mf.Version, mf.Trust))
	}
	return out
}

// 首批包清单（P9）
var FirstPartyPackages = []string{
	"gitea", "git-connector", "registry", "backup",
	"search", "llm-gateway", "mesh-sync", "webui",
}

// ExtractPackageMemory 从包抽取记忆（P9 包记忆抽取）
func ExtractPackageMemory(ctx context.Context, backend StorageBackend, pkgName string) (int, error) {
	// 占位：实际实现需解析包内容并提取记忆
	log.Printf("[package] extract memory from %s (stub)", pkgName)
	return 0, nil
}

// PackageDir 包目录路径
func PackageDir(base string) string {
	return filepath.Join(base, "packages")
}

// PackageManifestPath 包清单路径
func PackageManifestPath(base, pkgName string) string {
	return filepath.Join(PackageDir(base), pkgName, "manifest.json")
}

// TimeNowUTC 当前时间（UTC）
func TimeNowUTC() time.Time {
	return time.Now().UTC()
}
