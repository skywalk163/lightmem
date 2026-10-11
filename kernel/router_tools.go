// Agent 记忆工具接口（§4.4「Agent 记忆工具」）
// 真源：lightmem.md §4.4、§4.1 只增原则、§3.11 反馈闭环、§7.3 删除权（本轮软删除骨架）。
//
// 端点（面向 Agent 主动调用）：
//
//	POST /tools/memory/recall   跨层召回 + 融合（走 A1+A2）
//	POST /tools/memory/save     保存一条记忆（走 WritePolicy + 幂等）
//	POST /tools/memory/update   编辑已有记忆（追加新版本，不覆盖 fact）
//	POST /tools/memory/feedback 记录 adopted/rejected/edited 反馈
//	POST /tools/memory/forget   软删除（置 is_historical + superseded_by）
//
// 约束：所有端点走只增不覆盖；update 追加 version_history；forget 先做软删除 + 审计日志（硬级联留 P7）。
package kernel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// newMemoryID 生成一个近似 ULID 的单调标识（时间前缀 + 随机后缀）。
func newMemoryID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("%013x%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b))
}

// SaveMemory 走 WritePolicy 注册表 + 幂等，然后写入后端（写入的唯一正规入口）。
// 返回 (m, nil) 写入成功；(existing, ErrIdempotentNoop) 幂等命中；(nil, err) 被策略拒绝或写失败。
func (s *APIServer) SaveMemory(ctx context.Context, m *Memory, actor string) (*Memory, error) {
	if m == nil {
		return nil, errors.New("nil memory")
	}
	if m.MemoryID == "" {
		m.MemoryID = newMemoryID()
	}
	var existing *Memory
	if m.IdempotencyKey != "" {
		if lk, ok := s.backend.(IdempotencyLookup); ok {
			if ex, err := lk.FindByIdempotencyKey(ctx, m.TenantID, m.UserID, m.IdempotencyKey); err == nil && ex != nil {
				existing = ex
			}
		}
	}
	meta := WriteMeta{Actor: actor, Now: time.Now().UTC(), Existing: existing}
	reg := s.policy
	if reg == nil {
		reg = NewDefaultWritePolicyRegistry()
	}
	if err := reg.ValidateAll(ctx, m, meta); err != nil {
		if errors.Is(err, ErrIdempotentNoop) {
			return existing, ErrIdempotentNoop
		}
		return nil, err
	}
	if m.RecordedAt.IsZero() {
		m.RecordedAt = time.Now().UTC()
	}
	if m.Layer == "" {
		m.Layer = ClassifyLayer(m)
	}
	if err := s.backend.Upsert(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ------------------------------------------------------------------
// POST /tools/memory/recall
// ------------------------------------------------------------------

func (s *APIServer) handleToolRecall(w http.ResponseWriter, r *http.Request) {
	var rc RouterContext
	if err := json.NewDecoder(r.Body).Decode(&rc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if rc.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.RecallAndFuse(ctx, rc)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------------
// POST /tools/memory/save
// ------------------------------------------------------------------

func (s *APIServer) handleToolSave(w http.ResponseWriter, r *http.Request) {
	var m Memory
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if m.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	actor := m.UserID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	saved, err := s.SaveMemory(ctx, &m, actor)
	if errors.Is(err, ErrIdempotentNoop) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idempotent_noop", "memory": saved})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "memory": saved})
}

// ------------------------------------------------------------------
// POST /tools/memory/update
// ------------------------------------------------------------------

func (s *APIServer) handleToolUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID   string  `json:"memory_id"`
		Fact       string  `json:"fact"`
		Viewer     string  `json:"viewer"`
		UserID     string  `json:"user_id"`
		TenantID   string  `json:"tenant_id"`
		Confidence float64 `json:"confidence,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.MemoryID == "" || req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id and tenant_id required"})
		return
	}
	viewer := req.Viewer
	if viewer == "" {
		viewer = req.UserID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	old, err := s.backend.Get(ctx, req.MemoryID, viewer, req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// 追加新版本，不物理覆盖：新 memory_id + version_history 记录旧版本号。
	newID := newMemoryID()
	nm := *old
	nm.MemoryID = newID
	nm.IdempotencyKey = newID
	nm.Fact = req.Fact
	nm.Version = old.Version + 1
	hist := make([]int, 0, len(old.VersionHistory)+1)
	hist = append(hist, old.VersionHistory...)
	hist = append(hist, old.Version)
	nm.VersionHistory = hist
	nm.RecordedAt = time.Now().UTC()
	nm.ValidFrom = time.Now().UTC().Format("2006-01-02")
	nm.SupersededBy = nil
	nm.IsHistorical = false
	if req.Confidence > 0 {
		nm.Confidence = req.Confidence
	}
	if _, err := s.SaveMemory(ctx, &nm, viewer); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	// 旧版本标记被取代（不物理删除）。
	sup := newID
	old.SupersededBy = &sup
	old.ValidUntil = time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if err := s.backend.Upsert(ctx, old); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "superseded": old.MemoryID, "memory": nm})
}

// ------------------------------------------------------------------
// POST /tools/memory/feedback
// ------------------------------------------------------------------

func (s *APIServer) handleToolFeedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID string         `json:"memory_id"`
		Status   FeedbackStatus `json:"status"`
		Operator string         `json:"operator"`
		Viewer   string         `json:"viewer"`
		UserID   string         `json:"user_id"`
		TenantID string         `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.MemoryID == "" || req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id and tenant_id required"})
		return
	}
	switch req.Status {
	case FbAdopted, FbRejected, FbEdited:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be adopted|rejected|edited"})
		return
	}
	viewer := req.Viewer
	if viewer == "" {
		viewer = req.UserID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	m, err := s.backend.Get(ctx, req.MemoryID, viewer, req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	rec := FeedbackRecord{Status: req.Status, Time: time.Now().UTC(), Operator: req.Operator}
	if rec.Operator == "" {
		rec.Operator = viewer
	}
	m.FeedbackRecords = append(m.FeedbackRecords, rec)
	// §3.11 反馈闭环回写：rejected 降权并标记修正候选（整链修正在 P3 展开）。
	if req.Status == FbRejected {
		m.Confidence = clamp01(m.Confidence - 0.1)
		log.Printf("[feedback] correction-candidate memory_id=%s rejected by %s", m.MemoryID, rec.Operator)
		// FeedbackLoop：记录到 rejectedIDs，后续检索时过滤。
		s.rejectedIDs = append(s.rejectedIDs, m.MemoryID)
	}
	if err := s.backend.Upsert(ctx, m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded", "feedback": rec, "memory": m})
}

// ------------------------------------------------------------------
// POST /tools/memory/forget
// ------------------------------------------------------------------

func (s *APIServer) handleToolForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID string `json:"memory_id"`
		Reason   string `json:"reason"`
		Viewer   string `json:"viewer"`
		UserID   string `json:"user_id"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.MemoryID == "" || req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id and tenant_id required"})
		return
	}
	viewer := req.Viewer
	if viewer == "" {
		viewer = req.UserID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	m, err := s.backend.Get(ctx, req.MemoryID, viewer, req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// 软删除：置 is_historical + superseded_by（硬级联留 P7，§7.3 骨架）。
	marker := "forgotten"
	if req.Reason != "" {
		marker = "forgotten:" + req.Reason
	}
	m.IsHistorical = true
	m.SupersededBy = &marker
	if err := s.backend.Upsert(ctx, m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 审计日志（features.audit 在四档恒为 true，§7.2）
	log.Printf("[audit] forget memory_id=%s tenant=%s viewer=%s reason=%q at=%s",
		m.MemoryID, req.TenantID, viewer, req.Reason, time.Now().UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"status": "forgotten", "memory_id": m.MemoryID, "soft_delete": true})
}
