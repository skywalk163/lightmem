// Qdrant 存储后端（S1，§5.4 多后端可插拔）
// 真源：lightmem.md §5.4 存储后端可插拔清单；profiles/lims.profile.schema.json QdrantConfig。
//
// 设计约束（R16 S1 红线 + R17 S2 真机验收修正）：
//   - 只增不改：本文件不修改 StorageBackend / LayerScanner / ComplianceBackend 的既有
//     方法签名与 Memory / QueryParams / QueryResult 字段语义。
//   - 只读骨架优先：Qdrant 未配置（cfg.Qdrant == nil）时 newQdrantBackend 返回明确
//     error，不静默降级为其它后端（延续 R15 "SKIP 不冒充 PASS" 的诚实口径）。
//   - 确定性优先：文本检索与 PG/SQLite 一致，用 tokenize 词元重合度打分，
//     保证 eval/run_kpi.go 在 qdrant 档位下可复现。
//   - stdlib-only：只用 net/http + encoding/json + crypto/sha1，不新增第三方依赖，
//     不修改 go.mod。
//   - 点 ID 映射（R17 S2 真机修正）：Qdrant 1.19 的点 ID 类型 ExtendedPointId 只接受
//     u64 整数或 UUID 字符串，而 lightmem 的 MemoryID 是 ULID 风格任意字符串
//     （router_tools.go newMemoryID）。直接透传会被真机 Qdrant 以 400
//     "not a valid point ID" 全部拒绝——R16 的 httptest mock 接受任意字符串 ID，
//     掩盖了该契约差异。故统一走 qdrantPointID 确定性 UUIDv5 映射，原 MemoryID
//     完整保留在 payload._memory 的 Memory JSON 中，对外语义不变。
//   - 写操作带 wait=true（R17 S2 真机修正）：Qdrant 写为异步两段提交
//     （acknowledged→completed），显式 wait 保证读己之写（SaveMemory 后立即
//     Query 可见）。httptest mock 忽略 query 参数，既有契约测试不受影响。
package kernel

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// qdrantDefaultDim 向量维度缺省（与 backend_pg.go 默认 1024 对齐）。
const qdrantDefaultDim = 1024

// qdrantIDNamespace 是 lightmem 专用的 UUIDv5 命名空间（16 字节固定值）。
// 仅需全局唯一且跨版本稳定，无外部契约。
var qdrantIDNamespace = [16]byte{'l', 'i', 'g', 'h', 't', 'm', 'e', 'm', '-', 'q', 'd', 'r', 'a', 'n', 't', '1'}

// qdrantPointID 把任意 MemoryID 字符串确定性地映射为 Qdrant 合法的点 ID。
//
// 真源：Qdrant 1.19 lib/segment/src/types.rs 的 ExtendedPointId 反序列化——
// 既非 u64 也非 UUID 的字符串一律报错 "value ... is not a valid point ID,
// valid values are either an unsigned integer or a UUID"。
//
// 算法：UUIDv5 风格（RFC 4122）——SHA-1(namespace || memoryID) 取前 128 位，
// 置 version=5、variant=RFC4122 标志位。同一 MemoryID 永远得到同一点 ID，
// Upsert/Get/Delete/UpdateLayer 全路径一致。stdlib-only（crypto/sha1）。
func qdrantPointID(memoryID string) string {
	h := sha1.New()
	h.Write(qdrantIDNamespace[:])
	h.Write([]byte(memoryID))
	sum := h.Sum(nil) // 20 bytes，取前 16 字节
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// qdrantBackend 实现 StorageBackend / LayerScanner / ComplianceBackend。
type qdrantBackend struct {
	name    string
	httpURL string // e.g. http://127.0.0.1:6333
	apiKey  string
	col     string // collection 名
	dim     int
	client  *http.Client
}

// newQdrantBackend 构造 Qdrant 后端。cfg.Qdrant == nil 时返回明确错误（不降级）。
func newQdrantBackend(ctx context.Context, cfg *StorageConfig) (StorageBackend, error) {
	if cfg == nil || cfg.Qdrant == nil {
		return nil, fmt.Errorf("qdrant backend requires storage.qdrant config (not configured)")
	}
	q := cfg.Qdrant
	if q.Host == "" {
		return nil, fmt.Errorf("qdrant host required")
	}
	port := q.Port
	if port == 0 {
		port = 6333 // Qdrant HTTP REST 默认端口
	}
	scheme := "http"
	if q.HTTPS {
		scheme = "https"
	}
	b := &qdrantBackend{
		name:    "qdrant",
		httpURL: fmt.Sprintf("%s://%s:%d", scheme, q.Host, port),
		apiKey:  q.APIKey,
		col:     "lightmem_memories",
		dim:     qdrantDefaultDim,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	if err := b.ensureCollection(ctx); err != nil {
		return nil, fmt.Errorf("qdrant backend init: %w", err)
	}
	return b, nil
}

// Name 返回后端标识。
func (b *qdrantBackend) Name() string { return b.name }

// ---------------------------------------------------------------------
// HTTP 底层
// ---------------------------------------------------------------------

func (b *qdrantBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.httpURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		req.Header.Set("api-key", b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ensureCollection 幂等建集合（DDL 只含 CREATE，符合 R14 红线）。
func (b *qdrantBackend) ensureCollection(ctx context.Context) error {
	var info struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	err := b.do(ctx, http.MethodGet, "/collections/"+b.col, nil, &info)
	if err == nil {
		return nil // 已存在
	}
	// 不存在则 CREATE（wait=true：等集合就绪再返回，保证随后读写可用）
	createReq := map[string]any{
		"vectors": map[string]any{
			"size":     b.dim,
			"distance": "Cosine",
		},
	}
	return b.do(ctx, http.MethodPut, "/collections/"+b.col+"?wait=true", createReq, nil)
}

// point 是 Qdrant point 结构（payload 承载 Memory JSON）。
type qdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// memoryPayload 把 Memory 打包成 Qdrant payload。
// 约定：memory 序列化到 key "_memory"（字符串），layer/tenant/user/kind 抽出便于 filter。
func memoryPayload(m *Memory) (map[string]any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"_memory": string(raw),
		"layer":   string(m.Layer),
		"tenant":  m.TenantID,
		"user":    m.UserID,
		"kind":    string(m.MemoryKind),
	}, nil
}

// parseMemoryPayload 从 Qdrant payload 还原 Memory。
func parseMemoryPayload(p map[string]any) (*Memory, error) {
	raw, ok := p["_memory"]
	if !ok {
		return nil, fmt.Errorf("payload missing _memory")
	}
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	default:
		// 兼容 Qdrant 版本差异：可能已反序列化为 object。
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		s = string(b)
	}
	var m Memory
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------------------------------------------------------------------
// StorageBackend
// ---------------------------------------------------------------------

func (b *qdrantBackend) Ping(ctx context.Context) error {
	return b.do(ctx, http.MethodGet, "/collections/"+b.col, nil, nil)
}

func (b *qdrantBackend) Upsert(ctx context.Context, m *Memory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.MemoryID == "" {
		return fmt.Errorf("memory_id required")
	}
	if m.RecordedAt.IsZero() {
		m.RecordedAt = time.Now().UTC()
	}
	payload, err := memoryPayload(m)
	if err != nil {
		return err
	}
	// 若点已存在，取回其向量避免零向量覆盖。
	vec := make([]float32, b.dim)
	if existing, err := b.fetchPoint(ctx, m.MemoryID); err == nil && existing != nil && len(existing.Vector) == b.dim {
		vec = existing.Vector
	}
	upsertReq := map[string]any{
		"points": []qdrantPoint{{ID: qdrantPointID(m.MemoryID), Vector: vec, Payload: payload}},
	}
	return b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", upsertReq, nil)
}

func (b *qdrantBackend) Get(ctx context.Context, memoryID, viewer, tenant string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := b.fetchPoint(ctx, memoryID)
	if err != nil {
		return nil, err
	}
	m, err := parseMemoryPayload(p.Payload)
	if err != nil {
		return nil, err
	}
	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not visible to %s/%s", memoryID, viewer, tenant)
	}
	return m, nil
}

func (b *qdrantBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mems, err := b.scrollAll(ctx)
	if err != nil {
		return nil, err
	}
	var res []QueryResult
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	for _, m := range mems {
		if !m.canView(q.Viewer, q.TenantID) {
			continue
		}
		if len(q.MemoryKinds) > 0 && !containsKind(q.MemoryKinds, m.MemoryKind) {
			continue
		}
		if q.AsOf != nil && !m.visibleAt(*q.AsOf) {
			continue
		}
		score := 0.0
		if needle != "" {
			fact := strings.ToLower(m.Fact)
			if fact == needle {
				score = 1.0
			} else if strings.Contains(fact, needle) {
				score = 0.7
			} else {
				qTokens := tokenize(needle)
				if len(qTokens) > 0 {
					fSet := tokenSet(m.Fact)
					hit := 0
					for _, t := range qTokens {
						if fSet[t] {
							hit++
						}
					}
					if hit > 0 {
						score = 0.5 * float64(hit) / float64(len(qTokens))
					} else {
						continue
					}
				} else {
					continue // 查询无有效词元，跳过（与 SQLite 骨架一致）
				}
			}
		}
		if score <= 0 && q.Text == "" {
			score = 0.1 // 空查询：全部返回低分，便于指标统计
		}
		score += m.Confidence * 0.2 // 与 SQLite 骨架一致（§4.4 打分约定）
		res = append(res, QueryResult{
			MemoryID:     m.MemoryID,
			Fact:         m.Fact,
			Score:        score,
			Source:       "qdrant",
			OccurredAt:   m.OccurredAt,
			RecordedAt:   m.RecordedAt,
			ValidFrom:    m.ValidFrom,
			ValidUntil:   m.ValidUntil,
			SupersededBy: m.SupersededBy,
			IsHistorical: m.IsHistorical,
		})
	}
	sort.SliceStable(res, func(i, j int) bool {
		if res[i].Score != res[j].Score {
			return res[i].Score > res[j].Score
		}
		return res[i].MemoryID < res[j].MemoryID
	})
	if q.TopK > 0 && len(res) > q.TopK {
		res = res[:q.TopK]
	}
	return res, nil
}

func (b *qdrantBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(q.Vector) == 0 {
		return b.Query(ctx, q) // 向量缺失：降级为文本检索（§4.2 约定）
	}
	searchReq := map[string]any{
		"vector":       q.Vector,
		"limit":        q.TopK,
		"with_payload": true,
	}
	if q.TenantID != "" {
		searchReq["filter"] = map[string]any{
			"must": []map[string]any{{
				"key":   "tenant",
				"match": map[string]any{"value": q.TenantID},
			}},
		}
	}
	var resp struct {
		Result []struct {
			ID      string         `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := b.do(ctx, http.MethodPost, "/collections/"+b.col+"/points/search", searchReq, &resp); err != nil {
		// 搜索失败：降级为文本检索并回填 pending（§4.2）
		return b.Query(ctx, q)
	}
	var res []QueryResult
	for _, r := range resp.Result {
		m, err := parseMemoryPayload(r.Payload)
		if err != nil {
			continue
		}
		if !m.canView(q.Viewer, q.TenantID) {
			continue
		}
		res = append(res, QueryResult{
			MemoryID: m.MemoryID, Fact: m.Fact, Score: r.Score, Source: "qdrant-vector",
			OccurredAt: m.OccurredAt, RecordedAt: m.RecordedAt,
			ValidFrom: m.ValidFrom, ValidUntil: m.ValidUntil,
			SupersededBy: m.SupersededBy, IsHistorical: m.IsHistorical,
		})
	}
	return res, nil
}

func (b *qdrantBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	mems, err := b.scrollAll(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, m := range mems {
		if m.MemoryKind == kind && (tenant == "" || m.TenantID == tenant) {
			n++
		}
	}
	return n, nil
}

func (b *qdrantBackend) Close() error { return nil }

// ---------------------------------------------------------------------
// LayerScanner
// ---------------------------------------------------------------------

func (b *qdrantBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	mems, err := b.scrollAll(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Memory
	for _, m := range mems {
		if layer != "" && m.Layer != layer {
			continue
		}
		if tenant != "" && m.TenantID != tenant {
			continue
		}
		out = append(out, m)
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (b *qdrantBackend) UpdateLayer(ctx context.Context, memoryID string, layer MemoryLayer) error {
	p, err := b.fetchPoint(ctx, memoryID)
	if err != nil {
		return err
	}
	m, err := parseMemoryPayload(p.Payload)
	if err != nil {
		return err
	}
	m.Layer = layer
	payload, err := memoryPayload(m)
	if err != nil {
		return err
	}
	upsertReq := map[string]any{
		"points": []qdrantPoint{{ID: qdrantPointID(memoryID), Vector: p.Vector, Payload: payload}},
	}
	return b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", upsertReq, nil)
}

// ---------------------------------------------------------------------
// ComplianceBackend
// ---------------------------------------------------------------------

func (b *qdrantBackend) ExportByScope(ctx context.Context, tenant, userID string) ([]*Memory, error) {
	mems, err := b.scrollAll(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Memory
	for _, m := range mems {
		if m.TenantID == tenant && m.UserID == userID {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out, nil
}

func (b *qdrantBackend) DeleteByScope(ctx context.Context, tenant, userID string) ([]string, error) {
	mems, err := b.scrollAll(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range mems {
		if m.TenantID == tenant && m.UserID == userID {
			ids = append(ids, m.MemoryID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ids, nil
	}
	// 请求用映射后的点 ID；返回值仍是原始 MemoryID（合规导出语义不变）。
	delIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		delIDs = append(delIDs, qdrantPointID(id))
	}
	delReq := map[string]any{"points": delIDs}
	if err := b.do(ctx, http.MethodPost, "/collections/"+b.col+"/points/delete?wait=true", delReq, nil); err != nil {
		return nil, err
	}
	return ids, nil
}

func (b *qdrantBackend) DeleteByID(ctx context.Context, memoryID string) error {
	if memoryID == "" {
		return fmt.Errorf("memory_id required")
	}
	delReq := map[string]any{"points": []string{qdrantPointID(memoryID)}}
	return b.do(ctx, http.MethodPost, "/collections/"+b.col+"/points/delete?wait=true", delReq, nil)
}

// ---------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------

func (b *qdrantBackend) fetchPoint(ctx context.Context, id string) (*qdrantPoint, error) {
	var resp struct {
		Result qdrantPoint `json:"result"`
	}
	if err := b.do(ctx, http.MethodGet, "/collections/"+b.col+"/points/"+qdrantPointID(id), nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Result, nil
}

// scrollAll 拉回集合内全部点（骨架实现：单页 scroll；骨架档数据量小）。
func (b *qdrantBackend) scrollAll(ctx context.Context) ([]*Memory, error) {
	var resp struct {
		Result struct {
			Points []struct {
				ID      string         `json:"id"`
				Payload map[string]any `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	scrollReq := map[string]any{
		"limit":        1000,
		"with_payload": true,
	}
	if err := b.do(ctx, http.MethodPost, "/collections/"+b.col+"/points/scroll", scrollReq, &resp); err != nil {
		return nil, err
	}
	var out []*Memory
	for _, p := range resp.Result.Points {
		m, err := parseMemoryPayload(p.Payload)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// tokenSet / containsKind / visibleAt / tokenize 均复用 kernel 包既有声明：
//   tokenSet / tokenize  -> rank_fusion.go
//   containsKind         -> backend_sqlite.go
//   visibleAt / canView  -> backend_sqlite.go
// 这里不重复声明，避免包级 redeclaration 编译错。
//
// 说明：memoryPayload / parseMemoryPayload 是本文件私有，不会与其它文件冲突。
