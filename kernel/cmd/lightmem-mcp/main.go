// lightmem-mcp —— 内核原生 MCP 服务端（R21-A）
//
// 用法：
//
//	lightmem-mcp -transport http -listen :18444        # streamable-http（独立端口，不抢 18443）
//	lightmem-mcp -transport stdio                      # stdio 兜底（CodeArts/Cline 直连）
//	lightmem-mcp -transport http -rest-base http://127.0.0.1:18443
//
// 依赖 B 线的 AuthMiddleware（LIMS_AGENT_TOKENS 未配置时透传，零回归）。
// 红线⑦：不引入任何第三方依赖（go.mod/go.sum 不变）。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lightmem/kernel"
)

func main() {
	transport := flag.String("transport", "http", "transport: http (streamable-http) | stdio")
	listen := flag.String("listen", ":18444", "HTTP listen address (transport=http)")
	restBase := flag.String("rest-base", "", "REST loopback base URL (default http://127.0.0.1:18443)")
	timeout := flag.Duration("timeout", 15*time.Second, "REST call timeout")
	flag.Parse()

	if *restBase == "" {
		*restBase = "http://127.0.0.1:18443"
	}

	// 装配 token 表：仅从环境变量 LIMS_AGENT_TOKENS 读取（B 线提供；未配置则透传，红线④）。
	tokens := kernel.LoadAgentTokens(os.Getenv, nil)
	kernel.InitAgentTokens(tokens)
	if len(tokens) == 0 {
		log.Printf("[mcp] token 未配置，AuthMiddleware 透传（R21-B 默认关）")
	} else {
		log.Printf("[mcp] token 表已装配：%d 个", len(tokens))
	}

	mcpSrv := kernel.NewMCPServer(*restBase)
	mcpSrv.SetServerVersion("0.2.0")
	_ = timeout

	// R21 T3（解 D-1）：stdio 没有请求头，无法像 HTTP 那样从 Authorization 解析身份。
	// 部署约定：stdio 场景每个 Agent 各起一个进程，其 LIMS_AGENT_TOKENS 只装该 Agent
	// 自己那一条 token。此时「进程身份」唯一确定，用它：
	//   ① 向 REST 上游带 Authorization（否则 Windows→0.88 非回环调用必 401）；
	//   ② 透传 X-Lightmem-* 身份。
	// 表内多于一条（多身份无法区分）时**不猜测**，保持无身份（会被上游 401 拒），
	// 以免把身份错配给错误的 Agent。
	if tok, id, ok := kernel.SingleIdentity(); ok {
		mcpSrv.SetUpstreamCredentials(tok, &id)
		log.Printf("[mcp] stdio 单身份已装配：agent=%s user=%s tenant=%s",
			id.AgentID, id.UserID, id.TenantID)
	} else {
		log.Printf("[mcp] 未装配 stdio 单身份（token 表 0 条或多条）→ 上游调用可能 401；" +
			"stdio 部署请让每个 Agent 各带自己那一条 token")
	}

	switch *transport {
	case "stdio":
		log.Printf("[mcp] stdio transport: lightmem-mcp v0.2.0 base=%s", *restBase)
		stdioSrv := kernel.NewStdioServer(mcpSrv, os.Stdin, os.Stdout)
		stdioSrv.Run()
	case "http":
		httpSrv := kernel.NewMCPHTTPServer(mcpSrv)
		// B 线鉴权（透传或 401）+ 恢复
		var handler http.Handler = httpSrv.Handler()
		handler = kernel.AuthMiddleware(handler)
		handler = kernel.RecoverMiddleware(handler)
		server := &http.Server{
			Addr:         *listen,
			Handler:      handler,
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 120 * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		log.Printf("[mcp] HTTP transport on %s (MCP-over-HTTP 端口 18444，业务 REST 仍在 18443)", *listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[mcp] listen %s: %v", *listen, err)
		}
	default:
		log.Fatalf("[mcp] unknown transport %q (want http|stdio)", *transport)
	}
}
