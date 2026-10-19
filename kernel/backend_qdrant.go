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
		// R24-B · L-227：落 vector_status 便于 VectorSearch 过滤掉 pending /
		// 零向量点（§5.1 B1「避免 pending 点参与相似度」）。
		// 只增 key，不改既有 5 个 key 的语义，老 payload 缺该键时按 pending 处理。
		"vector_status": string(m.VectorStatus),
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
	// R24-B · L-227：修复前这里无条件写 `make([]float32, b.dim)` 全零向量，
	// 且 Upsert 全程不碰 VectorStatus —— 于是「没有向量」和「有一个全零向量」
	// 在索引里完全无法区分，VectorSearch 会把零向量当成真实向量参与相似度，
	// 而 Reconcile 也报不出「零向量」这一档。
	//
	// 修复口径（§5.1 B1，并根据**真机实测**修正，见下）：
	//   - 点已存在且向量可用 → 复用既有向量，vector_status=vectorized；
	//   - 否则 → vector_status=pending。
	//
	// ⚠️ 实测修正（Qdrant 1.19.1，2026-10-19）：任务书 B1 假设「不写 vector 字段，
	//    或按 Qdrant 契约写 null」都可以。真机实测**两种写法都被拒**：
	//      · 省略 vector 字段 → 400 `missing field \`vector\``
	//      · "vector": null   → 400 `data did not match any variant of untagged enum VectorStruct`
	//    即 Qdrant 1.19 的 point upsert **强制要求**携带合法向量。
	//    因此本实现改为：**仍写零向量（协议所迫），但把 vector_status 明确标为
	//    pending，并在 VectorSearch 侧过滤掉非 vectorized 的点**。
	//    效果与任务书意图等价：pending 点不参与相似度排序，且 Reconcile 能把
	//    「零向量」这一档如实报出来（判据①正是靠它验证重建后归零）。
	var vec []float32
	status := VectorPending
	if existing, err := b.fetchPoint(ctx, m.MemoryID); err == nil && existing != nil && len(existing.Vector) == b.dim && !isZeroVector(existing.Vector) {
		vec = existing.Vector
		status = VectorVectorized
	}
	if m.VectorStatus != "" {
		// 调用方显式声明的状态优先（例如 BackfillVectors 补完后回写 vectorized）。
		if m.VectorStatus == VectorVectorized && len(vec) == b.dim {
			status = VectorVectorized
		} else if m.VectorStatus == VectorPending {
			status = VectorPending
		}
	}
	// 必须在 memoryPayload(m) **之前**定稿 VectorStatus：payload 是该字段的
	// 唯一落盘载体（VectorSearch 的 vector_status 过滤与 Reconcile 的统计都读它）。
	// 早先版本把赋值放在 payload 之后，导致 payload 里的 vector_status 恒为旧值。
	m.VectorStatus = status

	if len(vec) != b.dim {
		// Qdrant 强制要求 vector 字段；无真实向量时写零向量并标 pending。
		vec = make([]float32, b.dim)
	}
	// payload 必须在 VectorStatus 定稿**之后**生成：payload 的 `_memory` 与
	// 顶层 `vector_status` 是该字段的唯一落盘载体（VectorSearch 的过滤与
	// Reconcile 的统计都读它）。
	payload, err := memoryPayload(m)
	if err != nil {
		return err
	}
	pt := map[string]any{
		"id":      qdrantPointID(m.MemoryID),
		"vector":  vec,
		"payload": payload,
	}
	upsertReq := map[string]any{"points": []map[string]any{pt}}
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
		// R24-B · L-226：默认只返回当前生效版本（三后端一致）。
		if !q.IncludeSuperseded && m.SupersededBy != nil {
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
	// R24-B · L-227：只让 vectorized 的点参与相似度（§5.1 B1）。
	// 缺 vector_status 键的老 payload 一律视为 pending，不参与向量检索——
	// 宁可少召回，也不让零向量污染排序（与 PG 的部分索引 `WHERE vector_status='vectorized'`
	// 同口径）。
	must := []map[string]any{{
		"key":   "vector_status",
		"match": map[string]any{"value": string(VectorVectorized)},
	}}
	if q.TenantID != "" {
		must = append(must, map[string]any{
			"key":   "tenant",
			"match": map[string]any{"value": q.TenantID},
		})
	}
	searchReq["filter"] = map[string]any{"must": must}
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

// ---------------------------------------------------------------------
// R24-B · B2 补向量 + B3 索引审计（可选能力接口，不进 StorageBackend）
// ---------------------------------------------------------------------

// isZeroVector 判断向量是否全零（L-227 的核心判据）。
// 空向量视为「无向量」，不计入零向量（无向量是 pending，零向量才是污染）。
func isZeroVector(v []float32) bool {
	if len(v) == 0 {
		return false
	}
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// scrollPoints 滚动取出全部点（**带向量**），供补向量与索引审计使用。
func (b *qdrantBackend) scrollPoints(ctx context.Context) ([]qdrantPoint, error) {
	var resp struct {
		Result struct {
			Points []qdrantPoint `json:"points"`
		} `json:"result"`
	}
	scrollReq := map[string]any{
		"limit":        1000,
		"with_payload": true,
		"with_vector":  true,
	}
	if err := b.do(ctx, http.MethodPost, "/collections/"+b.col+"/points/scroll", scrollReq, &resp); err != nil {
		return nil, err
	}
	return resp.Result.Points, nil
}

// BackfillVectors 实现 VectorBackfiller（B2）：滚动取「无向量或 pending」的点，
// 逐个算向量后批量回写（带 vector 字段 + vector_status=vectorized）。
//
// 与 PG 版同口径：只补不删；补完的点才参与 VectorSearch（见 Upsert 的 status 处理）。
func (b *qdrantBackend) BackfillVectors(ctx context.Context, embedder func(string) ([]float32, error)) (BackfillReport, error) {
	var rep BackfillReport
	if embedder == nil {
		return rep, fmt.Errorf("qdrant backfill: embedder is nil")
	}
	pts, err := b.scrollPoints(ctx)
	if err != nil {
		return rep, fmt.Errorf("qdrant backfill: scroll: %w", err)
	}
	for _, p := range pts {
		m, err := parseMemoryPayload(p.Payload)
		if err != nil || m == nil {
			continue
		}
		rep.Scanned++
		// 已有真实向量且标记为 vectorized → 无需补。
		if len(p.Vector) == b.dim && !isZeroVector(p.Vector) && m.VectorStatus == VectorVectorized {
			continue
		}
		vec, err := embedder(m.Fact)
		if err != nil || len(vec) != b.dim {
			rep.Failed++
			continue
		}
		m.VectorStatus = VectorVectorized
		payload, err := memoryPayload(m)
		if err != nil {
			rep.Failed++
			continue
		}
		pt := map[string]any{
			"id":      qdrantPointID(m.MemoryID),
			"vector":  vec,
			"payload": payload,
		}
		req := map[string]any{"points": []map[string]any{pt}}
		if err := b.do(ctx, http.MethodPut, "/collections/"+b.col+"/points?wait=true", req, nil); err != nil {
			rep.Failed++
			continue
		}
		rep.Vectorized++
	}
	return rep, nil
}

// AuditVectorIndex 实现 VectorIndexAuditor（B3）：统计索引侧的点数、零向量数，
// 以及与元数据的孤儿/缺失差异。
func (b *qdrantBackend) AuditVectorIndex(ctx context.Context, tenant string) (*IndexAudit, error) {
	pts, err := b.scrollPoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("qdrant audit: scroll: %w", err)
	}
	// 元数据侧：以 Scroll（payload）为准——Qdrant 是「元数据与索引同点」的后端，
	// 故不存在真正的孤儿索引；缺失索引 = 无向量（或零向量）的点。
	audit := &IndexAudit{}
	for _, p := range pts {
		m, err := parseMemoryPayload(p.Payload)
		if err != nil || m == nil {
			// payload 无法解析 = 索引有、元数据无 → 孤儿
			audit.OrphanIDs = append(audit.OrphanIDs, p.ID)
			continue
		}
		if tenant != "" && m.TenantID != tenant {
			continue
		}
		audit.Indexed++
		if isZeroVector(p.Vector) {
			audit.ZeroVector++
			audit.MissingIDs = append(audit.MissingIDs, m.MemoryID)
			continue
		}
		if len(p.Vector) == 0 {
			audit.MissingIDs = append(audit.MissingIDs, m.MemoryID)
		}
	}
	return audit, nil
}
