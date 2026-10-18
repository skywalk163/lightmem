// lightmem-frontend 是 R23 前台服务：面向人的登录 UI + 个人记忆工作台 + 社区浏览。
// 独立部署，只通过 REST API / Authenticator 对接后台 lightmem-kernel，不改后台。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lightmem/kernel/frontend"
)

func main() {
	var (
		listen     = flag.String("listen", "127.0.0.1:8080", "前台服务监听地址（建议仅回环或反向代理后）")
		storePath  = flag.String("store", "./data/frontend.db", "前台自有凭据库路径（:memory: 为内存）")
		backend    = flag.String("backend", "", "后台业务端点 base URL，如 http://127.0.0.1:18444")
		admin      = flag.String("admin", "", "后台管理 API base URL，如 http://127.0.0.1:18090")
		svcToken   = flag.String("service-token", "", "前台自身 token（调后台 /admin/tokens/session 用）；空则读 LIMS_FRONTEND_SERVICE_TOKEN")
		tenant     = flag.String("tenant", "default", "注册用户默认租户")
		roles      = flag.String("roles", "user", "注册用户默认角色")
		timeout    = flag.Duration("backend-timeout", 10*time.Second, "调后台单次请求超时")
		sessionTTL = flag.Duration("session-ttl", 7*24*time.Hour, "会话有效期")
	)
	flag.Parse()

	if *svcToken == "" {
		*svcToken = os.Getenv("LIMS_FRONTEND_SERVICE_TOKEN")
	}
	if *svcToken == "" {
		log.Fatalf("[boot] -service-token（或 LIMS_FRONTEND_SERVICE_TOKEN）必填：前台需它代为签发用户 session token")
	}
	if *backend == "" || *admin == "" {
		log.Fatalf("[boot] -backend 与 -admin 必填：前台必须知道后台业务端点与管理 API 地址")
	}

	store, err := frontend.OpenStore(*storePath)
	if err != nil {
		log.Fatalf("[boot] open store: %v", err)
	}
	defer store.Close()

	bc := frontend.NewBackendClient(frontend.BackendConfig{
		BaseURL: *backend, AdminURL: *admin, ServiceToken: *svcToken, Timeout: *timeout,
	})
	tpl, err := frontend.LoadTemplates()
	if err != nil {
		log.Fatalf("[boot] load templates: %v", err)
	}

	srv, err := frontend.NewServer(frontend.Config{
		StorePath: *storePath, Listen: *listen,
		DefaultTenant: *tenant, DefaultRoles: *roles, SessionName: "lightmem_session", SessionTTL: *sessionTTL,
	}, store, bc, tpl)
	if err != nil {
		log.Fatalf("[boot] new server: %v", err)
	}

	httpServer := &http.Server{
		Addr: *listen, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
	}

	// 启动前探测后台可达性（不阻塞启动，仅告警）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := bc.Health(ctx); err != nil {
		log.Printf("[WARN] backend unreachable at boot: %v", err)
	} else {
		log.Printf("[boot] backend ok: %s (admin %s)", *backend, *admin)
	}
	cancel()

	go func() {
		log.Printf("[boot] lightmem-frontend listening on %s (store=%s, tenant=%s, roles=%s)",
			*listen, *storePath, *tenant, *roles)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[boot] serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("[shutdown] received signal, graceful draining")
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[shutdown] forced close: %v", err)
	}
	log.Printf("[shutdown] done")
}
