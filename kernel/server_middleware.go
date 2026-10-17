// R9-A：HTTP 中间件（请求日志 / panic recover / trace ID）
// 真源：`lightmem/.dumate/inbox/_R9_生产化收口任务书.md` §1.A.3.1。
//
// 设计：
//   - LoggingMiddleware：结构化访问日志（method / path / status / latency_ms / trace_id / remote_ip），输出到 stdout；
//   - RecoverMiddleware：panic 捕获，返回 500 并记录堆栈，进程不崩；
//   - TraceIDMiddleware：单调递增 trace ID（进程级内存计数器，确定性，不用 UUID/随机数），
//     写入响应头 `X-Trace-Id` 并注入日志。
//
// 确定性红线（任务书 §1.A.2）：
//   - trace ID 用进程级单调计数器，禁止 time.Now()、随机源、map 迭代序；
//   - 中间件链装配顺序由调用方显式给出，不引入任何随机调度。
//
// 只增不改：本文件不修改 Memory / RouterContext / RecalledCandidate 既有字段语义，
// 也不改动 rank_fusion / router_mem / voting 的检索逻辑。
package kernel

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------
// Trace ID
// ------------------------------------------------------------------

// traceSeq 是进程级单调计数器（seed 固定，确定性；不使用随机源/UUID）。
var traceSeq atomic.Uint64

// NextTraceID 返回进程级单调递增的 trace ID（从 1 开始）。
func NextTraceID() uint64 { return traceSeq.Add(1) }

// ------------------------------------------------------------------
// Middleware 类型与链装配
// ------------------------------------------------------------------

// Middleware 是标准 http.Handler 装饰函数。
type Middleware func(http.Handler) http.Handler

// WithMiddleware 按给定顺序把中间件套在业务 handler 外层（先给的最外层）。
// 语义：WithMiddleware(h, A, B) 等价于 A(B(h))，即 A 最先处理请求。
func WithMiddleware(h http.Handler, m ...Middleware) http.Handler {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i] == nil {
			continue
		}
		h = m[i](h)
	}
	return h
}

// ------------------------------------------------------------------
// LoggingMiddleware：结构化访问日志
// ------------------------------------------------------------------

// LoggingMiddleware 记录 method / path / status / latency_ms / trace_id / remote_ip。
// 日志行格式固定（便于脚本断言）：
//
//	[access] method=POST path=/memories status=201 latency_ms=1 trace_id=1 remote_ip=192.168.0.1
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		latencyMS := time.Since(start).Milliseconds()
		// trace ID 由内层 TraceIDMiddleware 写入响应头（共享同一个 ResponseWriter），
		// 因此回读响应头即可；未装配 TraceID 时回退读取请求 context。
		trace := sw.Header().Get("X-Trace-Id")
		if trace == "" {
			if id := TraceIDFromContext(r.Context()); id != 0 {
				trace = strconv.FormatUint(id, 10)
			}
		}
		log.Printf("[access] method=%s path=%s status=%d latency_ms=%d trace_id=%s remote_ip=%s",
			r.Method, r.URL.Path, sw.status, latencyMS, trace, remoteIP(r))
	})
}

// statusWriter 包装 ResponseWriter 以捕获写出的状态码。
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}

// ------------------------------------------------------------------
// RecoverMiddleware：panic 捕获
// ------------------------------------------------------------------

// RecoverMiddleware 捕获 handler panic，返回 500 并打印堆栈，进程不崩。
// panic 信息以结构化日志输出：[panic] path=... err=... stack=...
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] path=%s err=%v\n%s", r.URL.Path, rec, debug.Stack())
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "internal server error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------
// TraceIDMiddleware：单调 trace ID 注入
// ------------------------------------------------------------------

type ctxKey int

const ctxKeyTraceID ctxKey = iota

// TraceIDMiddleware 为每个请求分配进程级单调 trace ID，
// 写入响应头 `X-Trace-Id` 并注入请求 context（供日志中间件读取）。
func TraceIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := NextTraceID()
		w.Header().Set("X-Trace-Id", fmt.Sprintf("%d", id))
		ctx := withTraceID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func withTraceID(ctx context.Context, id uint64) context.Context {
	return context.WithValue(ctx, ctxKeyTraceID, id)
}

// TraceIDFromContext 取出请求 context 中的 trace ID（未注入时返回 0）。
func TraceIDFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(ctxKeyTraceID).(uint64); ok {
		return v
	}
	return 0
}

// ------------------------------------------------------------------
// R21-B：AuthMiddleware 装配说明（仅追加；上述三个中间件语义零改动）
// ------------------------------------------------------------------
//
// 真源：`lightmem/.dumate/inbox/_R21_并行任务书.md` §3.1 / §五 B 线；
// 实现见 `kernel/auth.go`（Identity / IdentityFromContext / AuthMiddleware /
// RequireIdentity），本文件只登记装配约定，不承载鉴权逻辑。
//
// 推荐装配顺序（main.go 与 cmd/lightmem-mcp 一致）：
//
//	handler := WithMiddleware(mux,
//	    LoggingMiddleware,     // 最外层：401 也记访问日志
//	    MetricsMiddleware(m),  // 计数覆盖 401（鉴权失败也是请求）
//	    RecoverMiddleware,     // panic 兜底
//	    TraceIDMiddleware,     // 先分 trace id：401 响应也带 X-Trace-Id
//	    AuthMiddleware,        // token → Identity 注入 context（未配置则透传）
//	    RequireIdentity,       // LIMS_REQUIRE_AUTH=1 时强制已认证（默认透传）
//	)
//
// 默认态（未配置 LIMS_AGENT_TOKENS 且 profile 无 auth.agent_tokens）：
//
//	AuthMiddleware 透传、RequireIdentity 放行 → 与 R20 行为逐位一致（红线④）。
//
// 写方身份强制（handler 侧契约，C 线 router_tools.go 消费）：
//
//	所有写路径（save/update/feedback/forget）的 actor/tenant 一律经
//	ResolveActor(ctx, bodyUserID, bodyTenantID) 解析：身份存在时以 token
//	为准、忽略 body（防冒充，G4 修法）；private 记忆的 owner 必须是 token
//	的 user_id（EnforcePrivateOwner 判定，违反应 403）。
//
// 启动装配（main.go）：kernel.ConfigureAuth(kernel.AuthOptions{ProfilePath: …})
// 一键完成「env LIMS_AGENT_TOKENS（JSON 或文件路径）优先 → profile auth.agent_tokens
// 回落 → 两者皆无则透传」的装配；配置损坏返回 error，由 main fail-fast。
