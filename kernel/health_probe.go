// 健康探针（R29-T0 S0 接缝，最小可用实现）。
//
// 职责边界（与 B/C/D/E 线的分工，见任务书 §2.2 / §10）：
//   - LivezHandler：存活探针，只回答「进程在不在」，不碰任何依赖（最快）。
//   - ReadyzHandler：就绪探针，回答「能不能接流量」。S0 版本仅用 StorageBackend.Ping
//     探活（**无 TTL 缓存**）；B 线硬化：TTL 缓存 + SELECT 1 FROM memories 读探针，
//     见 health_probe.go 的 NewBackendProbe，B 线接手增强。
//
// 红线：本文件只用 stdlib，不引入第三方依赖（红线③）。
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// LivezHandler 存活探针：立即返回 200。不触达数据库/嵌入器，零依赖、最低延迟。
func LivezHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "alive"})
	})
}

// ReadinessProbe 就绪检查契约（§10 冻结）。
type ReadinessProbe interface {
	Check(ctx context.Context) error
}

// backendProbe 用 StorageBackend.Ping 实现就绪探测（S0 最小可用）。
// B 线应在其外层加 TTL 缓存与 SELECT 1 读探针，避免每次探活打数据库。
type backendProbe struct {
	backend StorageBackend
	name    string
}

// NewBackendProbe 构造后端就绪探针（§10 冻结签名）。
func NewBackendProbe(backend StorageBackend, name string) ReadinessProbe {
	return &backendProbe{backend: backend, name: name}
}

// Check 探活：Ping 不通即未就绪。
func (p *backendProbe) Check(ctx context.Context) error {
	if p.backend == nil {
		return errNilBackend
	}
	return p.backend.Ping(ctx)
}

// errNilBackend 在探针构造时传入 nil 后端时返回。
var errNilBackend = &probeError{msg: "backend is nil"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }

// ReadyzHandler 就绪探针 HTTP 端点（§10 冻结签名）。
// 检查失败 → 503；响应体仅 {status, check}。
func ReadyzHandler(p ReadinessProbe) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := p.Check(ctx); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "not_ready",
				"check":  err.Error(),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
	})
}
