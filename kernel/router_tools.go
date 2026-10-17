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
	// 解 G3：goal_detail 必须透传（GoalConstraintsPolicy 会校验）
	// 解 G7：默认 visibility = tenant（共享大脑语义），可配，不硬改
	if m.Visibility == "" {
		m.Visibility = VisTenant
	}
	// R21 收口 · G4：写方身份强制——已认证时以 token 身份为准，忽略 body 的 user_id/tenant_id（防冒充）。
	if wid, fallback := ResolveActor(r.Context(), m.UserID, m.TenantID); wid.UserID != "" || wid.TenantID != "" {
		if wid.UserID != "" {
			m.UserID = wid.UserID
		}
		if wid.TenantID != "" {
			m.TenantID = wid.TenantID
		}
		_ = fallback // true=未认证回退 body（审计用）
	}
	// private 归属校验：owner 必须与 token 身份一致。
	if err := EnforcePrivateOwner(r.Context(), m.Permission.Owner, string(m.Visibility)); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
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
	// 审计日志
	GetAuditLogger().Log("save", saved.MemoryID, saved.TenantID, saved.UserID, actor, "memory saved")
	// 指标：记录层容量
	GetMetricsCollector().SetLayerCount(saved.Layer, 1)
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "memory": saved})
}

// ------------------------------------------------------------------
// R21-C 工作流记忆工具面：建计划 / 推进度 / 改状态 / 列计划 / 记步骤 / 记总结
// ------------------------------------------------------------------

// handleGoalCreate POST /tools/memory/goal
// 建计划（走 GoalConstraintsPolicy，goal_detail 必填校验已在 writepolicy.go）。
// 入参：fact, goal_detail{status,constraints[],kpi{metric,target,cadence},deadline,progress}, visibility(默认 tenant)
func (s *APIServer) handleGoalCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fact           string      `json:"fact"`
		GoalDetail     *GoalDetail `json:"goal_detail"`
		Visibility     Visibility  `json:"visibility"`
		TenantID       string      `json:"tenant_id"`
		UserID         string      `json:"user_id"`
		ProjectID      string      `json:"project_id,omitempty"`
		IdempotencyKey string      `json:"idempotency_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	if req.Fact == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fact required"})
		return
	}
	if req.GoalDetail == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "goal memory requires goal_detail"})
		return
	}
	// 默认 visibility = tenant（共享大脑语义，解 G7；可配，不硬改）
	vis := req.Visibility
	if vis == "" {
		vis = VisTenant
	}
	actor := req.UserID
	// R21 收口 · 来源归属：已认证时把 agent_id 一并落库（原实现只用 actor 当 Source，
	// 导致 goal/step/summary 无法区分是哪个 Agent 创建的）。未认证时保持原样。
	agentID := ""
	if id, ok := IdentityFromContext(r.Context()); ok {
		agentID = id.AgentID
	}
	src := actor
	if agentID != "" {
		src = agentID
	}
	m := &Memory{
		MemoryID:       newMemoryID(),
		IdempotencyKey: req.IdempotencyKey,
		TenantID:       req.TenantID,
		UserID:         actor,
		AgentID:        agentID,
		ProjectID:      req.ProjectID,
		Visibility:     vis,
		MemoryKind:     KindGoal,
		Fact:           req.Fact,
		Source:         src,
		RecordedAt:     time.Now().UTC(),
		Confidence:     0.9,
		Version:        1,
		Layer:          LayerL1,
		GoalDetail:     req.GoalDetail,
		Permission:     Permission{Owner: actor},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	saved, err := s.SaveMemory(ctx, m, actor)
	if errors.Is(err, ErrIdempotentNoop) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idempotent_noop", "memory": saved})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	GetAuditLogger().Log("goal_create", saved.MemoryID, saved.TenantID, saved.UserID, actor, req.Fact)
	GetMetricsCollector().SetLayerCount(saved.Layer, 1)
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "memory": saved})
}

// handleGoalProgress POST /tools/memory/goal/progress
// 推进度：走 GoalManager 状态机（CanTransition 校验非法转移）。
func (s *APIServer) handleGoalProgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID string  `json:"memory_id"`
		Progress float64 `json:"progress"`
		TenantID string  `json:"tenant_id"`
		UserID   string  `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.MemoryID == "" || req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id and tenant_id required"})
		return
	}
	actor := req.UserID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// 解 G5：tenant/viewer 由调用方传入，不再硬编码 "default"/actor
	// 注意：backend.Get 内部用 viewer 做可见性过滤，这里用 actor 作为 viewer
	existing, err := s.backend.Get(ctx, req.MemoryID, actor, req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if existing.MemoryKind != KindGoal || existing.GoalDetail == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "not a goal memory"})
		return
	}
	// 用 GoalManager 的 UpdateGoalProgress（内部走状态机）
	// 注意：UpdateGoalProgress 当前硬编码 tenant="default"，此处直接操作 GoalDetail 并走状态机校验
	oldState := GoalState(existing.GoalDetail.Status)
	newState := oldState
	if req.Progress >= 1.0 {
		newState = GoalCompleted
	} else if req.Progress < 0 {
		newState = GoalActive
	}
	if oldState != newState && !CanTransition(oldState, newState) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("invalid state transition: %s -> %s", oldState, newState),
		})
		return
	}
	existing.GoalDetail.Progress = req.Progress
	existing.GoalDetail.Status = string(newState)
	existing.RecordedAt = time.Now().UTC()
	if err := s.backend.Upsert(ctx, existing); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	GetAuditLogger().Log("goal_progress", existing.MemoryID, existing.TenantID, existing.UserID, actor,
		fmt.Sprintf("progress=%.1f state=%s->%s", req.Progress, oldState, newState))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "progress_updated",
		"memory_id": existing.MemoryID,
		"progress":  existing.GoalDetail.Progress,
		"state":     existing.GoalDetail.Status,
	})
}

// handleGoalState POST /tools/memory/goal/state
// 改状态：走 GoalManager 状态机（CanTransition 校验非法转移）。
//
// R21 收口 · D-2：任务书 §3.2 冻结入参名为 `status`，而早期实现读 `state`。
// 两者语义相同，此处**同时兼容**：`status` 优先、`state` 兜底，二者均缺失才 400。
// （此前只认 `state`，Agent 按冻结口径传 `status` 会 HTTP 200 但状态不变——静默失效。）
func (s *APIServer) handleGoalState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID string    `json:"memory_id"`
		Status   GoalState `json:"status"`
		State    GoalState `json:"state"`
		TenantID string    `json:"tenant_id"`
		UserID   string    `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.MemoryID == "" || req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory_id and tenant_id required"})
		return
	}
	// `status` 优先（冻结口径），`state` 作历史别名兜底。
	next := req.Status
	if next == "" {
		next = req.State
	}
	if next == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status required (accepts status|state)"})
		return
	}
	// R21 收口 · G4：已认证时 tenant/user 以 token 身份为准（防冒充）。
	if wid, _ := ResolveActor(r.Context(), req.UserID, req.TenantID); wid.UserID != "" || wid.TenantID != "" {
		if wid.TenantID != "" {
			req.TenantID = wid.TenantID
		}
		if wid.UserID != "" {
			req.UserID = wid.UserID
		}
	}
	actor := req.UserID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	existing, err := s.backend.Get(ctx, req.MemoryID, actor, req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if existing.MemoryKind != KindGoal || existing.GoalDetail == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "not a goal memory"})
		return
	}
	oldState := GoalState(existing.GoalDetail.Status)
	if !CanTransition(oldState, next) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("invalid state transition: %s -> %s", oldState, next),
		})
		return
	}
	existing.GoalDetail.Status = string(next)
	existing.RecordedAt = time.Now().UTC()
	if err := s.backend.Upsert(ctx, existing); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	GetAuditLogger().Log("goal_state", existing.MemoryID, existing.TenantID, existing.UserID, actor,
		fmt.Sprintf("%s->%s", oldState, next))
	writeJSON(w, http.StatusOK, map[string]any{
		"status_updated": true,
		"memory_id":      existing.MemoryID,
		"state":          existing.GoalDetail.Status,
	})
}

// handlePlanList GET /tools/memory/plan
// 列计划：用现有 /search 的 kind=goal 过滤实现，支持按 status 过滤与 top_k。
func (s *APIServer) handlePlanList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenant := q.Get("tenant_id")
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	statusFilter := q.Get("status")
	topK := atoi(q.Get("top_k"), 100)
	if topK <= 0 || topK > 1000 {
		topK = 100
	}
	params := QueryParams{
		TenantID:    tenant,
		Viewer:      q.Get("viewer"),
		MemoryKinds: []MemoryKind{KindGoal},
		TopK:        topK,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, err := s.backend.Query(ctx, params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if res == nil {
		res = []QueryResult{}
	}
	// R21 收口 · L-A1：QueryResult 只含 §4.4 检索字段，不含 goal_detail。
	// 为让 `plan_list` 能显示 progress/status（任务书 §九“列计划看到 progress”），
	// 在此按 memory_id 取回完整 Memory 并附带 goal_detail；
	// status 过滤亦以取回的 goal_detail.status 为准（避免二次查询）。
	type planItem struct {
		QueryResult
		GoalDetail *GoalDetail `json:"goal_detail,omitempty"`
	}
	plans := make([]planItem, 0, len(res))
	for _, item := range res {
		m, gerr := s.backend.Get(ctx, item.MemoryID, params.Viewer, tenant)
		if gerr != nil {
			// 取不回完整记忆（可见性/竞态）时跳过，避免泄露或 500。
			continue
		}
		if statusFilter != "" && (m.GoalDetail == nil || m.GoalDetail.Status != statusFilter) {
			continue
		}
		plans = append(plans, planItem{QueryResult: item, GoalDetail: m.GoalDetail})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenant,
		"count":     len(plans),
		"plans":     plans,
	})
}

// handleStepAdd POST /tools/memory/step
// 记步骤：kind=procedure，带 project_id、可选 goal_id、seq。
func (s *APIServer) handleStepAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fact      string `json:"fact"`
		ProjectID string `json:"project_id"`
		GoalID    string `json:"goal_id,omitempty"`
		Seq       int    `json:"seq,omitempty"`
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.TenantID == "" || req.ProjectID == "" || req.Fact == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id, project_id, and fact required"})
		return
	}
	actor := req.UserID
	// R21 收口 · 来源归属：已认证时落 agent_id 与 agent 作为 source（同 goal）。
	agentID := ""
	if id, ok := IdentityFromContext(r.Context()); ok {
		agentID = id.AgentID
	}
	src := actor
	if agentID != "" {
		src = agentID
	}
	m := &Memory{
		MemoryID:       newMemoryID(),
		IdempotencyKey: newMemoryID(),
		TenantID:       req.TenantID,
		UserID:         actor,
		AgentID:        agentID,
		ProjectID:      req.ProjectID,
		Visibility:     VisTenant,
		MemoryKind:     KindProcedure,
		Fact:           req.Fact,
		Source:         src,
		RecordedAt:     time.Now().UTC(),
		Confidence:     0.85,
		Version:        1,
		Layer:          LayerL1,
		Permission:     Permission{Owner: actor},
	}
	if req.GoalID != "" {
		// 验证 goal 存在且属于同 tenant
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, err := s.backend.Get(ctx, req.GoalID, actor, req.TenantID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "goal_id not found or not accessible"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	saved, err := s.SaveMemory(ctx, m, actor)
	if errors.Is(err, ErrIdempotentNoop) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idempotent_noop", "memory": saved})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	GetAuditLogger().Log("step_add", saved.MemoryID, saved.TenantID, saved.UserID, actor, req.Fact)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":    "created",
		"memory_id": saved.MemoryID,
		"seq":       req.Seq,
		"goal_id":   req.GoalID,
	})
}

// handleSummaryAdd POST /tools/memory/summary
// 记总结：kind=event，带 project_id、可选 goal_id。
func (s *APIServer) handleSummaryAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fact      string `json:"fact"`
		ProjectID string `json:"project_id"`
		GoalID    string `json:"goal_id,omitempty"`
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if req.TenantID == "" || req.ProjectID == "" || req.Fact == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id, project_id, and fact required"})
		return
	}
	actor := req.UserID
	// R21 收口 · 来源归属：已认证时落 agent_id 与 agent 作为 source（同 goal）。
	agentID := ""
	if id, ok := IdentityFromContext(r.Context()); ok {
		agentID = id.AgentID
	}
	src := actor
	if agentID != "" {
		src = agentID
	}
	m := &Memory{
		MemoryID:       newMemoryID(),
		IdempotencyKey: newMemoryID(),
		TenantID:       req.TenantID,
		UserID:         actor,
		AgentID:        agentID,
		ProjectID:      req.ProjectID,
		Visibility:     VisTenant,
		MemoryKind:     KindEvent,
		Fact:           req.Fact,
		Source:         src,
		RecordedAt:     time.Now().UTC(),
		Confidence:     0.9,
		Version:        1,
		Layer:          LayerL1,
		Permission:     Permission{Owner: actor},
	}
	if req.GoalID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, err := s.backend.Get(ctx, req.GoalID, actor, req.TenantID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "goal_id not found or not accessible"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	saved, err := s.SaveMemory(ctx, m, actor)
	if errors.Is(err, ErrIdempotentNoop) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idempotent_noop", "memory": saved})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	GetAuditLogger().Log("summary_add", saved.MemoryID, saved.TenantID, saved.UserID, actor, req.Fact)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":    "created",
		"memory_id": saved.MemoryID,
		"goal_id":   req.GoalID,
	})
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
	// 审计日志
	GetAuditLogger().Log("update", nm.MemoryID, nm.TenantID, nm.UserID, viewer, "memory updated")
	// 指标：记录生命周期操作
	GetMetricsCollector().RecordLifecycleStep("update")
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
		// FeedbackLoop：记录到 rejectedIDs，后续检索时过滤（R10-A：加锁写）。
		s.addRejectedID(m.MemoryID)
	}
	if err := s.backend.Upsert(ctx, m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 审计日志
	GetAuditLogger().Log("feedback", m.MemoryID, m.TenantID, m.UserID, viewer, "feedback: "+string(req.Status))
	// 指标：记录反馈统计
	GetMetricsCollector().RecordFeedback(req.Status == FbAdopted, req.Status == FbRejected)
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
	// 审计日志
	GetAuditLogger().Log("forget", m.MemoryID, m.TenantID, m.UserID, viewer, req.Reason)
	// 指标：记录生命周期操作
	GetMetricsCollector().RecordLifecycleStep("forget")
	// 审计日志（features.audit 在四档恒为 true，§7.2）
	log.Printf("[audit] forget memory_id=%s tenant=%s viewer=%s reason=%q at=%s",
		m.MemoryID, req.TenantID, viewer, req.Reason, time.Now().UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"status": "forgotten", "memory_id": m.MemoryID, "soft_delete": true})
}
