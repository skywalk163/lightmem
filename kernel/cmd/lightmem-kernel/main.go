// lightmem-kernel 生产级入口（R9-A 重写）
// 用法：
//
//	lightmem-kernel -profile ../profiles/lims-family.profile.yaml -listen :8080
//	lightmem-kernel -embedder vector -vector-meta eval/vectors/vectors_meta.json ...
//	lightmem-kernel -pprof -listen :18080
//
// 相对 R8 骨架（d1e458f）的增量（任务书 §1.A）：
//   - graceful shutdown（SIGTERM/SIGINT → http.Server.Shutdown → drain consolidate → 关后端）
//   - 中间件链：Logging → Metrics → Recover → TraceID → 业务路由
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

	handler := kernel.WithMiddleware(mux,
		kernel.LoggingMiddleware,
		kernel.MetricsMiddleware(metrics),
		kernel.RecoverMiddleware,
		kernel.TraceIDMiddleware,
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
