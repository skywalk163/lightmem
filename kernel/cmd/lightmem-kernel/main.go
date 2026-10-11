// lightmem-kernel 最小可运行骨架入口
// 用法：
//
//	lightmem-kernel -profile ../profiles/lims-family.profile.yaml -listen :8080
//
// 目标：读取 Family 档 profile，装配 StorageBackend，暴露 §4.4 的 API 骨架。
// 注意：当前骨架 SQLite 后端为内存实现（跑通链路用）；生产须替换为
// database/sql + sqlite-vec / pgvector 实现（§4.2 异步向量补全）。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lightmem/kernel"
)

func main() {
	profilePath := flag.String("profile", "../profiles/lims-family.profile.yaml", "path to lims-*.profile.yaml")
	listen := flag.String("listen", ":8080", "listen address")
	// 骨架回退：Family/SMB/Enterprise 档默认 pgvector/qdrant 未接线，
	// 显式 -backend-override sqlite 可先跑通装配链路（日志会打 WARN）。
	backendOverride := flag.String("backend-override", "", "override storage backend for skeleton run (sqlite)")
	flag.Parse()

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

	backend, err := kernel.NewStorageBackend(context.Background(), p)
	if err != nil {
		log.Fatalf("[boot] %v", err)
	}
	defer backend.Close()

	srv := kernel.NewAPIServer(p, backend)
	// 骨架 embedder：确定性哈希向量（跑通 §4.2 通路用，非真实语义模型）。
	// 维度取 profile 的 storage.embedding.dimensions（Family 档 1024），
	// 与 pgvector 表的 vector(1024) 对齐。真实场景替换为 BGE-large-zh 等。
	dim := 1024
	if p.Storage != nil && p.Storage.Embedding != nil && p.Storage.Embedding.Dimensions > 0 {
		dim = p.Storage.Embedding.Dimensions
	}
	srv.SetEmbedder(kernel.HashEmbedder(dim))
	// §3.10 生命周期策略：从 profile 读取（若 profile 暴露），否则默认
	// Family 档建议值（L0=200、L1=10000、TTL 60min/30d、L1PromoteAccess=10）。
	srv.SetLayerPolicy(kernel.DefaultLayerPolicy())

	// §3.4 多信号 rank 融合：装配默认 8 路信号（权重为占位默认值，待 A8 KPI 校准）。
	srv.SetRankFusion(kernel.DefaultRankFusion())

	// §3.13 后台整合 worker：启动 goroutine（默认 5 分钟；LIMS_CONSOLIDATE_INTERVAL 可覆盖，测试用短间隔）。
	cons := kernel.NewConsolidator(backend)
	if v := os.Getenv("LIMS_CONSOLIDATE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cons.SetInterval(d)
		} else {
			log.Printf("[boot] invalid LIMS_CONSOLIDATE_INTERVAL=%q, using default", v)
		}
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go cons.Start(bgCtx)

	log.Printf("[boot] listening on %s", *listen)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		log.Fatalf("[serve] %v", err)
	}
}
