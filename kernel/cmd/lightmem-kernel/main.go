// lightmem-kernel 生产级入口（R9-A 重写）
// 用法：
//
//	lightmem-kernel -profile ../profiles/lims-family.profile.yaml -listen :8080
//	lightmem-kernel -embedder vector -vector-meta eval/vectors/vectors_meta.json ...
//	lightmem-kernel -pprof -listen :18080
//
// 相对 R8 骨架（d1e458f）的增量（任务书 §1.A）：
//   - graceful shutdown（SIGTERM/SIGINT → http.Server.Shutdown → drain consolidate → 关后端）
//   - 中间件链：Logging → Metrics → Recover → TraceID → Auth → RequireIdentity → 业务路由
//     （R21-B：Auth/RequireIdentity 默认透传，未配置 token 时与 R9-A 行为逐位一致）
//   - /metrics 升级：默认输出 Prometheus 文本（R9 指标 + 既有 lims_* 指标）；?format=json 保留旧 JSON
//   - /healthz 升级：JSON {status, backend, embedder, last_consolidate}
//   - -embedder hash|vector（默认 hash 占位，零依赖启动）
//   - -pprof（默认关闭）、-shutdown-timeout（默认 10s）
//
// 红线（任务书 §1.A.2）：
//   - 默认态（无 flag 变更）下启动行为与原骨架逐位兼容：同端口、同业务路由、同 SQLite fallback
//   - 不改 kernel/router.go（本文件只在外层 mux 追加 /healthz 与拦截 /metrics，不触碰业务路由）
//   - 不改 Memory / RouterContext / RecalledCandidate 字段语义
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lightmem/kernel"
)

const (
	defaultVectorMatrix = "eval/vectors/vectors.npy"
	defaultVectorMeta   = "eval/vectors/vectors_meta.json"
	defaultVectorIndex  = "eval/vectors/vectors_index.jsonl"
)

func main() {
	profilePath := flag.String("profile", "../profiles/lims-family.profile.yaml", "path to lims-*.profile.yaml")
	listen := flag.String("listen", ":8080", "listen address")
	backendOverride := flag.String("backend-override", "", "override storage backend for skeleton run (sqlite)")
	embedderMode := flag.String("embedder", "hash", "embedder mode: hash (default, zero-dep) | vector (BGE-m3 static)")
	vectorMatrix := flag.String("vector-matrix", defaultVectorMatrix, "path to .npy vector matrix (used when -embedder vector)")
	vectorMeta := flag.String("vector-meta", defaultVectorMeta, "path to vector metadata json (used when -embedder vector)")
	vectorIndex := flag.String("vector-index", defaultVectorIndex, "path to text index jsonl (used when -embedder vector)")
	pprofOn := flag.Bool("pprof", false, "enable /debug/pprof endpoints (default false)")
	metricsOn := flag.Bool("metrics", true, "expose /metrics endpoint (default true)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	// R22.2/R22.3：管理 API 独立端口。空 = 不启用（零回归，缺省行为不变）。
	adminListen := flag.String("admin-listen", "", "enable admin API (R22.2) on this address, e.g. 127.0.0.1:8090 (recommended loopback only); empty = disabled")
	flag.Parse()

	// 1) 装配 profile
	p, err := kernel.LoadProfile(*profilePath, os.Getenv)
	if err != nil {
		log.Fatalf("[boot] %v", err)
	}
	log.Printf("[boot] profile=%s backend=%s tier=%s", p.Profile, p.StorageBackend, p.Profile)

	if *backendOverride != "" {
		if *backendOverride != "sqlite" {
			log.Fatalf("[boot] unknown backend-override %q (only sqlite)", *backendOverride)
		}
		log.Printf("[WARN] overriding storage_backend %s -> sqlite (skeleton only)", p.StorageBackend)
		p.StorageBackend = kernel.BackendSQLite
	}

	// 2) 装配存储后端
	backend, err := kernel.NewStorageBackend(context.Background(), p)
	if err != nil {
		log.Fatalf("[boot] %v", err)
	}

	// 3) 装配 API server + embedder + 生命周期策略 + rank fusion
	srv := kernel.NewAPIServer(p, backend)
	dim := 1024
	if p.Storage != nil && p.Storage.Embedding != nil && p.Storage.Embedding.Dimensions > 0 {
		dim = p.Storage.Embedding.Dimensions
	}

	embedderName := "hash"
	switch strings.ToLower(*embedderMode) {
	case "hash":
		srv.SetEmbedder(kernel.HashEmbedder(dim))
	case "vector":
		ve, verr := kernel.NewVectorEmbedder(kernel.VectorEmbedderConfig{
			MatrixPath: *vectorMatrix,
			MetaPath:   *vectorMeta,
			IndexPath:  *vectorIndex,
		})
		if verr != nil {
			_ = backend.Close()
			log.Fatalf("[boot] -embedder vector failed (%v); retry with -embedder hash", verr)
		}
		srv.SetEmbedder(ve.Embed)
		embedderName = ve.Name()
		log.Printf("[boot] vector embedder loaded: %s dim=%d count=%d",
			ve.Name(), ve.Dim(), ve.Count())
	default:
		_ = backend.Close()
		log.Fatalf("[boot] unknown -embedder %q (want hash|vector)", *embedderMode)
	}

	srv.SetLayerPolicy(kernel.DefaultLayerPolicy())
	srv.SetRankFusion(kernel.DefaultRankFusion())

	// R22.2/R22.3：启用管理 API 时，把后端 DB 注入 APIServer（/admin 路由据此注册）。
	// 缺省 -admin-listen 为空 = 不注入 = /admin 不暴露（接线默认关，零回归）。
	if *adminListen != "" {
		dp, ok := backend.(kernel.DBProvider)
		if !ok {
			_ = backend.Close()
			log.Fatalf("[boot] -admin-listen requires a backend exposing DB(); got %T (sqlite supported)", backend)
		}
		// 确保 auth 表存在（R22 验收暴露：SetAuthDB 只挂 AdminStore，不建表）。
		// 不建表会让管理 API 首次调用报 "no such table"，此处显式幂等建表。
		actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := kernel.EnsureAuthTables(actx, dp.DB()); err != nil {
			acancel()
			_ = backend.Close()
			log.Fatalf("[boot] ensure auth tables: %v", err)
		}
		acancel()
		srv.SetAuthDB(dp.DB())
		// R22 验收暴露的接线缺口：DB token 表（agent_tokens/auth_tokens）此前只对
		// admin API 生效，业务端点（/mcp、REST）从未走它。此处注入 dbAuthenticator，
		// 使签发的 token 能真正鉴权业务请求。
		// 注：若 profile 显式配置了外部认证器（ory_kratos/sso），后执行的 ConfigureAuth
		// 会覆盖它（外部 IdP 优先）；未配置则 DB token 认证生效，覆盖 legacy 静态表路径。
		kernel.SetAuthenticator(kernel.NewDBAuthenticator(dp.DB(), "sqlite"))
		log.Printf("[boot] admin API enabled + DB token auth wired — listen on %s", *adminListen)
	}

	// 4) 后台整合 worker（启动前挂 R9-A consolidate 时间戳回调）
	cons := kernel.NewConsolidator(backend)
	if v := os.Getenv("LIMS_CONSOLIDATE_INTERVAL"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil && d > 0 {
			cons.SetInterval(d)
		} else {
			log.Printf("[boot] invalid LIMS_CONSOLIDATE_INTERVAL=%q, using default", v)
		}
	}

	// 5) 进程级指标注册表 + consolidate 时间戳回填
	metrics := kernel.NewServerMetrics()
	cons.SetOnRunOnce(func(rep kernel.ConsolidationReport) {
		metrics.RecordConsolidate(rep.RanAt)
	})

	// 5b) R24-C：访问追踪器（L-223）+ 生命周期引擎（L-224）接线。
	//     **默认全关**：两个开关皆未设时不注入、不注册路由、不起定时器，
	//     逐位保持 c908091 行为（红线：新能力默认关）。
	//
	//     与 A 线的接缝：A 线已在 RecallAndFuse 返回前调用 s.NoteRecall(ids)，
	//     且 access==nil 时 no-op；这里注入 tracker 后即自动承接命中计数。
	//     `router_mem.go` 归 A 线独占，C 线不改它。
	accessTracker := kernel.NewAccessTracker(accessTrackerCapacity())
	if os.Getenv("LIMS_ACCESS_TRACK") == "1" {
		srv.SetAccessTracker(accessTracker)
		log.Printf("[boot] R24-C access tracker enabled (capacity=%d, tau=7d)", accessTrackerCapacity())
	}
	// 5c) 访问计数落库定时器（默认关；空/非法值=不启用）
	if v := os.Getenv("LIMS_ACCESS_FLUSH_INTERVAL"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil && d > 0 {
			go func() {
				ticker := time.NewTicker(d)
				defer ticker.Stop()
				for range ticker.C {
					if n, err := accessTracker.Flush(context.Background(), backend, ""); err != nil {
						log.Printf("[access] flush failed: %v", err)
					} else if n > 0 {
						log.Printf("[access] flushed %d memories", n)
					}
				}
			}()
			log.Printf("[boot] R24-C access flush timer enabled: %s", d)
		} else {
			log.Printf("[boot] invalid LIMS_ACCESS_FLUSH_INTERVAL=%q, disabled", v)
		}
	}
	// 5d) B/C 两线的管理端点：按开关自注册（默认不启用 → routeHooks 为空 → 零回归）。
	if os.Getenv("LIMS_ADMIN_REINDEX") == "1" {
		kernel.EnableReindexRoutes(srv) // B 线：/admin/reconcile、/admin/reindex
		log.Printf("[boot] R24-B reindex admin endpoints enabled")
	}
	if os.Getenv("LIMS_LIFECYCLE_INTERVAL") != "" || os.Getenv("LIMS_ADMIN_LIFECYCLE") == "1" {
		kernel.EnableLifecycleRoutes(srv) // C 线：/admin/lifecycle/run
		log.Printf("[boot] R24-C lifecycle admin endpoint enabled")
	}

	// 6) 装配 HTTP mux：外层 mux 承载 /healthz、/metrics、pprof；
	//    业务路由整体挂载到 "/"（逐位保留 router.go 全部端点）。
	mux := http.NewServeMux()
	mux.Handle("/healthz", kernel.HealthzHandler(srv, backend, embedderName, metrics))
	if *metricsOn {
		mux.Handle("/metrics", metricsHandler(metrics))
	}
	if *pprofOn {
		log.Printf("[boot] pprof enabled on /debug/pprof/")
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	mux.Handle("/", srv.Handler())

	// 6b) R21-B：鉴权装配（task book §3.1/§五 B 线）。
	//     token 源：env LIMS_AGENT_TOKENS（JSON 或文件路径）优先，
	//     回落 profile auth.agent_tokens；两者皆无 = 透传（红线④，与 R20 逐位一致）。
	//     配置损坏 → fail-fast（绝不带病启动后静默无鉴权）。
	if err := kernel.ConfigureAuth(kernel.AuthOptions{ProfilePath: *profilePath}); err != nil {
		_ = backend.Close()
		log.Fatalf("[boot] auth config: %v", err)
	}

	handler := kernel.WithMiddleware(mux,
		kernel.LoggingMiddleware,
		kernel.MetricsMiddleware(metrics),
		kernel.RecoverMiddleware,
		kernel.TraceIDMiddleware,
		kernel.AuthMiddleware,
		kernel.RequireIdentity,
	)

	// 7) 后台整合 goroutine
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go cons.Start(bgCtx)

	// 8) HTTP server
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// R22.3：管理 API 独立端口。建议仅绑定回环；adminGuard 默认已拒绝非回环来源。
	var adminServer *http.Server
	if ah := srv.AdminHandler(); ah != nil {
		adminServer = &http.Server{
			Addr:              *adminListen,
			Handler:           ah,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("[boot] admin api listening on %s", *adminListen)
			if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("[admin] %v", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[boot] listening on %s", *listen)
		errCh <- server.ListenAndServe()
	}()

	log.Printf("[boot] embedder=%s metrics=%v pprof=%v shutdown-timeout=%s",
		embedderName, *metricsOn, *pprofOn, *shutdownTimeout)

	select {
	case err := <-errCh:
		bgCancel()
		_ = backend.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[serve] %v", err)
		}
	case sig := <-sigCh:
		log.Printf("[shutdown] received signal %s, graceful draining (timeout=%s)", sig, *shutdownTimeout)
	}

	// 9) graceful shutdown：停止接收新请求，等待在途请求完成
	shCtx, shCancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer shCancel()
	_ = server.Shutdown(shCtx)
	if adminServer != nil {
		_ = adminServer.Shutdown(shCtx)
	}

	// 10) drain 后台 consolidate
	log.Printf("[shutdown] cancel consolidate worker")
	bgCancel()

	// 11) 关闭后端连接池
	log.Printf("[shutdown] closing backend %s", backend.Name())
	_ = backend.Close()
	log.Printf("[shutdown] done")
}

// metricsHandler 返回 /metrics 处理器。
// 默认输出 Prometheus 文本（R9 指标 + 既有 lims_* 指标）；
// ?format=json 保留 R8 骨架的 JSON 输出格式（逐位兼容旧消费者）。
func metricsHandler(metrics *kernel.ServerMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "json" {
			snap := kernel.GetMetricsCollector().Snapshot()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			writeJSON(w, http.StatusOK, map[string]any{
				"lims_recall_latency_ms":   snap.RecallLatencyMS,
				"lims_hit_rate":            snap.HitRate,
				"lims_pollution_rate":      snap.PollutionRate,
				"lims_layer_counts":        snap.LayerCounts,
				"lims_total_memories":      snap.TotalMemories,
				"lims_total_queries":       snap.TotalQueries,
				"lims_signal_latencies_ms": snap.SignalLatencies,
				"lims_feedback_stats":      snap.FeedbackStats,
				"lims_lifecycle_stats":     snap.LifecycleStats,
			})
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(metrics.PrometheusText()))
		_, _ = w.Write([]byte(kernel.GetMetricsCollector().PrometheusText()))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// accessTrackerCapacity 访问追踪器的待落库条目上限（LIMS_ACCESS_CAPACITY）。
// <=0 表示不限（由 Flush 频率兜底）；非法值回退到 10000，避免静默无界增长。
func accessTrackerCapacity() int {
	v := os.Getenv("LIMS_ACCESS_CAPACITY")
	if v == "" {
		return 10000
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
		log.Printf("[boot] invalid LIMS_ACCESS_CAPACITY=%q, using 10000", v)
		return 10000
	}
	return n
}
