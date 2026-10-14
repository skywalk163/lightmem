// lightmem 端到端集成测试套件（R9-B）——全生命周期用例。
//
// 覆盖：save -> search -> recall -> update -> forget -> GDPR export -> cascade-delete，
// 以及多租户隔离 / 多用户可见性硬过滤 / HTTP 层重启数据存活 / 并发安全。
//
// 全部用例经真实 net/http（httptest.Server）打 kernel.APIServer.Handler()，
// 验证路由 -> 中间件 -> handler -> 存储后端全链路的序列化与装配正确性。
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// TestSaveAndRecall 写入一条记忆，recall 能召回且 fact 文本匹配。
func TestSaveAndRecall(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-sr-1", "t1", "u1", "tenant", "客户A偏好周一上午开周会")

	ids := e.recallHit(t, "客户A 偏好", "t1", "u1")
	mustHasID(t, ids, "e2e-sr-1")
}

// TestSearchEndpoint GET /search 返回正确 JSON 结构（数组，每项含 memory_id/fact）。
func TestSearchEndpoint(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-se-1", "t1", "u1", "tenant", "财务部门使用 ERP 系统记账")

	arr := e.searchText(t, "ERP", "t1", "u1")
	if len(arr) == 0 {
		t.Fatalf("search ERP: expected at least one hit, got empty")
	}
	found := false
	for _, r := range arr {
		mid, _ := r["memory_id"].(string)
		fact, _ := r["fact"].(string)
		if mid == "e2e-se-1" {
			found = true
			if !containsSubstr(fact, "ERP") {
				t.Fatalf("search result fact=%q should contain ERP", fact)
			}
		}
	}
	if !found {
		t.Fatalf("search ERP: e2e-se-1 not in %v", arr)
	}
}

// TestUpdateAppendsVersion update 追加新版本，旧版本标记 superseded（不再被 recall），
// 新版本 fact 生效且可被召回。
func TestUpdateAppendsVersion(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-up-1", "t1", "u1", "tenant", "原事实：客户A偏好邮件沟通")

	body := map[string]any{
		"memory_id": "e2e-up-1",
		"fact":      "新事实：客户A偏好即时消息沟通",
		"viewer":    "u1",
		"user_id":   "u1",
		"tenant_id": "t1",
	}
	status, raw := e.doJSON(t, http.MethodPost, "/tools/memory/update", body)
	if status != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", status, raw)
	}
	var res struct {
		Status     string `json:"status"`
		Superseded string `json:"superseded"`
		Memory     struct {
			MemoryID string `json:"memory_id"`
			Fact     string `json:"fact"`
			Version  int    `json:"version"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal update: %v: %s", err, raw)
	}
	if res.Status != "updated" {
		t.Fatalf("want status=updated, got %s", res.Status)
	}
	if res.Superseded != "e2e-up-1" {
		t.Fatalf("want superseded=e2e-up-1, got %s", res.Superseded)
	}
	if res.Memory.Fact != "新事实：客户A偏好即时消息沟通" {
		t.Fatalf("new fact mismatch: %s", res.Memory.Fact)
	}
	if res.Memory.Version < 1 {
		t.Fatalf("new version should be >=1, got %d", res.Memory.Version)
	}
	if res.Memory.MemoryID == "e2e-up-1" {
		t.Fatalf("update should mint a new memory_id, got same id")
	}

	// 新版本可被召回，旧版本被 superseded 过滤。
	ids := e.recallHit(t, "即时消息", "t1", "u1")
	mustHasID(t, ids, res.Memory.MemoryID)
	mustNotHasID(t, ids, "e2e-up-1")
}

// TestForgetDeletesMemory forget（软删除置 superseded_by）后 recall 不再召回。
func TestForgetDeletesMemory(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-fg-1", "t1", "u1", "tenant", "临时草稿记忆待删除")

	if ids := e.recallHit(t, "临时草稿", "t1", "u1"); !containsID(ids, "e2e-fg-1") {
		t.Fatalf("pre-forget: expected e2e-fg-1 in %v", ids)
	}

	body := map[string]any{
		"memory_id": "e2e-fg-1",
		"reason":    "用户请求删除",
		"viewer":    "u1",
		"user_id":   "u1",
		"tenant_id": "t1",
	}
	status, raw := e.doJSON(t, http.MethodPost, "/tools/memory/forget", body)
	if status != http.StatusOK {
		t.Fatalf("forget: want 200, got %d: %s", status, raw)
	}

	ids := e.recallHit(t, "临时草稿", "t1", "u1")
	mustNotHasID(t, ids, "e2e-fg-1")
}

// TestGDPRExport GDPR export 返回该 (tenant,user) 作用域全部记忆，不含其他用户。
func TestGDPRExport(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-ex-1", "t1", "u1", "tenant", "导出记忆甲")
	e.saveFact(t, "e2e-ex-2", "t1", "u1", "tenant", "导出记忆乙")
	e.saveFact(t, "e2e-ex-3", "t1", "u2", "tenant", "另一用户不导出")

	body := map[string]any{"tenant_id": "t1", "user_id": "u1"}
	status, raw := e.doJSON(t, http.MethodPost, "/api/v1/gdpr/export", body)
	if status != http.StatusOK {
		t.Fatalf("gdpr export: want 200, got %d: %s", status, raw)
	}
	var res struct {
		Exported int `json:"exported"`
		Memories []struct {
			MemoryID string `json:"memory_id"`
		} `json:"memories"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal export: %v: %s", err, raw)
	}
	if res.Exported != 2 {
		t.Fatalf("want exported=2, got %d", res.Exported)
	}
	got := map[string]bool{}
	for _, m := range res.Memories {
		got[m.MemoryID] = true
	}
	if !got["e2e-ex-1"] || !got["e2e-ex-2"] {
		t.Fatalf("export should contain ex-1/ex-2, got %v", got)
	}
	if got["e2e-ex-3"] {
		t.Fatalf("export must NOT contain other-user memory ex-3")
	}
}

// TestCascadeDelete 级联删除后该 (tenant,user) 作用域记忆全部消失。
func TestCascadeDelete(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-cd-1", "t1", "u1", "tenant", "级联删除测试甲")
	e.saveFact(t, "e2e-cd-2", "t1", "u1", "tenant", "级联删除测试乙")

	body := map[string]any{"tenant_id": "t1", "user_id": "u1", "actor": "e2e"}
	status, raw := e.doJSON(t, http.MethodPost, "/api/v1/gdpr/cascade-delete", body)
	if status != http.StatusOK {
		t.Fatalf("cascade-delete: want 200, got %d: %s", status, raw)
	}
	var res struct {
		DeletedCount int      `json:"deleted_count"`
		DeletedIDs   []string `json:"deleted_memory_ids"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal cascade: %v: %s", err, raw)
	}
	if res.DeletedCount != 2 {
		t.Fatalf("want deleted_count=2, got %d (%v)", res.DeletedCount, res.DeletedIDs)
	}

	// 删除后 recall 不应命中任何已删记忆。
	ids := e.recallHit(t, "级联删除", "t1", "u1")
	mustNotHasID(t, ids, "e2e-cd-1")
	mustNotHasID(t, ids, "e2e-cd-2")
}

// TestTenantIsolation 多租户硬隔离：tenant-A 看不到 tenant-B 的记忆。
func TestTenantIsolation(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-ti-1", "tenant-alpha", "u1", "tenant", "Alpha 租户机密项目")

	// 跨租户 /search 硬过滤。
	arr := e.searchText(t, "Alpha", "tenant-beta", "u1")
	if len(arr) != 0 {
		t.Fatalf("cross-tenant search should be empty, got %v", arr)
	}

	// 跨租户 recall 硬过滤。
	ids := e.recallHit(t, "Alpha 租户机密", "tenant-beta", "u1")
	mustNotHasID(t, ids, "e2e-ti-1")

	// 同租户可见（对照）。
	ids = e.recallHit(t, "Alpha 租户机密", "tenant-alpha", "u1")
	mustHasID(t, ids, "e2e-ti-1")
}

// TestUserVisibility 私有记忆仅 owner 可见，同租户其他用户被硬过滤。
func TestUserVisibility(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-uv-1", "t1", "alice", "private", "alice 的私密日记内容")

	// owner 可见。
	ids := e.recallHit(t, "私密日记", "t1", "alice")
	mustHasID(t, ids, "e2e-uv-1")

	// 同租户其他用户不可见。
	ids = e.recallHit(t, "私密日记", "t1", "bob")
	mustNotHasID(t, ids, "e2e-uv-1")
}

// TestPersistenceAcrossRestart HTTP 层重启（同一后端实例重建 httptest.Server）后数据仍在。
//
// 已知边界（登记不修复）：当前 SQLite 骨架后端为纯内存 map 实现（backend_sqlite.go），
// Close() 为 no-op，无磁盘持久化；本用例验证「HTTP 服务重建不丢后端状态」，
// 真正的进程重启持久化待 R10 接入真实 sqlite driver 后补测。
func TestPersistenceAcrossRestart(t *testing.T) {
	e := newE2EEnv(t)
	e.saveFact(t, "e2e-pr-1", "t1", "u1", "tenant", "重启后仍然存在的记忆")

	// 关闭旧 HTTP server（不关闭 backend），用同一 backend 重建服务。
	e.ts.Close()
	ts2 := newTestServerFromHandler(t, e.srv.Handler())
	defer ts2.Close()
	e.ts = ts2
	e.client = ts2.Client()

	ids := e.recallHit(t, "重启后仍然存在", "t1", "u1")
	mustHasID(t, ids, "e2e-pr-1")
}

// TestConcurrentSaveRecall 并发 save + recall，-race 无数据竞争。
func TestConcurrentSaveRecall(t *testing.T) {
	e := newE2EEnv(t)

	const n = 10
	var wg sync.WaitGroup
	// 并发写入：每个 goroutine 写固定 id，无共享可变状态。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("e2e-cc-%d", i)
			e.saveFact(t, id, "t1", "u1", "tenant", fmt.Sprintf("并发写入的记忆编号%d", i))
		}(i)
	}
	// 并发检索：与写入穿插，验证读写并发安全。
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				e.recallHit(t, "并发写入", "t1", "u1")
			}
		}()
	}
	wg.Wait()

	// 全部落库且可被一次干净 recall 召回。
	ids := e.recallHit(t, "并发写入的记忆", "t1", "u1")
	for i := 0; i < n; i++ {
		mustHasID(t, ids, fmt.Sprintf("e2e-cc-%d", i))
	}
}

// containsSubstr 简单子串判断（避免 import strings 仅为此一处）。
func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
