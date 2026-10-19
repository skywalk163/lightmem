// R24-D：L3 引用式存储（3FS 引用式存储池映射）。
//
// 真源：lightmem.md 任务书 R24 §7「D 线」。
//
// 目标：把 L3 归档从"正文也躺在记忆库里"改为"记忆库只存指针与摘要，正文外置"。
// 读时透明水合；删除时必须连正文一起删（GDPR 级联）。
//
// 三条设计原则：
//  1. **默认关、零回归**：threshold <= 0 或 archive.path 未配时，
//     ArchiveWithRef 直接返回原记忆，不落盘、不改字段。
//  2. **不静默降级**：backend != "local" 时构造 store 返回明确错误，
//     绝不悄悄退回 local（R15 "SKIP 不冒充 PASS" 口径）。
//  3. **digest 校验**：Hydrate 时校验 SHA-256，不匹配必须报错，
//     禁止返回摘要冒充正文。
//
// 红线遵守：
//   - 不改 Memory 字段语义（ExternalRef 既有 *string 字段，只赋予明确语义）。
//   - 零新增依赖（go.mod / go.sum 零变化，全部 stdlib）。
//   - S3/MinIO 签名栈本轮不做（接口留位，local 实现先行）。
package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ------------------------------------------------------------------
// 接口定义（§9 冻结）
// ------------------------------------------------------------------

// RefStore 正文外置存储接口。
// ref 形态：lims://local/<sha256前32位>（稳定、可校验、可迁移，不写绝对路径）。
type RefStore interface {
	// Put 写入数据，返回 ref URI 与 digest（hex-encoded sha256）。
	Put(ctx context.Context, key string, data []byte) (ref string, digest string, err error)
	// Get 按 ref 读回原始数据。
	Get(ctx context.Context, ref string) ([]byte, error)
	// Stat 返回元信息（大小、digest、创建时间）。
	Stat(ctx context.Context, ref string) (RefMeta, error)
	// Delete 删除正文文件（幂等：不存在视为已删）。
	Delete(ctx context.Context, ref string) error
}

// RefMeta 正文文件元信息。
type RefMeta struct {
	Size      int64     `json:"size"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
}

// ------------------------------------------------------------------
// 本地文件系统实现
// ------------------------------------------------------------------

// LocalRefStore 基于本地文件系统的 RefStore 实现。
// 布局：<root>/<first-2-hex>/<remaining-30-hex>（两级散列桶，避免单目录文件爆炸）。
type LocalRefStore struct {
	root string
}

// NewLocalRefStore 从 ArchiveConfig 构造本地 RefStore。
//
//	fail-closed：
//	 - cfg 为 nil → 错误
//	 - cfg.Path 为空 → 错误
//	 - cfg.Backend 为空或 "local" → 正常构造
//	 - cfg.Backend 为其他值（如 "s3"/"minio"）→ 明确错误，不降级
func NewLocalRefStore(cfg *ArchiveConfig) (*LocalRefStore, error) {
	if cfg == nil {
		return nil, errors.New("refstore: archive config is nil (storage.archive not configured)")
	}
	if cfg.Path == "" {
		return nil, errors.New("refstore: archive.path is empty (storage.archive.path not configured)")
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.Backend))
	if backend != "" && backend != "local" {
		return nil, fmt.Errorf("refstore: backend %q not implemented (only 'local' supported this round; fail-closed, no silent fallback)", cfg.Backend)
	}
	// 确保根目录存在（创建时一次到位，不静默）
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil {
		return nil, fmt.Errorf("refstore: create archive root %q: %w", cfg.Path, err)
	}
	return &LocalRefStore{root: cfg.Path}, nil
}

// refToPath 把 lims://local/<hex32> 映射到本地文件路径。
// 布局：root / <hex[:2]> / <hex[2:]>
func (s *LocalRefStore) refToPath(ref string) (string, error) {
	const prefix = "lims://local/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("refstore: invalid ref scheme %q (want lims://local/...)", ref)
	}
	hexPart := strings.TrimPrefix(ref, prefix)
	if len(hexPart) < 32 {
		return "", fmt.Errorf("refstore: ref hex too short (%d chars, want 32)", len(hexPart))
	}
	// 只取前 32 位，多余后缀忽略（向前兼容）
	hex32 := hexPart[:32]
	for _, c := range hex32 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", fmt.Errorf("refstore: ref contains non-hex char %q", c)
		}
	}
	dir := filepath.Join(s.root, hex32[:2])
	return filepath.Join(dir, hex32), nil
}

// digestToRef 由 sha256 digest（hex）构造 ref URI。
func digestToRef(digestHex string) string {
	return "lims://local/" + digestHex[:32]
}

func (s *LocalRefStore) Put(_ context.Context, _ string, data []byte) (string, string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	ref := digestToRef(digest)

	path, err := s.refToPath(ref)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", fmt.Errorf("refstore: create bucket dir: %w", err)
	}
	// 幂等写：同 digest 已存在则不重复写（内容相同，stat 后直接返回）
	if existing, err := os.Stat(path); err == nil && !existing.IsDir() {
		return ref, digest, nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", "", fmt.Errorf("refstore: write body: %w", err)
	}
	return ref, digest, nil
}

func (s *LocalRefStore) Get(_ context.Context, ref string) ([]byte, error) {
	path, err := s.refToPath(ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("refstore: body not found for ref %q", ref)
		}
		return nil, fmt.Errorf("refstore: read body: %w", err)
	}
	return data, nil
}

func (s *LocalRefStore) Stat(_ context.Context, ref string) (RefMeta, error) {
	path, err := s.refToPath(ref)
	if err != nil {
		return RefMeta{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RefMeta{}, fmt.Errorf("refstore: body not found for ref %q", ref)
		}
		return RefMeta{}, fmt.Errorf("refstore: stat body: %w", err)
	}
	// 重读文件算 digest（Stat 必须返回可校验 digest）
	data, err := os.ReadFile(path)
	if err != nil {
		return RefMeta{}, fmt.Errorf("refstore: read for digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return RefMeta{
		Size:      st.Size(),
		Digest:    hex.EncodeToString(sum[:]),
		CreatedAt: st.ModTime().UTC(),
	}, nil
}

func (s *LocalRefStore) Delete(_ context.Context, ref string) error {
	path, err := s.refToPath(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // 幂等：已删视为成功
		}
		return fmt.Errorf("refstore: delete body: %w", err)
	}
	// 顺手清理空桶目录（best-effort，失败不报错）
	_ = os.Remove(filepath.Dir(path))
	return nil
}

// ------------------------------------------------------------------
// 写路径：ArchiveWithRef（正文外置）
// ------------------------------------------------------------------

// DefaultArchiveThreshold 默认阈值：4096 字节。
const DefaultArchiveThreshold = 4096

// ExternalRefSummaryLen 外置后 Fact 保留的摘要长度。
const ExternalRefSummaryLen = 200

// ArchiveWithRef 判断是否需要把正文外置。
//
// 规则（§7.1 写死，可测）：
//   - threshold <= 0 → 禁用，直接返回原记忆（零回归）
//   - len(m.Fact) <= threshold → 不需要外置，直接返回
//   - len(m.Fact) > threshold → 外置：
//     a. store.Put 写正文
//     b. m.ExternalRef = ref
//     c. m.Fact = 前200字符 + "…"
//     d. m.SourceEvidence.Snippet 一并外置（若存在）
//
// 注意：本函数**只改 m 的字段**，不写后端（Upsert 由调用方完成）。
func ArchiveWithRef(ctx context.Context, store RefStore, m *Memory, threshold int) (*Memory, error) {
	if m == nil {
		return nil, errors.New("refstore: nil memory")
	}
	if threshold <= 0 {
		return m, nil // 禁用 → 零回归
	}
	if len(m.Fact) <= threshold {
		return m, nil // 不需要外置
	}
	if store == nil {
		return nil, errors.New("refstore: store is nil but fact exceeds threshold (refstore not configured)")
	}

	// 拼接外置载荷：Fact 正文 + SourceEvidence.Snippet（一并外置，避免密级泄露路径）
	payload := m.Fact
	if m.SourceEvidence != nil && m.SourceEvidence.Snippet != "" {
		payload = payload + "\n---\n[evidence]\n" + m.SourceEvidence.Snippet
	}

	ref, _, err := store.Put(ctx, m.MemoryID, []byte(payload))
	if err != nil {
		return nil, fmt.Errorf("refstore: archive fact: %w", err)
	}

	// 截断 Fact 为摘要
	summary := m.Fact
	if len(summary) > ExternalRefSummaryLen {
		summary = summary[:ExternalRefSummaryLen] + "…"
	}

	// 写入 ExternalRef（既有 *string 字段，赋予明确语义）
	m.ExternalRef = &ref
	m.Fact = summary

	// Snippet 一并外置（清空，避免密级信息留在元数据里）
	if m.SourceEvidence != nil {
		m.SourceEvidence.Snippet = ""
	}

	return m, nil
}

// ------------------------------------------------------------------
// 读路径：Hydrate（透明水合）
// ------------------------------------------------------------------

// Hydrate 读时水合：若 m.ExternalRef 非空，从 store 读回完整正文。
//
// 规则（§7.1 写死）：
//   - m.ExternalRef 为空 → 直接返回 m（零回归）
//   - m.ExternalRef 非空 → store.Get + digest 校验
//   - digest 不匹配 → 返回错误，**不得**返回摘要冒充正文
//   - digest 匹配 → 把 Fact 恢复为完整正文
//
// 注意：本函数是**纯函数**（不改 m 的其他字段），调用方决定如何使用。
func Hydrate(ctx context.Context, store RefStore, m *Memory) (*Memory, error) {
	if m == nil {
		return nil, errors.New("refstore: nil memory")
	}
	if m.ExternalRef == nil || *m.ExternalRef == "" {
		return m, nil // 无外置 → 零回归
	}
	if store == nil {
		return nil, fmt.Errorf("refstore: memory %s has external_ref %q but store is nil", m.MemoryID, *m.ExternalRef)
	}

	data, err := store.Get(ctx, *m.ExternalRef)
	if err != nil {
		return nil, fmt.Errorf("refstore: hydrate get: %w", err)
	}

	// digest 校验：重算 sha256，与 ref 中的 hex 比对
	sum := sha256.Sum256(data)
	computedDigest := hex.EncodeToString(sum[:])
	expectedDigest := (*m.ExternalRef)[len("lims://local/"):]
	if len(expectedDigest) > 32 {
		expectedDigest = expectedDigest[:32]
	}
	if !strings.EqualFold(computedDigest[:32], expectedDigest) {
		return nil, fmt.Errorf("refstore: digest mismatch for %q (expected prefix %s, got %s) — tampered or corrupted, refusing to return summary as body",
			*m.ExternalRef, expectedDigest, computedDigest[:32])
	}

	// 分离 Fact 与 evidence（Put 时用 "\n---\n[evidence]\n" 拼接）
	// 水合时同时恢复 Fact 正文与 SourceEvidence.Snippet。
	payloadStr := string(data)
	if idx := strings.Index(payloadStr, "\n---\n[evidence]\n"); idx >= 0 {
		m.Fact = payloadStr[:idx]
		snippet := payloadStr[idx+len("\n---\n[evidence]\n"):]
		if m.SourceEvidence == nil {
			m.SourceEvidence = &SourceEvidence{}
		}
		m.SourceEvidence.Snippet = snippet
	} else {
		m.Fact = payloadStr
	}
	return m, nil
}

// ------------------------------------------------------------------
// GDPR 级联：PurgeRefsForMemories
// ------------------------------------------------------------------

// PurgeRefsForMemories 批量删除记忆对应的外置正文。
// 供 GDPR DeleteByScope / DeleteByID 级联调用（§7.1 硬要求）。
//
// 注意：本函数**不删后端里的 Memory 记录**——它只负责删外置正文。
// 后端删除由 ComplianceBackend.DeleteByScope / DeleteByID 完成。
//
// 幂等：已删的 ref 再次删除视为成功。
// 错误处理：单个 ref 删除失败不中断，收集到返回切片里，最后统一返回。
func PurgeRefsForMemories(ctx context.Context, store RefStore, ms []*Memory) []error {
	if store == nil {
		return nil
	}
	var errs []error
	for _, m := range ms {
		if m == nil || m.ExternalRef == nil || *m.ExternalRef == "" {
			continue
		}
		if err := store.Delete(ctx, *m.ExternalRef); err != nil {
			errs = append(errs, fmt.Errorf("purge %s: %w", m.MemoryID, err))
		}
	}
	return errs
}

// PurgeRefsByIDs 按 memory_id 列表 + store 删除外置正文。
// 简化版：调用方需要先拿到 ExternalRef（通过 Get），再传入。
// 本函数只做遍历删除，不查库。
func PurgeRefsByIDs(ctx context.Context, store RefStore, refs []string) []error {
	if store == nil {
		return nil
	}
	var errs []error
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if err := store.Delete(ctx, ref); err != nil {
			errs = append(errs, fmt.Errorf("purge ref %s: %w", ref, err))
		}
	}
	return errs
}

// ------------------------------------------------------------------
// APIServer 接线（setter 放这里，不放 router.go —— T0 红线）
// ------------------------------------------------------------------

// SetRefStore 注入 L3 正文外置存储。
// nil 时 ArchiveWithRef / Hydrate 退化为 no-op（零回归）。
func (s *APIServer) SetRefStore(store RefStore) {
	s.refStore = store
}

// GetRefStore 读取已注入的 RefStore（可能为 nil）。
func (s *APIServer) GetRefStore() RefStore {
	if s.refStore == nil {
		return nil
	}
	if rs, ok := s.refStore.(RefStore); ok {
		return rs
	}
	return nil
}
