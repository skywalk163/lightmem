// /healthz 增量字段接线（R29-T0 PE-1，设计稿 §3.2.3「/healthz 增量字段（只加不减）」）。
//
// 为什么是本文件而不是改 server_metrics.go：
//   - `kernel/server_metrics.go` 是 **B 线独占域**（任务书 §2.2），T0 不得越界改；
//   - 故用**装饰器**（decorator）包住既有 `HealthzHandler`：先按原样产出响应，
//     再按需注入启动自检字段。原 handler 的语义（含 503 degraded 分支、
//     last_consolidate、profile）**一字不改**，只是多了一层 JSON 合并。
//
// 语义（设计稿 §3.2.3）：
//   - **只加不减**：不覆盖 status/backend/embedder/last_consolidate/profile；
//   - **默认静默（红线④）**：`opts == nil` 或字段零值 → 输出与 c203db8 逐字节一致；
//   - **不泄漏**：schema_check 只给结论与缺失列名，不回显 DSN/口令
//     （`ProfileJSON` 已脱敏，勿重复实现）。
//
// 增量字段：
//
//	started_at          进程启动时刻（一眼看出「刚重启过」）
//	schema_ok           "ok" / "missing" / "skipped"（启动自检表+列结论）
//	schema_check        "ok" / "missing:<col>,<col>" / "skipped"
//	rows                启动自检时的条数（R16 对比基线）
//	restore_point       WAL 大小 / PG 恢复点
//	unfinished          上次异常关闭的残留排水任务（D 线标记）
//	version_match       "false" 时输出（二进制与库版本不一致）
//	version             构建版本（BuildVersion；unknown 时不输出）
//	last_lifecycle_run  上次生命周期引擎运行时刻（默认关时不输出）
package kernel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// HealthzOptions 是 /healthz 的可选增量字段来源（R29-T0 PE-1）。
//
// 全部字段零值 = 不输出该字段 = 与 c203db8 逐字节一致（红线④）。
// 由 main.go 在装配期填充：启动自检结果（E 线 RunBootReport）+ 生命周期时间戳（C 线）。
type HealthzOptions struct {
	// StartedAt 进程启动时刻（零值 = 不输出 started_at）。
	StartedAt time.Time
	// BootReport 启动自检结果（nil = 不输出 schema_* / rows / restore_point）。
	BootReport *BootReport
	// LifecycleRunFn 在**请求时**读取上次生命周期运行时刻（nil = 不输出该字段）。
	//
	// 用函数而非快照值：维护调度器在 mux 装配**之后**才启动（main.go 顺序），
	// 快照会永远拿到零值。返回零值时不输出该字段（默认关语义）。
	LifecycleRunFn func() time.Time
}

// HealthzWithOptions 装饰既有 /healthz handler，按需注入启动自检字段（PE-1）。
//
// base 为 nil 时原样返回 nil（调用方无需判空分支）。
// opts 为 nil 时**直接返回 base**——零开销、零行为变化（默认关）。
//
// 默认关的精确边界（红线④口径，务必分清）：
//   - `schema_ok` / `schema_check` / `rows` / `restore_point` / `unfinished` /
//     `version_match` **仅在配了 -data-dir（bootRep != nil）时**出现；
//   - `started_at` 与 `version` 是**纯加法**（设计稿 §3.2.3 明列的两个字段）：
//     started_at 恒可得（进程启动时刻，无依赖）；version 仅在构建注入了
//     BuildVersion 时出现（未注入 = unknown = 不输出）。
//     二者不改变任何既有字段语义，旧消费者按 key 取值不受影响。
func HealthzWithOptions(base http.Handler, opts *HealthzOptions) http.Handler {
	if base == nil || opts == nil {
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &healthzCapture{header: http.Header{}}
		base.ServeHTTP(rec, r)

		body := rec.body.Bytes()
		// 仅当原 handler 产出合法 JSON 对象时才合并；否则原样透传（防御性，
		// 绝不因装饰器把健康检查打挂）。
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			copyHeader(w.Header(), rec.header)
			w.WriteHeader(rec.status)
			_, _ = w.Write(body)
			return
		}
		opts.applyToAny(obj)

		out, err := json.Marshal(obj)
		if err != nil {
			copyHeader(w.Header(), rec.header)
			w.WriteHeader(rec.status)
			_, _ = w.Write(body)
			return
		}
		copyHeader(w.Header(), rec.header)
		w.WriteHeader(rec.status)
		_, _ = w.Write(out)
	})
}

// applyToAny 把非零字段写入 /healthz 响应对象（只加不减）。
func (o *HealthzOptions) applyToAny(resp map[string]any) {
	if o == nil {
		return
	}
	if !o.StartedAt.IsZero() {
		resp["started_at"] = o.StartedAt.UTC().Format(time.RFC3339)
	}
	if br := o.BootReport; br != nil {
		resp["schema_ok"] = br.Table
		resp["schema_check"] = br.Columns
		resp["rows"] = strconv.FormatInt(br.Rows, 10)
		if br.RestorePoint != "" {
			resp["restore_point"] = br.RestorePoint
		}
		if br.Unfinished != "" {
			// 上次异常关闭的残留排水任务（D 线标记，E 线自检读取）。
			resp["unfinished"] = br.Unfinished
		}
		if !br.VersionMatch {
			resp["version_match"] = "false"
		}
	}
	if lr := o.LifecycleRunFn; lr != nil {
		if t := lr(); !t.IsZero() {
			resp["last_lifecycle_run"] = t.UTC().Format(time.RFC3339)
		}
	}
	if v := BuildVersion; v != "" && v != "unknown" {
		resp["version"] = v
	}
}

// healthzCapture 是轻量响应捕获器（记录 status/header/body，供装饰器改写）。
type healthzCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *healthzCapture) Header() http.Header { return c.header }

func (c *healthzCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *healthzCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK // 未显式 WriteHeader 时默认 200
	}
	return c.body.Write(p)
}

// copyHeader 把捕获的 header 写回真实 ResponseWriter（跳过长度相关头，
// 因为 body 已被改写，长度由 net/http 重新计算）。
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
