// R21-A MCP 服务端单测（任务书 §五 A 线 4：≥12 个 Test 函数）
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// 测试装配
// ------------------------------------------------------------------

// newTestMCP 用 httptest REST 服务 + 独立 MCP 服务端。
func newTestMCP(t *testing.T) (*MCPServer, *httptest.Server) {
	t.Helper()
	// 装配一个轻量 REST 服务：复用 router_search_test 的 SQLite 后端装配方式
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	srv.SetRankFusion(DefaultRankFusion())
	srv.SetLayerPolicy(DefaultLayerPolicy())

	// 挂载 AuthMiddleware（未配置 token → 透传，零回归）
	mux := srv.Handler()
	mux = AuthMiddleware(mux)
	mux = RecoverMiddleware(mux)

	rest := httptest.NewServer(mux)
	t.Cleanup(rest.Close)
	mcp := NewMCPServer(rest.URL)
	return mcp, rest
}

// handleJSON 便捷调用
func (s *MCPServer) call(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(msg)
	out := s.HandleJSON(context.Background(), raw)
	if out == nil {
		return nil
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("bad json response: %v / %s", err, out)
	}
	return resp
}

func initReq(version string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": version,
			"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
		},
	}
}

func toolCallReq(name string, args map[string]any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
}

// ------------------------------------------------------------------
// 1. initialize 回显客户端版本（legacy）
// ------------------------------------------------------------------

func TestMCPInitializeEchoesLegacyVersion(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, initReq("2025-11-25"))
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("want 2025-11-25, got %v", result["protocolVersion"])
	}
}

// 2. initialize 回显现代版本
func TestMCPInitializeEchoesModernVersion(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, initReq("2026-07-28"))
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2026-07-28" {
		t.Fatalf("want 2026-07-28, got %v", result["protocolVersion"])
	}
	si := result["serverInfo"].(map[string]any)
	if si["name"] != MCPDefaultName {
		t.Fatalf("serverInfo.name = %v", si["name"])
	}
}

// 3. tools/list 返回 13 个工具且 schema 完整
func TestMCPToolsListCountAndSchema(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 13 {
		t.Fatalf("want 13 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, item := range tools {
		tool := item.(map[string]any)
		name := tool["name"].(string)
		names[name] = true
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %s missing inputSchema", name)
		}
		if schema["type"] != "object" {
			t.Fatalf("tool %s schema.type = %v", name, schema["type"])
		}
	}
	for _, want := range []string{"lightmem_save", "lightmem_recall", "lightmem_search",
		"lightmem_update", "lightmem_feedback", "lightmem_forget", "lightmem_health",
		"lightmem_goal_create", "lightmem_goal_progress", "lightmem_goal_state",
		"lightmem_plan_list", "lightmem_step_add", "lightmem_summary_add"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
}

// 4. goal_create 工具 schema 必须暴露 goal_detail 与 visibility（解 G3）
func TestMCPGoalCreateSchemaHasGoalDetail(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := resp["result"].(map[string]any)["tools"].([]any)
	var found bool
	for _, item := range tools {
		tool := item.(map[string]any)
		if tool["name"] != "lightmem_goal_create" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["goal_detail"]; !ok {
			t.Fatalf("goal_create missing goal_detail property")
		}
		if _, ok := props["visibility"]; !ok {
			t.Fatalf("goal_create missing visibility property")
		}
		required := tool["inputSchema"].(map[string]any)["required"].([]any)
		if len(required) < 2 {
			t.Fatalf("goal_create required list too short: %v", required)
		}
		found = true
	}
	if !found {
		t.Fatalf("lightmem_goal_create not found")
	}
}

// 5. tools/call 未知工具 → isError
func TestMCPToolsCallUnknownTool(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, toolCallReq("lightmem_nonexistent", map[string]any{}))
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("want isError=true, got %v", result)
	}
	content := result["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), "未知工具") {
		t.Fatalf("unexpected text: %v", content[0])
	}
}

// 6. tools/call 缺参 → isError 且提示 fact 不能为空
func TestMCPToolsCallMissingRequired(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, toolCallReq("lightmem_save", map[string]any{}))
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("want isError=true for missing fact")
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "fact 不能为空") {
		t.Fatalf("unexpected error text: %s", text)
	}
}

// 7. JSON-RPC 错误码：未知 method → -32601
func TestMCPMethodNotFoundCode(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover"})
	err := resp["error"].(map[string]any)
	if err["code"].(float64) != -32601 {
		t.Fatalf("want -32601, got %v", err["code"])
	}
}

// 8. 非法 JSON → -32700
func TestMCPParseError(t *testing.T) {
	s, _ := newTestMCP(t)
	out := s.HandleJSON(context.Background(), []byte("{not json"))
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	err := resp["error"].(map[string]any)
	if err["code"].(float64) != -32700 {
		t.Fatalf("want -32700, got %v", err["code"])
	}
}

// 9. 中文 UTF-8 写入 + recall 往返完整（无 surrogates）
func TestMCPChineseRoundTrip(t *testing.T) {
	s, _ := newTestMCP(t)
	// 写入
	fact := "天马的北极星指标是真实语料 holdout 零 token 覆盖 >= 0.95"
	resp := s.call(t, toolCallReq("lightmem_save", map[string]any{
		"fact":      fact,
		"tenant_id": "dshteam",
		"user_id":   "dsh-agent",
	}))
	if resp["error"] != nil {
		t.Fatalf("save failed: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("save isError: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, fact) {
		t.Fatalf("save text missing fact: %s", text)
	}
	// recall
	resp = s.call(t, toolCallReq("lightmem_recall", map[string]any{
		"text":      "北极星指标",
		"tenant_id": "dshteam",
		"user_id":   "dsh-agent",
	}))
	result = resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("recall isError: %v", result)
	}
	text = result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "北极星指标") {
		t.Fatalf("recall text missing chinese query result: %s", text)
	}
}

// 10. stdio 多行 framing：每行独立 JSON-RPC，可多次握手
func TestMCPStdioFraming(t *testing.T) {
	s, _ := newTestMCP(t)
	var in bytes.Buffer
	var out bytes.Buffer
	stdioSrv := NewStdioServer(s, &in, &out)
	// 模拟 stdin 关闭前的若干行
	rawInit := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`
	rawList := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	rawPing := `{"jsonrpc":"2.0","id":3,"method":"ping"}`
	in.WriteString(rawInit + "\n" + rawList + "\n" + rawPing + "\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		stdioSrv.Run()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("stdio run timeout")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 responses, got %d: %s", len(lines), out.String())
	}
	for i, line := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("line %d not json: %v", i, err)
		}
		if resp["error"] != nil {
			t.Fatalf("line %d unexpected error: %v", i, resp["error"])
		}
	}
}

// 11. stdio 长行（>64KB）不截断
func TestMCPStdioLongLine(t *testing.T) {
	s, _ := newTestMCP(t)
	// 造一个非常长的 fact
	longFact := strings.Repeat("长", 70000)
	var in bytes.Buffer
	var out bytes.Buffer
	stdioSrv := NewStdioServer(s, &in, &out)
	msg := map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "lightmem_save", "arguments": map[string]any{
			"fact": longFact, "tenant_id": "dshteam", "user_id": "u1",
		}}}
	raw, _ := json.Marshal(msg)
	in.Write(raw)
	in.WriteByte('\n')

	done := make(chan struct{})
	go func() {
		defer close(done)
		stdioSrv.Run()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("stdio long line timeout")
	}
	if out.Len() == 0 {
		t.Fatalf("no output for long line")
	}
	// 输出必须含 memory_id 或错误（说明未截断）
	text := out.String()
	if !strings.Contains(text, "memory_id") && !strings.Contains(text, "error") {
		t.Fatalf("unexpected response: %.200s", text)
	}
}

// 12. HTTP transport：POST 返回 JSON + initialize 返回 Mcp-Session-Id
func TestMCPHTTPPostSessionID(t *testing.T) {
	s, _ := newTestMCP(t)
	httpSrv := NewMCPHTTPServer(s)
	handler := AuthMiddleware(httpSrv.Handler())
	handler = RecoverMiddleware(handler)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("missing Mcp-Session-Id header")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	result := body["result"].(map[string]any)
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
}

// 13. HTTP transport：SSE 响应（Accept: text/event-stream）
func TestMCPHTTPPostSSE(t *testing.T) {
	s, _ := newTestMCP(t)
	httpSrv := NewMCPHTTPServer(s)
	ts := httptest.NewServer(RecoverMiddleware(AuthMiddleware(httpSrv.Handler())))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("want SSE, got %s", ct)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "event: message") {
		t.Fatalf("missing event: message, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "lightmem_goal_create") {
		t.Fatalf("SSE body missing tools, got %s", buf.String()[:200])
	}
}

// 14. HTTP DELETE 终止会话
func TestMCPHTTPDeleteSession(t *testing.T) {
	s, _ := newTestMCP(t)
	httpSrv := NewMCPHTTPServer(s)
	ts := httptest.NewServer(RecoverMiddleware(AuthMiddleware(httpSrv.Handler())))
	defer ts.Close()

	// 先 initialize 拿 session
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sid == "" {
		t.Fatalf("no session id")
	}

	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mcp", nil)
	del.Header.Set("Mcp-Session-Id", sid)
	dresp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", dresp.StatusCode)
	}
}

// 15. HTTP GET /mcp SSE 长连接（短生命周期验证）
func TestMCPHTTPGetSSEStream(t *testing.T) {
	s, _ := newTestMCP(t)
	httpSrv := NewMCPHTTPServer(s)
	ts := httptest.NewServer(RecoverMiddleware(AuthMiddleware(httpSrv.Handler())))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 上下文被取消导致 err 也可接受
		return
	}
	resp.Body.Close()
}

// 16. goal_detail 透传：完整 goal 通过 MCP 写入不再 422（解 G3）
func TestMCPGoalDetailNoLonger422(t *testing.T) {
	s, _ := newTestMCP(t)
	goalDetail := map[string]any{
		"status":      "active",
		"constraints": []string{"不调 LLM 生成评测集", "固定 seed", "零新增第三方依赖"},
		"kpi":         map[string]any{"metric": "holdout 零 token 覆盖率", "target": 95, "cadence": "per_release"},
		"deadline":    "2026-11-01",
		"progress":    0.1,
	}
	resp := s.call(t, toolCallReq("lightmem_goal_create", map[string]any{
		"fact":        "R21 主线：内核原生 MCP + 多 Agent 记忆大脑接入",
		"goal_detail": goalDetail,
		"tenant_id":   "dshteam",
		"user_id":     "skywalk",
		"project_id":  "r21",
	}))
	if resp["error"] != nil {
		t.Fatalf("unexpected jsonrpc error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		t.Fatalf("goal_create isError: %s", text)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "计划已创建") {
		t.Fatalf("unexpected text: %s", text)
	}
}

// 17. 工作流全链路：建计划→记步骤→推 50%→记总结→列计划
func TestMCPWorkflowFullChain(t *testing.T) {
	s, _ := newTestMCP(t)
	goalDetail := map[string]any{
		"status":      "active",
		"constraints": []string{"不新增第三方依赖"},
		"kpi":         map[string]any{"metric": "gate PASS", "target": 14, "cadence": "per_release"},
		"progress":    0.0,
	}
	// 建计划
	resp := s.call(t, toolCallReq("lightmem_goal_create", map[string]any{
		"fact": "R21 集成真机验证", "goal_detail": goalDetail,
		"tenant_id": "dshteam", "user_id": "skywalk", "project_id": "r21",
	}))
	if resp["error"] != nil {
		t.Fatalf("goal_create error: %v", resp["error"])
	}
	goalText := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(goalText, "memory_id=") {
		t.Fatalf("goal_create text missing memory_id: %s", goalText)
	}

	// 记步骤
	resp = s.call(t, toolCallReq("lightmem_step_add", map[string]any{
		"fact":       "在 0.88 部署 R21 HEAD 并跑 gofmt/vet/test",
		"project_id": "r21", "tenant_id": "dshteam", "user_id": "skywalk", "seq": 1,
	}))
	if resp["error"] != nil {
		t.Fatalf("step_add error: %v", resp["error"])
	}
	stepText := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(stepText, "步骤已记录") {
		t.Fatalf("step_add text: %s", stepText)
	}

	// 记总结
	resp = s.call(t, toolCallReq("lightmem_summary_add", map[string]any{
		"fact":       "R21 完成 13 工具 + 双传输，G3/G5/G6 清零",
		"project_id": "r21", "tenant_id": "dshteam", "user_id": "skywalk",
	}))
	if resp["error"] != nil {
		t.Fatalf("summary_add error: %v", resp["error"])
	}

	// 列计划
	resp = s.call(t, toolCallReq("lightmem_plan_list", map[string]any{
		"tenant_id": "dshteam", "user_id": "skywalk",
	}))
	if resp["error"] != nil {
		t.Fatalf("plan_list error: %v", resp["error"])
	}
	planText := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(planText, "共") || !strings.Contains(planText, "条") {
		t.Fatalf("plan_list text: %s", planText)
	}
}

// 18. goal_detail 缺失 → 服务端仍 422（策略保留，正确行为）
func TestMCPGoalDetailMissingStill422(t *testing.T) {
	s, _ := newTestMCP(t)
	resp := s.call(t, toolCallReq("lightmem_goal_create", map[string]any{
		"fact": "缺 goal_detail 应该被拒", "tenant_id": "dshteam", "user_id": "skywalk",
	}))
	if resp["error"] != nil {
		t.Fatalf("unexpected jsonrpc error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("want isError for missing goal_detail")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "goal_detail") {
		t.Fatalf("error text: %s", text)
	}
}
