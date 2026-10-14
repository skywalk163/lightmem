// lightmem 端到端集成测试套件（R9-B）。
//
// 测试方式：httptest.NewServer 挂 kernel.APIServer.Handler()，内存 SQLite 骨架后端 +
// 确定性 hash embedder；全部走真实 net/http 序列化/路由/反序列化链路，不直接调 handler 函数。
//
// 红线遵守：
//   - 纯新增测试包，不改动 kernel/ 任何生产代码；
//   - 内存 SQLite + httptest，不依赖真实 PG/Redis/网络；
//   - 不调 LLM / 外部 embedder（hash embedder 占位）；
//   - 确定性：固定 memory_id / tenant / user，不依赖 wall clock 断言。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lightmem/kernel"
)

// e2eEnv 持有一个跑起来的 httptest.Server 及其装配依赖。
type e2eEnv struct {
	srv     *kernel.APIServer
	backend kernel.StorageBackend
	ts      *httptest.Server
	client  *http.Client
}

// newE2EEnv 装配一个 Solo 档内存 SQLite 后端 + hash embedder 的真实 HTTP 服务。
// 每个用例独立环境，互不污染。
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	p := &kernel.Profile{
		Profile:        kernel.TierSolo,
		StorageBackend: kernel.BackendSQLite,
		Storage: &kernel.StorageConfig{
			SQLite: &kernel.SQLiteConfig{Path: ":memory:"},
		},
	}
	ctx := context.Background()
	backend, err := kernel.NewStorageBackend(ctx, p)
	if err != nil {
		t.Fatalf("NewStorageBackend: %v", err)
	}
	srv := kernel.NewAPIServer(p, backend)
	// 确定性 hash embedder（与生产 main.go 一致）。
	srv.SetEmbedder(kernel.HashEmbedder(256))
	// 注意：此处**不**注入共享 *RankFusion。生产 main.go 调 SetRankFusion(DefaultRankFusion())
	// 会让所有请求共享同一 fusion 实例，而 RecallAndFuse 每次请求会写 rf.Now/AsOf/QueryFrame
	// （kernel/router_mem.go:255-259），并发 recall 时存在数据竞争（已登记为生产代码已知 bug，
	// 见 eval/reports/r9b_e2e_20261014.md §已知限制）。走 s.fusion==nil 路径时 RecallAndFuse
	// 每次请求局部新建 fusion（router_mem.go:250-252），功能等价且并发安全。

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = backend.Close()
	})
	return &e2eEnv{
		srv:     srv,
		backend: backend,
		ts:      ts,
		client:  ts.Client(),
	}
}

// ---- HTTP 原语 ----

// doJSON 发一个 JSON 请求，返回 (status, rawBody)。body 为 nil 时不发 body。
func (e *e2eEnv) doJSON(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body for %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// ---- 领域便捷方法（保持用例体精简）----

// saveFact 走 POST /tools/memory/save 写入一条 fact 记忆；显式固定 memory_id 保证确定性。
func (e *e2eEnv) saveFact(t *testing.T, id, tenant, user, vis, fact string) {
	t.Helper()
	body := map[string]any{
		"memory_id":       id,
		"idempotency_key": id,
		"tenant_id":       tenant,
		"user_id":         user,
		"memory_kind":     string(kernel.KindFact),
		"fact":            fact,
		"confidence":      0.9,
		"visibility":      vis,
		"permission":      map[string]any{"owner": user},
		"source":          "e2e",
	}
	status, raw := e.doJSON(t, http.MethodPost, "/tools/memory/save", body)
	if status != http.StatusCreated {
		t.Fatalf("save %s: want 201, got %d: %s", id, status, raw)
	}
}

// recallHit 走 POST /tools/memory/recall，返回 fused 结果里的 memory_id 列表（按排名）。
func (e *e2eEnv) recallHit(t *testing.T, text, tenant, user string) []string {
	t.Helper()
	body := map[string]any{
		"text":      text,
		"tenant_id": tenant,
		"user_id":   user,
		"top_k":     100, // E2E 需拿到全部候选，避免默认 topK 截断断言
	}
	status, raw := e.doJSON(t, http.MethodPost, "/tools/memory/recall", body)
	if status != http.StatusOK {
		t.Fatalf("recall %q: want 200, got %d: %s", text, status, raw)
	}
	var res struct {
		Fused []struct {
			MemoryID string `json:"memory_id"`
			Fact     string `json:"fact"`
		} `json:"fused"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal recall body: %v: %s", err, raw)
	}
	ids := make([]string, 0, len(res.Fused))
	for _, f := range res.Fused {
		ids = append(ids, f.MemoryID)
	}
	return ids
}

// newTestServerFromHandler 用已有 handler 起一个新的 httptest.Server（用于重启测试重建 HTTP 层）。
func newTestServerFromHandler(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}

// containsID 判断列表里是否含指定 id。
func containsID(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// mustHasID 断言 id 在 ids 列表里。
func mustHasID(t *testing.T, ids []string, id string) {
	t.Helper()
	if !containsID(ids, id) {
		t.Fatalf("expected fused to contain %s, got %v", id, ids)
	}
}

// mustNotHasID 断言 id 不在 ids 列表里。
func mustNotHasID(t *testing.T, ids []string, id string) {
	t.Helper()
	if containsID(ids, id) {
		t.Fatalf("expected fused NOT to contain %s, got %v", id, ids)
	}
}

// searchText 走 GET /search，返回原始结果数组（每项含 memory_id/fact）。
func (e *e2eEnv) searchText(t *testing.T, q, tenant, viewer string) []map[string]any {
	t.Helper()
	path := fmt.Sprintf("/search?q=%s&tenant_id=%s&viewer=%s", q, tenant, viewer)
	status, raw := e.doJSON(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("search %q: want 200, got %d: %s", q, status, raw)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("unmarshal search body: %v: %s", err, raw)
	}
	return arr
}
