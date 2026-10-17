// R21-C 工作流记忆工具面端到端单测（计划/步骤/总结/进度/列计划）。
// 覆盖任务书 §五 C 线验收项：G3/G5/G6 清零、全链路闭环。
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newWorkflowTestEnv 装配 SQLite 后端的 APIServer。
func newWorkflowTestEnv(t *testing.T) (*APIServer, StorageBackend) {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	srv.SetRankFusion(DefaultRankFusion())
	return srv, b
}

// doWorkflowJSON 发一次 POST/GET 到 APIServer。
func doWorkflowJSON(srv *APIServer, method, path string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// ------------------------------------------------------------------
// 1. 建计划（含 goal_detail）成功
// ------------------------------------------------------------------
func TestGoalCreateSuccess(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "完成 R21 并行任务",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞其他线", "门禁全绿"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
			"deadline":    "2026-10-17",
			"progress":    0.0,
		},
		"visibility":      "tenant",
		"tenant_id":       "dshteam",
		"user_id":         "skywalk",
		"idempotency_key": "goal-r21-001",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("goal create: want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Status string `json:"status"`
		Memory struct {
			MemoryID   string `json:"memory_id"`
			TenantID   string `json:"tenant_id"`
			UserID     string `json:"user_id"`
			Visibility string `json:"visibility"`
			MemoryKind string `json:"memory_kind"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != "created" {
		t.Fatalf("want status=created, got %s", res.Status)
	}
	if res.Memory.TenantID != "dshteam" {
		t.Fatalf("want tenant=dshteam, got %s", res.Memory.TenantID)
	}
	if res.Memory.Visibility != "tenant" {
		t.Fatalf("want visibility=tenant, got %s", res.Memory.Visibility)
	}
	if res.Memory.MemoryKind != "goal" {
		t.Fatalf("want kind=goal, got %s", res.Memory.MemoryKind)
	}
}

// ------------------------------------------------------------------
// 2. 缺 goal_detail 422（G3 清零）
// ------------------------------------------------------------------
func TestGoalCreateMissingGoalDetail(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact":      "完成 R21 并行任务",
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("goal create no goal_detail: want 422, got %d: %s", w.Code, w.Body.String())
	}
}

// ------------------------------------------------------------------
// 3. 缺 constraints 422（GoalConstraintsPolicy 拦截）
// ------------------------------------------------------------------
func TestGoalCreateMissingConstraints(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "完成 R21 并行任务",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
			"deadline":    "2026-10-17",
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("goal create empty constraints: want 422, got %d: %s", w.Code, w.Body.String())
	}
}

// ------------------------------------------------------------------
// 4. 缺 kpi.target 422（GoalConstraintsPolicy 拦截）
// ------------------------------------------------------------------
func TestGoalCreateMissingKpiTarget(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "完成 R21 并行任务",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(0), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("goal create zero kpi.target: want 422, got %d: %s", w.Code, w.Body.String())
	}
}

// ------------------------------------------------------------------
// 5. 推进度 + 状态机（active → completed）
// ------------------------------------------------------------------
func TestGoalProgressStateMachine(t *testing.T) {
	srv, b := newWorkflowTestEnv(t)
	// 建计划
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "完成 R21 并行任务",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	var created struct {
		Memory struct {
			MemoryID string `json:"memory_id"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	goalID := created.Memory.MemoryID

	// 推 50%
	w = doWorkflowJSON(srv, "POST", "/tools/memory/goal/progress", map[string]any{
		"memory_id": goalID,
		"progress":  0.5,
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("progress 50%%: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var p50 struct {
		Progress float64 `json:"progress"`
		State    string  `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p50)
	if p50.Progress != 0.5 {
		t.Fatalf("want progress=0.5, got %.2f", p50.Progress)
	}
	if p50.State != "active" {
		t.Fatalf("want state=active, got %s", p50.State)
	}

	// 推 100% → 自动转 completed
	w = doWorkflowJSON(srv, "POST", "/tools/memory/goal/progress", map[string]any{
		"memory_id": goalID,
		"progress":  1.0,
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("progress 100%%: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var p100 struct {
		Progress float64 `json:"progress"`
		State    string  `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p100)
	if p100.State != "completed" {
		t.Fatalf("want state=completed, got %s", p100.State)
	}

	// 验证落库
	m, _ := b.Get(context.Background(), goalID, "skywalk", "dshteam")
	if m.GoalDetail.Progress != 1.0 {
		t.Fatalf("want stored progress=1.0, got %.2f", m.GoalDetail.Progress)
	}
}

// ------------------------------------------------------------------
// 6. 非法状态转移被拒（completed → active 不合法）
// ------------------------------------------------------------------
func TestGoalInvalidStateTransition(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "完成 R21 并行任务",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	var created struct {
		Memory struct {
			MemoryID string `json:"memory_id"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	goalID := created.Memory.MemoryID

	// 先推到 completed
	_ = doWorkflowJSON(srv, "POST", "/tools/memory/goal/progress", map[string]any{
		"memory_id": goalID,
		"progress":  1.0,
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})

	// 再尝试改回 active → 应被拒
	w = doWorkflowJSON(srv, "POST", "/tools/memory/goal/state", map[string]any{
		"memory_id": goalID,
		"state":     "active",
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid transition: want 422, got %d: %s", w.Code, w.Body.String())
	}
}

// ------------------------------------------------------------------
// 7. 列计划（G6 清零）
// ------------------------------------------------------------------
func TestPlanList(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	// 建 2 个计划
	for i := 0; i < 2; i++ {
		w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
			"fact": "计划 #" + string(rune('0'+i)),
			"goal_detail": map[string]any{
				"status":      "active",
				"constraints": []string{"不阻塞"},
				"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
			},
			"tenant_id": "dshteam",
			"user_id":   "skywalk",
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("goal create #%d: want 201, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// 列计划
	w := doWorkflowJSON(srv, "GET", "/tools/memory/plan?tenant_id=dshteam", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan list: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Count int `json:"count"`
		Plans []struct {
			MemoryID string `json:"memory_id"`
		} `json:"plans"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Count < 2 {
		t.Fatalf("want at least 2 plans, got %d", res.Count)
	}
}

// ------------------------------------------------------------------
// 8. 列计划按 status 过滤
// ------------------------------------------------------------------
func TestPlanListFilterByStatus(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	// 建一个 active + 一个 completed
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "active 计划",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create active: want 201, got %d: %s", w.Code, w.Body.String())
	}
	w = doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "completed 计划",
		"goal_detail": map[string]any{
			"status":      "completed",
			"constraints": []string{"已完成"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create completed: want 201, got %d: %s", w.Code, w.Body.String())
	}

	w = doWorkflowJSON(srv, "GET", "/tools/memory/plan?tenant_id=dshteam&status=active", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan list filter: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Count != 1 {
		t.Fatalf("want 1 active plan, got %d", res.Count)
	}
}

// ------------------------------------------------------------------
// 9. 记步骤（kind=procedure）
// ------------------------------------------------------------------
func TestStepAdd(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/step", map[string]any{
		"fact":       "完成 R21-C 编码",
		"project_id": "r21",
		"goal_id":    "",
		"seq":        1,
		"tenant_id":  "dshteam",
		"user_id":    "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("step add: want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Status   string `json:"status"`
		MemoryID string `json:"memory_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != "created" {
		t.Fatalf("want status=created, got %s", res.Status)
	}
}

// ------------------------------------------------------------------
// 10. 记总结（kind=event）
// ------------------------------------------------------------------
func TestSummaryAdd(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/summary", map[string]any{
		"fact":       "R21-C 全链路闭环通过",
		"project_id": "r21",
		"goal_id":    "",
		"tenant_id":  "dshteam",
		"user_id":    "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("summary add: want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Status   string `json:"status"`
		MemoryID string `json:"memory_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != "created" {
		t.Fatalf("want status=created, got %s", res.Status)
	}
}

// ------------------------------------------------------------------
// 11. 全链路闭环：建计划 → 记步骤 → 推 50% → 记总结 → 列计划 → recall
// ------------------------------------------------------------------
func TestWorkflowEndToEnd(t *testing.T) {
	srv, b := newWorkflowTestEnv(t)
	// 1) 建计划
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "R21-C 全链路测试",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"门禁全绿"},
			"kpi":         map[string]any{"metric": "测试通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	var g struct {
		Memory struct {
			MemoryID string `json:"memory_id"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	goalID := g.Memory.MemoryID

	// 2) 记步骤
	w = doWorkflowJSON(srv, "POST", "/tools/memory/step", map[string]any{
		"fact":       "完成 6 个 handler 实现",
		"project_id": "r21",
		"goal_id":    goalID,
		"seq":        1,
		"tenant_id":  "dshteam",
		"user_id":    "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("step add: want 201, got %d: %s", w.Code, w.Body.String())
	}

	// 3) 推 50%
	w = doWorkflowJSON(srv, "POST", "/tools/memory/goal/progress", map[string]any{
		"memory_id": goalID,
		"progress":  0.5,
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("progress: want 200, got %d: %s", w.Code, w.Body.String())
	}

	// 4) 记总结
	w = doWorkflowJSON(srv, "POST", "/tools/memory/summary", map[string]any{
		"fact":       "全链路测试通过",
		"project_id": "r21",
		"goal_id":    goalID,
		"tenant_id":  "dshteam",
		"user_id":    "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("summary add: want 201, got %d: %s", w.Code, w.Body.String())
	}

	// 5) 列计划，progress=0.5
	w = doWorkflowJSON(srv, "GET", "/tools/memory/plan?tenant_id=dshteam", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan list: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var pl struct {
		Plans []struct {
			MemoryID string `json:"memory_id"`
		} `json:"plans"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pl)
	found := false
	for _, p := range pl.Plans {
		if p.MemoryID == goalID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("goal not found in plan list")
	}

	// 6) recall 能召回步骤与总结
	w = doWorkflowJSON(srv, "POST", "/tools/memory/recall", map[string]any{
		"text":      "全链路测试",
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("recall: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var recall struct {
		Fused []struct {
			MemoryID   string `json:"memory_id"`
			MemoryKind string `json:"memory_kind"`
		} `json:"fused"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &recall)
	if len(recall.Fused) == 0 {
		t.Fatalf("recall should return results, got none")
	}

	// 7) 验证落库 progress=0.5
	m, _ := b.Get(context.Background(), goalID, "skywalk", "dshteam")
	if m.GoalDetail.Progress != 0.5 {
		t.Fatalf("want stored progress=0.5, got %.2f", m.GoalDetail.Progress)
	}
}

// ------------------------------------------------------------------
// 12. 多租户隔离（G5 清零：tenant=dshteam 建的计划，在别的 tenant 列不出）
// ------------------------------------------------------------------
func TestGoalTenantIsolation(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	// 在 dshteam 建计划
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "dshteam 计划",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create in dshteam: want 201, got %d: %s", w.Code, w.Body.String())
	}

	// 在 otherteam 列 → 应为空
	w = doWorkflowJSON(srv, "GET", "/tools/memory/plan?tenant_id=otherteam", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan list otherteam: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Count != 0 {
		t.Fatalf("want 0 plans in otherteam, got %d", res.Count)
	}
}

// ------------------------------------------------------------------
// 13. handleToolSave 扩展：goal_detail 透传不再 422
// ------------------------------------------------------------------
func TestToolSaveWithGoalDetail(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/save", map[string]any{
		"memory_id":       "g1",
		"idempotency_key": "k-g1",
		"tenant_id":       "dshteam",
		"user_id":         "skywalk",
		"memory_kind":     "goal",
		"fact":            "通过 save 端点建计划",
		"visibility":      "tenant",
		"permission":      map[string]interface{}{"owner": "skywalk"},
		"source":          "chat",
		"goal_detail": map[string]interface{}{
			"status":      "active",
			"constraints": []interface{}{"不阻塞"},
			"kpi":         map[string]interface{}{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("save with goal_detail: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ------------------------------------------------------------------
// 14. 默认 visibility = tenant（解 G7）
// ------------------------------------------------------------------
func TestToolSaveDefaultVisibilityTenant(t *testing.T) {
	srv, b := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/save", map[string]any{
		"memory_id":       "v1",
		"idempotency_key": "k-v1",
		"tenant_id":       "dshteam",
		"user_id":         "skywalk",
		"memory_kind":     "fact",
		"fact":            "测试默认 visibility",
		"permission":      map[string]interface{}{"owner": "skywalk"},
		"source":          "chat",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("save: want 201, got %d: %s", w.Code, w.Body.String())
	}
	m, _ := b.Get(context.Background(), "v1", "skywalk", "dshteam")
	if m.Visibility != VisTenant {
		t.Fatalf("want default visibility=tenant, got %s", m.Visibility)
	}
}

// ------------------------------------------------------------------
// 15. goal_detail 往返完整（写入后读回字段一致）
// ------------------------------------------------------------------
func TestGoalDetailRoundTrip(t *testing.T) {
	srv, b := newWorkflowTestEnv(t)
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "R21-C 往返测试",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"约束1", "约束2"},
			"kpi":         map[string]any{"metric": "通过率", "target": int64(100), "cadence": "per_run"},
			"deadline":    "2026-10-17",
			"progress":    0.0,
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	var created struct {
		Memory struct {
			MemoryID string `json:"memory_id"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	goalID := created.Memory.MemoryID

	m, _ := b.Get(context.Background(), goalID, "skywalk", "dshteam")
	if m.GoalDetail == nil {
		t.Fatalf("goal_detail should be stored")
	}
	if len(m.GoalDetail.Constraints) != 2 {
		t.Fatalf("want 2 constraints, got %d", len(m.GoalDetail.Constraints))
	}
	if m.GoalDetail.KPI.Target != 100 {
		t.Fatalf("want kpi.target=100, got %d", m.GoalDetail.KPI.Target)
	}
	if m.GoalDetail.Deadline != "2026-10-17" {
		t.Fatalf("want deadline=2026-10-17, got %s", m.GoalDetail.Deadline)
	}
}

// ------------------------------------------------------------------
// 16. 步骤写入带 goal_id 验证
// ------------------------------------------------------------------
func TestStepAddWithGoalID(t *testing.T) {
	srv, _ := newWorkflowTestEnv(t)
	// 先建 goal
	w := doWorkflowJSON(srv, "POST", "/tools/memory/goal", map[string]any{
		"fact": "测试 goal",
		"goal_detail": map[string]any{
			"status":      "active",
			"constraints": []string{"不阻塞"},
			"kpi":         map[string]any{"metric": "门禁通过率", "target": int64(100), "cadence": "per_run"},
		},
		"tenant_id": "dshteam",
		"user_id":   "skywalk",
	})
	var g struct {
		Memory struct {
			MemoryID string `json:"memory_id"`
		} `json:"memory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	goalID := g.Memory.MemoryID

	// 记步骤，带 goal_id
	w = doWorkflowJSON(srv, "POST", "/tools/memory/step", map[string]any{
		"fact":       "步骤 1",
		"project_id": "r21",
		"goal_id":    goalID,
		"seq":        1,
		"tenant_id":  "dshteam",
		"user_id":    "skywalk",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("step add with goal_id: want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		GoalID string `json:"goal_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.GoalID != goalID {
		t.Fatalf("want goal_id=%s, got %s", goalID, res.GoalID)
	}
}
