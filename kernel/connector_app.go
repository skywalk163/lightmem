// P5 应用类连接器（S3）
// 真源：lightmem.md §4.6 外部连接器、§5.4 存储后端可插拔；
// profiles/lims.profile.schema.json features.external_connectors。
//
// R16 S3 红线：
//   - 只增不改：只新增 connector_app.go，不修改既有 connector.go（DB/Doc 行为冻结）。
//   - 骨架优先：AppConnector 只做确定性、无网络依赖的骨架实现。
//     GitConnector 通过扫描本地仓库元数据文件（README/LICENSE/CHANGELOG + .git/config）
//     产出 Memory，不在 Sync 中调外部 git 命令或网络，确保「可复现」。
//   - 只读：不向外部写。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AppConnector 应用类连接器（骨架接口）。
// 与既有 Connector 接口语义一致：Name / Sync / Health / Close。
// 区别：Sync 只做确定性、一次性或轮询式的应用数据入记忆，不阻塞主线程。
type AppConnector struct {
	name    string
	backend StorageBackend
	cfg     *AppConfig
}

// AppConfig 应用类连接器通用配置。
// 仅新增，不改既有 DBConfig / DocConfig 字段。
type AppConfig struct {
	Kind       string   `json:"kind"`                 // git / gitea / jira / notion / ...
	Root       string   `json:"root"`                 // 数据根路径（git: 本地仓库路径）
	Extensions []string `json:"extensions,omitempty"` // 关注的文件扩展名（git: 文档类）
	PollSec    int      `json:"poll_sec"`
	TenantID   string   `json:"tenant_id"`
	UserID     string   `json:"user_id"`
	MaxFiles   int      `json:"max_files,omitempty"` // 单次扫描上限（骨架防爆）
}

// NewAppConnector 构造应用类连接器。默认关闭，需显式指定 kind。
func NewAppConnector(name string, backend StorageBackend) *AppConnector {
	return &AppConnector{
		name:    name,
		backend: backend,
		cfg: &AppConfig{
			Kind:     "",
			PollSec:  60,
			TenantID: "connector",
			UserID:   "system",
			MaxFiles: 100,
		},
	}
}

func (c *AppConnector) Name() string { return c.name }

// Health 校验配置最小字段（不触碰外部资源）。
func (c *AppConnector) Health(ctx context.Context) error {
	if c.cfg == nil || c.cfg.Kind == "" {
		return fmt.Errorf("app connector %s: kind not configured", c.name)
	}
	switch c.cfg.Kind {
	case "git", "gitea":
		if c.cfg.Root == "" {
			return fmt.Errorf("app connector %s: root (repo path) not configured", c.name)
		}
		if _, err := os.Stat(c.cfg.Root); err != nil {
			return fmt.Errorf("app connector %s: root stat: %w", c.name, err)
		}
	default:
		return fmt.Errorf("app connector %s: unsupported kind %q (want git|gitea)", c.name, c.cfg.Kind)
	}
	return nil
}

func (c *AppConnector) Close() error { return nil }

// Sync 按 poll_sec 轮询扫描应用数据（git：扫描文档元数据）。
// cfg 传入时解析并更新配置；不阻塞，遇错继续（骨架容错）。
func (c *AppConnector) Sync(ctx context.Context, cfg json.RawMessage) error {
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, c.cfg); err != nil {
			return fmt.Errorf("app connector %s: parse cfg: %w", c.name, err)
		}
	}
	if c.cfg.Kind == "" {
		return fmt.Errorf("app connector %s: kind required", c.name)
	}
	// 骨架：不做长轮询，先执行一轮扫描（R16 S3 确定性）。
	if err := c.scanOnce(ctx); err != nil {
		log.Printf("[app] %s scanOnce: %v", c.name, err)
		return err
	}
	return nil
}

// scanOnce 单次扫描（骨架：git 走本地元数据）。
func (c *AppConnector) scanOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch c.cfg.Kind {
	case "git", "gitea":
		return c.scanGit(ctx)
	default:
		return fmt.Errorf("unsupported kind %q", c.cfg.Kind)
	}
}

// gitMetadata 单条仓库元数据快照。
type gitMetadata struct {
	Repo      string `json:"repo"`
	Tag       string `json:"tag,omitempty"`
	Branch    string `json:"branch,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	RelPath   string `json:"rel_path"`
	Size      int64  `json:"size"`
	ModTime   int64  `json:"mod_time"` // Unix 秒，UTC
}

// scanGit 扫描本地 git 仓库根目录：读取元数据文件（README/LICENSE/CHANGELOG + .git/config）。
// 只读、确定性、无网络；产出 KindFact 记忆，idempotency_key = app:<name>:<relpath>:<modtime>。
func (c *AppConnector) scanGit(ctx context.Context) error {
	root := filepath.Clean(c.cfg.Root)
	// 1. 从 .git/config 抽 remote（若存在）
	remoteURL := c.gitRemoteURL(root)
	// 2. 扫描元数据文件
	var files []gitMetadata
	limit := c.cfg.MaxFiles
	if limit <= 0 {
		limit = 100
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 忽略权限/IO 错误，骨架容错
		}
		if d.IsDir() {
			// 跳过 .git 内部目录（保留 .git 本身以便读 config）
			if p != root && strings.HasPrefix(filepath.Base(p), ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(p)
		if len(ext) > 0 {
			ext = ext[1:]
		}
		if !c.extMatch(ext) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if len(files) >= limit {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, gitMetadata{
			Repo:      filepath.Base(root),
			RemoteURL: remoteURL,
			RelPath:   filepath.ToSlash(rel),
			Size:      info.Size(),
			ModTime:   info.ModTime().Unix(),
		})
		return nil
	})

	count := 0
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		fact := c.gitFact(f)
		key := fmt.Sprintf("app:%s:%s:%d", c.name, f.RelPath, f.ModTime)
		mid := newMemoryID()
		// 走统一幂等入口：后端支持 IdempotencyLookup 时先查已有记录（§4.1）。
		if lk, ok := c.backend.(IdempotencyLookup); ok {
			if ex, err := lk.FindByIdempotencyKey(ctx, c.cfg.TenantID, c.cfg.UserID, key); err == nil && ex != nil {
				mid = ex.MemoryID
			}
		}
		m := &Memory{
			MemoryID:       mid,
			IdempotencyKey: key,
			TenantID:       c.cfg.TenantID,
			UserID:         c.cfg.UserID,
			MemoryKind:     KindFact,
			Fact:           fact,
			Source:         c.name,
			RecordedAt:     time.Now().UTC(),
			Confidence:     0.7,
			Version:        1,
			Layer:          LayerL1,
			Visibility:     VisTenant,
		}
		if err := c.backend.Upsert(ctx, m); err != nil {
			log.Printf("[app] %s upsert %s: %v", c.name, f.RelPath, err)
			continue
		}
		count++
	}
	if count > 0 {
		log.Printf("[app] %s synced %d git metadata files", c.name, count)
	}
	return nil
}

// extMatch 是否属于本连接器关注的文件扩展名。
// 空 Extensions 时只关注 markdown / yaml / json / txt（文档类元数据）。
func (c *AppConnector) extMatch(ext string) bool {
	if len(c.cfg.Extensions) > 0 {
		for _, e := range c.cfg.Extensions {
			if strings.EqualFold(e, ext) {
				return true
			}
		}
		return false
	}
	switch strings.ToLower(ext) {
	case "md", "markdown", "yaml", "yml", "json", "txt", "toml":
		return true
	}
	return false
}

// gitFact 由元数据生成可读事实字符串（确定性）。
func (c *AppConnector) gitFact(f gitMetadata) string {
	parts := []string{
		fmt.Sprintf("仓库 %s 文件 %s", f.Repo, f.RelPath),
		fmt.Sprintf("大小 %d 字节", f.Size),
		fmt.Sprintf("修改时间 %s", time.Unix(f.ModTime, 0).UTC().Format("2006-01-02T15:04:05Z")),
	}
	if f.Branch != "" {
		parts = append(parts, "分支 "+f.Branch)
	}
	if f.RemoteURL != "" {
		parts = append(parts, "远端 "+f.RemoteURL)
	}
	return strings.Join(parts, "；")
}

// gitRemoteURL 从 .git/config 抽取第一个 remote url（确定性，仅读文件）。
func (c *AppConnector) gitRemoteURL(root string) string {
	cfgPath := filepath.Join(root, ".git", "config")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(strings.ToLower(line), "url = "); ok {
			return strings.TrimSpace(line[len(line)-len(v):])
		}
	}
	return ""
}
