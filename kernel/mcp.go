// R21-A 内核原生 MCP 服务端（§五 A 线）
//
// 目标：让 lightmem 原生说 MCP，一个 HTTP 端点服务所有支持 HTTP 的 Agent，
// 同一二进制 stdio 模式服务只能吃 stdio 的 Agent。彻底去掉 Python 桥这一跳。
//
// 设计口径（任务书 §五 A 线 + §3.2 冻结表 + 红线⑦）：
//   - 纯 stdlib：net/http / encoding/json / bufio（零新增依赖，不改 go.mod/go.sum）；
//   - 方法：initialize（回显客户端 protocolVersion）/ notifications/initialized /
//     tools/list / tools/call / ping；server/discover 返回 -32601（DSH 会正常回退 legacy）；
//   - 错误码遵循 JSON-RPC：-32700 parse / -32601 method not found / -32602 invalid params；
//   - 工具业务错误用 isError:true 的 tool result（不走协议层错误）；
//   - 工具实现一律走 loopback HTTP 调 REST（§3.2 端点），不直接 import C 线新函数（解耦约定）；
//   - 13 个工具：7 既有（save/recall/search/update/feedback/forget/health）+
//     6 工作流（goal_create/goal_progress/goal_state/plan_list/step_add/summary_add）。
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MCP 服务端常量
const (
	MCPDefaultName    = "lightmem"
	MCPDefaultVersion = "0.2.0"
	MCPProtocolLegacy = "2025-11-25"
	MCPProtocolModern = "2026-07-28"
)

// MCPErrorCode JSON-RPC 标准错误码
type MCPErrorCode int

const (
	MCPErrParse    MCPErrorCode = -32700
	MCPErrInvalid  MCPErrorCode = -32602
	MCPErrMethod   MCPErrorCode = -32601
	MCPErrInternal MCPErrorCode = -32603
)

// ------------------------------------------------------------------
// MCP 消息结构（JSON-RPC 2.0）
// ------------------------------------------------------------------

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    MCPErrorCode `json:"code"`
	Message string       `json:"message"`
	Data    any          `json:"data,omitempty"`
}

// MCPContent MCP tool result 内容项
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPToolResult MCP tools/call 返回
type MCPToolResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// MCPTool MCP tool 定义（tools/list 返回项）
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpToolHandler 工具业务实现：入参 args（已注入身份默认值），返回文本或错误。
type mcpToolHandler func(ctx context.Context, args map[string]any) (string, error)

// mcpTool 内部注册项
type mcpTool struct {
	meta   MCPTool
	handle mcpToolHandler
}

// MCPServer MCP 协议核心（复用 APIServer 的 REST 面）
type MCPServer struct {
	baseURL   string // REST loopback 地址，如 http://127.0.0.1:18443
	timeout   time.Duration
	tools     []mcpTool
	serverVer string
	http      *http.Client
	// R21 T3：保留内部 REST 客户端引用，供 SetUpstreamCredentials 下发凭证/身份。
	rest *mcpRESTClient

	mu       sync.Mutex
	sessions map[string]time.Time
}

// ------------------------------------------------------------------
// REST loopback client
// ------------------------------------------------------------------

// mcpRESTClient 封装对 REST 端点的调用（A/C 解耦：A 线仅 HTTP 调用，不 import C 线函数）
type mcpRESTClient struct {
	baseURL string
	client  *http.Client
	// R21 T3（解 D-1）：stdio 模式无请求头，用启动配置的身份 + 凭证直连 REST。
	// token / identity 为空时行为与 R20 逐位一致（零回归）。
	token    string
	identity *Identity
}

func newMCPRESTClient(baseURL string, timeout time.Duration) *mcpRESTClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &mcpRESTClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// SetUpstreamCredentials 设置向 REST 上游调用时使用的凭证与（stdio 模式的）身份。
//
// R21 T3（解 D-1）：stdio 通道没有请求头，若 REST 不在回环上，服务端的
// identityFromLoopbackHeaders 不会信任 X-Lightmem-*，导致全部 401。
// 这里让部署层在**受信侧**配置凭证与身份：
//
//	token    —— 该 Agent 的 agent token（去 18443 鉴权用）
//	identity —— 该 Agent 的身份（user/tenant/agent），随 REST 调用透传
//
// 二者为空时行为与 R20 逐位一致（零回归）。
func (c *mcpRESTClient) SetUpstreamCredentials(token string, identity *Identity) {
	c.token = strings.TrimSpace(token)
	c.identity = identity
}

func (c *mcpRESTClient) do(ctx context.Context, method, path string, query url.Values, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	// R21 T3（解 D-1）：向 REST 上游带**凭证**。
	// stdio 模式下本进程在客户端机器上运行，REST 未必是回环（Windows→0.88 就不是），
	// 因此 X-Lightmem-* 头不会被服务端信任（见 auth.go identityFromLoopbackHeaders）。
	// 必须直接带 Authorization，才能让 18443 的鉴权通过。
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// 身份透传：若 MCP 请求已鉴权，把 token 身份带给 REST 端（防 loopback 侧再解析）
	if id, ok := IdentityFromContext(ctx); ok {
		if id.UserID != "" {
			req.Header.Set("X-Lightmem-User", id.UserID)
		}
		if id.TenantID != "" {
			req.Header.Set("X-Lightmem-Tenant", id.TenantID)
		}
		if id.AgentID != "" {
			req.Header.Set("X-Lightmem-Agent", id.AgentID)
		}
	} else if c.identity != nil {
		// R21 T3（解 D-1）：stdio 模式没有请求头 → 用启动时配置的身份。
		// 这样 CodeArts/Roo（只能吃 stdio）也能以**自己的 agent 身份**写入，
		// 而不是无身份/被拒。
		if c.identity.UserID != "" {
			req.Header.Set("X-Lightmem-User", c.identity.UserID)
		}
		if c.identity.TenantID != "" {
			req.Header.Set("X-Lightmem-Tenant", c.identity.TenantID)
		}
		if c.identity.AgentID != "" {
			req.Header.Set("X-Lightmem-Agent", c.identity.AgentID)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rest %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(raw)) == 0 {
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("rest %s %s -> HTTP %d: empty body", method, path, resp.StatusCode)
		}
		return map[string]any{}, nil
	}
	// /search 直接返回 JSON 数组；统一包裹成 {"items": [...]}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []any
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("rest %s %s -> HTTP %d: invalid json: %w", method, path, resp.StatusCode, err)
		}
		if resp.StatusCode >= 400 {
			return map[string]any{"items": arr}, fmt.Errorf("rest %s %s -> HTTP %d", method, path, resp.StatusCode)
		}
		return map[string]any{"items": arr}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, fmt.Errorf("rest %s %s -> HTTP %d: invalid json: %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		msg := ""
		if e, ok := out["error"]; ok {
			msg = fmt.Sprintf("%v", e)
		}
		if msg == "" {
			msg = string(bytes.TrimSpace(raw))
			if len(msg) > 300 {
				msg = msg[:300] + "..."
			}
		}
		return out, fmt.Errorf("rest %s %s -> HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return out, nil
}

// ------------------------------------------------------------------
// MCPServer 构造与装配
// ------------------------------------------------------------------

// NewMCPServer 创建 MCP 服务端。baseURL 为 REST loopback 地址（A 线不直接调 C 线函数）。
func NewMCPServer(baseURL string) *MCPServer {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18443"
	}
	s := &MCPServer{
		baseURL:   baseURL,
		timeout:   15 * time.Second,
		serverVer: MCPDefaultVersion,
		http:      &http.Client{Timeout: 15 * time.Second},
		sessions:  map[string]time.Time{},
	}
	rc := newMCPRESTClient(baseURL, s.timeout)
	s.rest = rc
	s.registerTools(rc)
	return s
}

// SetUpstreamCredentials 把凭证与 stdio 身份转交给内部 REST 客户端（R21 T3，解 D-1）。
func (s *MCPServer) SetUpstreamCredentials(token string, identity *Identity) {
	if s != nil && s.rest != nil {
		s.rest.SetUpstreamCredentials(token, identity)
	}
}

// SetServerVersion 覆盖 serverInfo.version（测试用）。
func (s *MCPServer) SetServerVersion(v string) { s.serverVer = v }

// Tools 返回当前注册的工具定义（供 tools/list 输出）。
func (s *MCPServer) Tools() []MCPTool {
	out := make([]MCPTool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.meta)
	}
	return out
}

func (s *MCPServer) registerTools(rc *mcpRESTClient) {
	s.tools = []mcpTool{
		{meta: toolSaveMeta(), handle: handlerSave(rc)},
		{meta: toolRecallMeta(), handle: handlerRecall(rc)},
		{meta: toolSearchMeta(), handle: handlerSearch(rc)},
		{meta: toolUpdateMeta(), handle: handlerUpdate(rc)},
		{meta: toolFeedbackMeta(), handle: handlerFeedback(rc)},
		{meta: toolForgetMeta(), handle: handlerForget(rc)},
		{meta: toolHealthMeta(), handle: handlerHealth(rc)},
		{meta: toolGoalCreateMeta(), handle: handlerGoalCreate(rc)},
		{meta: toolGoalProgressMeta(), handle: handlerGoalProgress(rc)},
		{meta: toolGoalStateMeta(), handle: handlerGoalState(rc)},
		{meta: toolPlanListMeta(), handle: handlerPlanList(rc)},
		{meta: toolStepAddMeta(), handle: handlerStepAdd(rc)},
		{meta: toolSummaryAddMeta(), handle: handlerSummaryAdd(rc)},
	}
}

// ------------------------------------------------------------------
// JSON-RPC 请求分发（stdio 与 HTTP 共用）
// ------------------------------------------------------------------

// HandleJSON 处理单条 JSON-RPC 请求；返回可写出的 JSON 字节（nil 表示无需回包，如通知）。
func (s *MCPServer) HandleJSON(ctx context.Context, raw []byte) []byte {
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return s.mustMarshal(mcpResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &mcpError{Code: MCPErrParse, Message: "parse error"},
		})
	}
	// 通知类：无 id 且非请求
	if len(req.ID) == 0 {
		s.handleNotification(ctx, &req)
		return nil
	}
	resp := s.dispatch(ctx, &req)
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	return s.mustMarshal(resp)
}

func (s *MCPServer) handleNotification(_ context.Context, req *mcpRequest) {
	switch req.Method {
	case "notifications/initialized", "initialized", "notifications/cancelled":
		// 静默处理
	case "ping":
		// ping 带 id 时走 dispatch；无 id 的通知式 ping 也静默
	default:
		// 未知通知方法：静默忽略
	}
}

func (s *MCPServer) dispatch(ctx context.Context, req *mcpRequest) mcpResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "ping":
		return mcpResponse{Result: map[string]any{}}
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return mcpResponse{
			Error: &mcpError{Code: MCPErrMethod, Message: "method not found: " + req.Method},
		}
	}
}

func (s *MCPServer) handleInitialize(req *mcpRequest) mcpResponse {
	// 回显客户端请求的 protocolVersion（不硬写死，兼容 2025-11-25 / 2026-07-28）
	version := MCPProtocolLegacy
	if len(req.Params) > 0 {
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.ProtocolVersion != "" {
			version = params.ProtocolVersion
		}
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo": map[string]any{
			"name":    MCPDefaultName,
			"version": s.serverVer,
		},
		"instructions": "lightmem 面向 AI Agent 与人的长期记忆系统（四层温度分层 + 多信号融合）。\n" +
			"提供计划(goal)/步骤(procedure)/总结(event)/进度推进/计划查询等完整工作流记忆工具。\n" +
			"当用户要求记住某事时调用 lightmem_save；问题可能依赖历史信息时先调用 lightmem_recall。",
	}
	return mcpResponse{Result: result}
}

func (s *MCPServer) handleToolsList(req *mcpRequest) mcpResponse {
	// 支持 cursor 分页参数（忽略，一次性返回全部）
	result := map[string]any{"tools": s.Tools()}
	return mcpResponse{Result: result}
}

func (s *MCPServer) handleToolsCall(ctx context.Context, req *mcpRequest) mcpResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return mcpResponse{
			Error: &mcpError{Code: MCPErrInvalid, Message: "invalid params: " + err.Error()},
		}
	}
	if params.Name == "" {
		return mcpResponse{
			Error: &mcpError{Code: MCPErrInvalid, Message: "invalid params: name required"},
		}
	}
	var handler mcpToolHandler
	for _, t := range s.tools {
		if t.meta.Name == params.Name {
			handler = t.handle
			break
		}
	}
	if handler == nil {
		return mcpResponse{
			Result: toolResult("未知工具："+params.Name, true),
		}
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	// 注入身份默认值（B 线：token 身份优先，忽略 body 里的 user_id/tenant_id）
	// R21 T3：stdio 无请求 context，回退到进程级单身份（s.rest.identity）。
	var fb *Identity
	if s.rest != nil {
		fb = s.rest.identity
	}
	args := injectIdentityArgsWith(ctx, params.Arguments, fb)
	text, err := handler(ctx, args)
	if err != nil {
		return mcpResponse{Result: toolResult(err.Error(), true)}
	}
	return mcpResponse{Result: toolResult(text, false)}
}

func toolResult(text string, isErr bool) MCPToolResult {
	r := MCPToolResult{Content: []MCPContent{{Type: "text", Text: text}}}
	if isErr {
		r.IsError = true
	}
	return r
}

func (s *MCPServer) mustMarshal(v mcpResponse) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return b
}

// injectIdentityArgs 把「权威身份」注入工具入参，使各工具 handler 无需各自处理身份。
// 已通过鉴权时用 token 身份覆盖 user_id/tenant_id/agent_id（防冒充）。
//
// 身份来源优先级：
//  1. 请求 context 里的 Identity（HTTP transport 由 AuthMiddleware 注入）；
//  2. R21 T3：若 context 无身份（stdio 通道没有请求），回退到配置在 REST 客户端上的
//     **stdio 单身份**（由 main.go 用 SetUpstreamCredentials 装配）。
//
// 第二条是 stdio 能写库的关键：否则 handler 拿不到 tenant_id，会被 REST 端
// 以 `tenant_id required` 拒（400）。
func injectIdentityArgs(ctx context.Context, args map[string]any) map[string]any {
	return injectIdentityArgsWith(ctx, args, nil)
}

// injectIdentityArgsWith 注入身份；fallback 为 stdio 场景的进程级单身份（可为 nil）。
func injectIdentityArgsWith(ctx context.Context, args map[string]any, fallback *Identity) map[string]any {
	id, ok := IdentityFromContext(ctx)
	if !ok && fallback != nil {
		id, ok = *fallback, true
	}
	if !ok {
		return args
	}
	if id.UserID != "" {
		args["user_id"] = id.UserID
	}
	if id.TenantID != "" {
		args["tenant_id"] = id.TenantID
	}
	if id.AgentID != "" {
		args["agent_id"] = id.AgentID
	}
	return args
}

// ------------------------------------------------------------------
// 工具参数常用提取辅助
// ------------------------------------------------------------------

func strArg(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return def
		}
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}

func floatArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, err := t.Float64()
		if err == nil {
			return f
		}
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err == nil {
			return f
		}
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		i, err := t.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		i, err := strconv.Atoi(t)
		if err == nil {
			return i
		}
	}
	return def
}

func boolArg(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return def
}
