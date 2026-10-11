// HTTP API 骨架（§4.4）
// 最小可运行路由：GET /health、GET /profile、POST /memories、GET /search、GET /metrics/kinds。
// 生产实现需替换为真实 http.ServeMux + 中间件链（可见性注入、审计、Capsicum 沙箱）。
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// APIServer 持有装配好的后端
type APIServer struct {
	profile  *Profile
	backend  StorageBackend
	embedder func(string) ([]float32, error) // 向量化函数（§4.2 异步补全）；nil 时禁用
	layerPol *LayerPolicy                    // 生命周期策略（§3.10）；nil 时用 DefaultLayerPolicy()
	fusion   *RankFusion                     // 多信号 rank 融合引擎（§3.4）；nil 时用 DefaultRankFusion()
	policy   *WritePolicyRegistry            // 写策略注册表（§3.15）；nil 时用内置默认策略
	// FeedbackLoop：rejected_ids 记录用户拒绝的记忆，后续检索时过滤。
	rejectedIDs []string
}

func NewAPIServer(p *Profile, b StorageBackend) *APIServer {
	return &APIServer{profile: p, backend: b, policy: NewDefaultWritePolicyRegistry()}
}

// SetEmbedder 注入向量化函数（EmbeddingModel），供 BackfillVectors 使用。
func (s *APIServer) SetEmbedder(f func(string) ([]float32, error)) {
	s.embedder = f
}

// SetLayerPolicy 注入生命周期策略（§3.10）。
func (s *APIServer) SetLayerPolicy(p LayerPolicy) {
	s.layerPol = &p
}

// SetRankFusion 注入多信号 rank 融合引擎（§3.4）。
func (s *APIServer) SetRankFusion(rf *RankFusion) {
	s.fusion = rf
}

// SetWritePolicyRegistry 注入写策略注册表（§3.15）。
func (s *APIServer) SetWritePolicyRegistry(r *WritePolicyRegistry) {
	s.policy = r
}

// Handler 返回 http.Handler（可直接挂到 http.ListenAndServe）
func (s *APIServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /profile", s.handleProfile)
	mux.HandleFunc("POST /memories", s.handleUpsert)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /metrics/kinds", s.handleKinds)
	mux.HandleFunc("GET /metrics/layers", s.handleLayerMetrics)
	mux.HandleFunc("POST /search/vector", s.handleVectorSearch)
	mux.HandleFunc("POST /vector/backfill", s.handleBackfill)
	mux.HandleFunc("POST /lifecycle/run", s.handleLifecycleRun)
	// P2 智能路由与融合（§3.1/§3.4）
	mux.HandleFunc("POST /mem/router", s.handleMemRouter)
	// P2 Agent 记忆工具接口（§4.4）
	mux.HandleFunc("POST /tools/memory/recall", s.handleToolRecall)
	mux.HandleFunc("POST /tools/memory/save", s.handleToolSave)
	mux.HandleFunc("POST /tools/memory/update", s.handleToolUpdate)
	mux.HandleFunc("POST /tools/memory/feedback", s.handleToolFeedback)
	mux.HandleFunc("POST /tools/memory/forget", s.handleToolForget)
	// P3 检索 trace（§3.4）
	mux.HandleFunc("GET /search/trace", s.handleSearchTrace)
	// P4 审计 + 监控（§3.10/§5.3）
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /audit/{memory_id}", s.handleAudit)
	mux.HandleFunc("GET /audit", s.handleAuditList)
	return mux
}

func (s *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.backend.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"profile": string(s.profile.Profile),
		"backend": s.backend.Name(),
	})
}

func (s *APIServer) handleProfile(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, json.RawMessage(s.profile.ProfileJSON()))
}

func (s *APIServer) handleUpsert(w http.ResponseWriter, r *http.Request) {
	var m Memory
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// §3.15 写路径：幂等检查前置于 WritePolicy，再由注册表统一校验。
	saved, err := s.SaveMemory(ctx, &m, m.UserID)
	if errors.Is(err, ErrIdempotentNoop) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idempotent_noop", "memory": saved})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *APIServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := QueryParams{
		TenantID:    q.Get("tenant_id"),
		Viewer:      q.Get("viewer"),
		Text:        q.Get("q"),
		MemoryKinds: parseKinds(q.Get("kind")),
		TopK:        atoi(q.Get("top_k"), 10),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, err := s.backend.Query(ctx, params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *APIServer) handleKinds(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant_id")
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := map[string]int64{}
	for _, k := range []MemoryKind{KindEvent, KindFact, KindPreference, KindProcedure, KindGoal} {
		n, err := s.backend.CountByKind(ctx, k, tenant)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out[string(k)] = n
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVectorSearch 向量检索（§4.4 /search/vector）。
// 失败时返回 503，由调用方降级为 /search（§4.2 契约）。
func (s *APIServer) handleVectorSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query    string    `json:"query"`
		TenantID string    `json:"tenant_id"`
		Viewer   string    `json:"viewer"`
		Vector   []float32 `json:"vector"` // 可选：直接传向量
		TopK     int       `json:"top_k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	// 未直接传向量时，用 embedder 对 query 做向量化
	var vec []float32
	if len(req.Vector) > 0 {
		vec = req.Vector
	} else if req.Query != "" && s.embedder != nil {
		v, err := s.embedder(req.Query)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "embedding failed: " + err.Error(), "downgrade": "use GET /search?q=..."})
			return
		}
		vec = v
	} else {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "need query (with embedder) or vector", "downgrade": "use GET /search?q=..."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.backend.VectorSearch(ctx, QueryParams{
		TenantID: req.TenantID,
		Viewer:   req.Viewer,
		Vector:   vec,
		TopK:     req.TopK,
	})
	if err != nil {
		// §4.2 降级契约：向量检索失败 → 503 + 提示降级
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": err.Error(), "downgrade": "use GET /search?q=..."})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBackfill 手动触发一次向量补全（§4.2 后台 worker）。
func (s *APIServer) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if s.embedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "embedder not configured"})
		return
	}
	pg, ok := s.backend.(*pgBackend)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backfill only on pgvector backend"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := pg.BackfillVectors(ctx, s.embedder); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "backfilled"})
}

// handleLayerMetrics 返回四层计数（§3.10 温度指标）。
// layer 为空表示统计全部层。仅 pgvector 后端支持。
func (s *APIServer) handleLayerMetrics(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant_id")
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	pg, ok := s.backend.(*pgBackend)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "layer metrics only on pgvector backend"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := map[string]int64{}
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		n, err := pg.CountByLayer(ctx, layer, tenant)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out[string(layer)] = n
	}
	total := int64(0)
	for _, v := range out {
		total += v
	}
	out["total"] = total
	writeJSON(w, http.StatusOK, out)
}

// handleSearchTrace 返回最近一次检索的 trace（P3 retrieval_trace）。
func (s *APIServer) handleSearchTrace(w http.ResponseWriter, r *http.Request) {
	// 从 query 参数获取 text
	text := r.URL.Query().Get("text")
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rc := RouterContext{Text: text, TenantID: r.URL.Query().Get("tenant_id"), UserID: r.URL.Query().Get("user_id")}
	res, err := s.RecallAndFuse(ctx, rc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res.Trace)
}

// handleLifecycleRun 手动触发一次分层生命周期扫描（§3.10）。
func (s *APIServer) handleLifecycleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		Max      int    `json:"max"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	if _, ok := s.backend.(LayerScanner); !ok {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "lifecycle requires LayerScanner backend (pgvector)"})
		return
	}
	policy := DefaultLayerPolicy()
	if s.layerPol != nil {
		policy = *s.layerPol
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := LifecycleScanner(ctx, s.backend, policy, req.TenantID, req.Max)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleMetrics 返回 Prometheus 格式指标（P4 监控）。
func (s *APIServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// 支持两种格式：JSON（默认）和 Prometheus 文本格式（?format=prometheus）
	format := r.URL.Query().Get("format")

	if format == "prometheus" {
		// Prometheus 文本格式
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		metrics := GetMetricsCollector()
		_, _ = w.Write([]byte(metrics.PrometheusText()))
		return
	}

	// JSON 格式
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 统计各层条数
	counts := make(map[string]int64)
	for _, layer := range []MemoryLayer{LayerL0, LayerL1, LayerL2, LayerL3} {
		if sc, ok := s.backend.(LayerScanner); ok {
			ms, _ := sc.ScanLayer(ctx, layer, "", 10000)
			counts[string(layer)] = int64(len(ms))
		}
	}

	// 统计污染率（被拒绝的记忆数 / 总记忆数）
	total := int64(0)
	for _, c := range counts {
		total += c
	}

	// 获取指标快照
	metrics := GetMetricsCollector()
	snap := metrics.Snapshot()

	// 合并层容量
	for layer, count := range counts {
		snap.LayerCounts[layer] = count
	}
	snap.TotalMemories = total

	writeJSON(w, http.StatusOK, map[string]any{
		"lims_recall_latency_ms":    snap.RecallLatencyMS,
		"lims_hit_rate":             snap.HitRate,
		"lims_pollution_rate":       snap.PollutionRate,
		"lims_layer_counts":         snap.LayerCounts,
		"lims_total_memories":       snap.TotalMemories,
		"lims_total_queries":        snap.TotalQueries,
		"lims_signal_latencies_ms":  snap.SignalLatencies,
		"lims_feedback_stats":       snap.FeedbackStats,
		"lims_lifecycle_stats":      snap.LifecycleStats,
	})
}

// handleAudit 返回指定记忆的操作历史（P4 审计）。
func (s *APIServer) handleAudit(w http.ResponseWriter, r *http.Request) {
	memoryID := r.PathValue("memory_id")
	if memoryID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id required"})
		return
	}

	// 从审计日志获取
	audit := GetAuditLogger()
	entries := audit.GetEntries(memoryID)

	// 检查是否在 rejectedIDs 中
	rejected := false
	for _, rid := range s.rejectedIDs {
		if rid == memoryID {
			rejected = true
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"memory_id":   memoryID,
		"rejected":    rejected,
		"audit_log":   entries,
		"audit_count": len(entries),
	})
}

// handleAuditList 返回审计日志列表（P4 审计）。
func (s *APIServer) handleAuditList(w http.ResponseWriter, r *http.Request) {
	audit := GetAuditLogger()

	// 支持 limit 参数
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		// 简单解析
		if n := 0; len(l) > 0 {
			for _, r := range l {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
			if n > 0 && n < 1000 {
				limit = n
			}
		}
	}

	// 支持 tenant 过滤
	tenantID := r.URL.Query().Get("tenant_id")
	var entries []AuditEntry
	if tenantID != "" {
		entries = audit.GetEntriesByTenant(tenantID)
	} else {
		entries = audit.GetRecentEntries(limit)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"audit_log":   entries,
		"audit_count": len(entries),
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func parseKinds(s string) []MemoryKind {
	if s == "" {
		return nil
	}
	var out []MemoryKind
	for _, k := range split(s, ",") {
		out = append(out, MemoryKind(k))
	}
	return out
}

func split(s, sep string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if string(r) == sep {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func atoi(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}
