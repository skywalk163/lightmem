// R21-A MCP 双传输（§五 A 线 2）
//
// stdio：逐行读 stdin（bufio.Scanner 设大 buffer），UTF-8 原样，不得做任何 GBK 转换
//
//	—— Go 默认 UTF-8，天然规避 R160 I-10（Python 桥的 surrogates not allowed 坑）。
//
// streamable-http：POST /mcp，请求 Accept: application/json, text/event-stream；
//
//	响应按需 JSON 或 SSE；实现 Mcp-Session-Id 头（initialize 生成、后续请求携带）；
//	GET /mcp（SSE 流）与 DELETE /mcp（会话终止）。
package kernel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------
// stdio transport
// ------------------------------------------------------------------

// StdioServer 封装 stdin/stdout 的 MCP stdio 服务端。
type StdioServer struct {
	MCP *MCPServer
	in  *bufio.Scanner
	out io.Writer
}

// NewStdioServer 创建 stdio 服务端（in/out 可注入，测试用；nil 时用 os.Stdin/os.Stdout）。
func NewStdioServer(mcp *MCPServer, in io.Reader, out io.Writer) *StdioServer {
	scanner := bufio.NewScanner(in)
	// 大 buffer：单条 JSON-RPC 可能很长（工具 schema 描述）；bufio.Scanner 默认 64KB 上限会截断。
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &StdioServer{MCP: mcp, in: scanner, out: out}
}

// Run 阻塞式运行 stdio 主循环，直至 stdin 关闭。
// 任何单条请求异常都不杀死主循环。
func (s *StdioServer) Run() {
	for s.in.Scan() {
		line := strings.TrimSpace(s.in.Text())
		if line == "" {
			continue
		}
		// 单条 JSON；不做批量展开（MCP 已弃用批量，遇列表则按项处理）
		ctx := context.Background()
		out := s.MCP.HandleJSON(ctx, []byte(line))
		if out == nil {
			continue
		}
		if _, err := s.out.Write(append(out, '\n')); err != nil {
			log.Printf("[mcp-stdio] write error: %v", err)
			return
		}
	}
	log.Printf("[mcp-stdio] stdin closed, exit")
}

// ------------------------------------------------------------------
// streamable-http transport
// ------------------------------------------------------------------

// HTTPServer 封装 streamable-http 的 HTTP 路由（A 线独立 mux，不抢占业务 18443）。
type HTTPServer struct {
	MCP *MCPServer

	mu       sync.Mutex
	sessions map[string]*mcpHttPSession
}

type mcpHttPSession struct {
	lastSeen time.Time
}

// NewMCPHTTPServer 创建 HTTP transport 处理器。
func NewMCPHTTPServer(mcp *MCPServer) *HTTPServer {
	return &HTTPServer{MCP: mcp, sessions: map[string]*mcpHttPSession{}}
}

// Handler 返回 http.Handler，路由到 /mcp 端点（支持 POST/GET/DELETE）。
//
// T3 部署（R21）：额外提供 /healthz 供 rc.d supervisor 与监控探针使用。
// 该端点**不暴露任何记忆数据**（只回进程存活与 REST 后端地址），
// 且被 AuthMiddleware 的运维端点豁免规则放行——否则 supervisor 的
// `fetch /healthz` 会 401，lightmem-mcp 服务无法启动。
func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

// handleHealthz 返回 MCP 服务自身存活状态与上游 REST 地址（不含记忆数据）。
func (s *HTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	upstream := ""
	if s != nil && s.MCP != nil {
		upstream = s.MCP.baseURL
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"service":  "lightmem-mcp",
		"version":  "0.2.0",
		"upstream": upstream,
	})
}

func (s *HTTPServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handlePost POST /mcp：JSON 或 SSE（按 Accept 头）
func (s *HTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	acceptSSE := acceptsSSE(r.Header.Get("Accept"))

	// initialize 生成会话 ID
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &probe)

	ctx := r.Context()
	if sessionID != "" {
		ctx = withMCPIdentityFromHTTP(ctx, r)
	}
	out := s.MCP.HandleJSON(ctx, body)
	if out == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if probe.Method == "initialize" {
		newID := newSessionID()
		s.mu.Lock()
		s.sessions[newID] = &mcpHttPSession{lastSeen: time.Now()}
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", newID)
	} else if sessionID != "" {
		s.touchSession(sessionID)
	}

	if acceptSSE {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", out)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleGet GET /mcp：SSE 流（长连接，保持到上下文取消）
func (s *HTTPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		sessionID = newSessionID()
		s.mu.Lock()
		s.sessions[sessionID] = &mcpHttPSession{lastSeen: time.Now()}
		s.mu.Unlock()
	}
	s.touchSession(sessionID)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Mcp-Session-Id", sessionID)
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 保活心跳，直到客户端断开
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keep-alive %s\n\n", time.Now().UTC().Format(time.RFC3339))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

// handleDelete DELETE /mcp：终止会话
func (s *HTTPServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	s.mu.Lock()
	if sessionID != "" {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "terminated"})
}

func (s *HTTPServer) touchSession(id string) {
	s.mu.Lock()
	if ses, ok := s.sessions[id]; ok {
		ses.lastSeen = time.Now()
	}
	s.mu.Unlock()
}

// acceptsSSE 判断客户端是否只接受 SSE（不含 application/json）。
// DSH 客户端会同时声明 `Accept: application/json, text/event-stream`，
// 此时应返回 JSON（兼容绝大多数 MCP 客户端）；仅当客户端**只**要 SSE 时才走 SSE。
func acceptsSSE(accept string) bool {
	lower := strings.ToLower(accept)
	if !strings.Contains(lower, "text/event-stream") {
		return false
	}
	// 若同时声明 application/json，优先 JSON（MCP 规范：POST 单请求默认 application/json）
	return !strings.Contains(lower, "application/json")
}

// ------------------------------------------------------------------
// HTTP transport 侧的身份注入
// ------------------------------------------------------------------

func withMCPIdentityFromHTTP(ctx context.Context, r *http.Request) context.Context {
	if _, ok := IdentityFromContext(ctx); ok {
		return ctx
	}
	user := r.Header.Get("X-Lightmem-User")
	tenant := r.Header.Get("X-Lightmem-Tenant")
	agent := r.Header.Get("X-Lightmem-Agent")
	if user == "" && tenant == "" && agent == "" {
		return ctx
	}
	return withIdentity(ctx, Identity{UserID: user, TenantID: tenant, AgentID: agent})
}

// ------------------------------------------------------------------
// 会话 ID
// ------------------------------------------------------------------

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
