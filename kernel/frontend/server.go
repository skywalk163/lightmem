package frontend

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
)

// Config 是前台服务配置。
type Config struct {
	StorePath     string // 前台自有库路径
	Listen        string // HTTP 监听地址
	DefaultTenant string // 注册用户默认租户
	DefaultRoles  string // 注册用户默认角色（如 "user"）
	SessionName   string // session cookie 名
	SessionTTL    time.Duration
}

// Server 是前台服务：人登录 UI + 工作台 + 社区浏览，独立部署。
type Server struct {
	store   *Store
	backend *backendClient
	tpl     *templates
	cfg     Config
	now     func() time.Time
}

// NewServer 构造前台服务。
func NewServer(cfg Config, store *Store, backend *backendClient, tpl *templates) (*Server, error) {
	if cfg.SessionName == "" {
		cfg.SessionName = "lightmem_session"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.DefaultTenant == "" {
		cfg.DefaultTenant = "default"
	}
	if cfg.DefaultRoles == "" {
		cfg.DefaultRoles = "user"
	}
	if store == nil {
		return nil, errors.New("frontend: store required")
	}
	return &Server{store: store, backend: backend, tpl: tpl, cfg: cfg, now: time.Now}, nil
}

// Handler 返回前台完整 HTTP handler（含 /api 代理与 /static 静态资源）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.csrf(s.handleLoginSubmit))
	mux.HandleFunc("GET /register", s.handleRegisterForm)
	mux.HandleFunc("POST /register", s.csrf(s.handleRegisterSubmit))
	mux.HandleFunc("POST /logout", s.csrf(s.requireLogin(s.handleLogout)))
	mux.HandleFunc("GET /workspace", s.requireLogin(s.handleWorkspace))
	mux.HandleFunc("GET /goals", s.requireLogin(s.handleGoals))
	mux.HandleFunc("GET /discover", s.requireLogin(s.handleDiscover))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// API 代理：鉴权由 requireLogin 中间件（读 session cookie）保证。
	mux.HandleFunc("POST /api/memory/recall", s.requireLogin(s.handleAPIRecall))
	mux.HandleFunc("POST /api/memory/save", s.requireLogin(s.handleAPISave))
	mux.HandleFunc("GET /api/goals", s.requireLogin(s.handleAPIGoals))
	mux.HandleFunc("POST /api/goals/create", s.requireLogin(s.handleAPIGoalCreate))
	mux.HandleFunc("POST /api/goals/{id}/progress", s.requireLogin(s.handleAPIGoalProgress))
	mux.HandleFunc("POST /api/goals/{id}/state", s.requireLogin(s.handleAPIGoalState))
	mux.HandleFunc("GET /api/discover", s.requireLogin(s.handleAPIDiscover))

	mux.HandleFunc("GET /static/", serveStatic)
	return mux
}

// requireLogin 要求请求带有效 session cookie，否则重定向到 /login。
func (s *Server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// csrf 校验 POST 请求的表单 _csrf 是否与 cookie csrf 一致（双提交模式）。
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got := r.FormValue("_csrf")
			want := s.csrfToken(r)
			if got == "" || want == "" || got != want {
				http.Error(w, "csrf mismatch", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// hasSession 判断请求是否带 session cookie。
func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(s.cfg.SessionName)
	return err == nil && c.Value != ""
}

// sessionToken 返回 session cookie 里的后台 token 明文。
func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.cfg.SessionName)
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookie 写入/清除 session cookie（HttpOnly + SameSite=Lax）。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.isSecure(r),
	})
}

func (s *Server) isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// csrfToken 取或生成 CSRF token（存 cookie，模板输出）。
func (s *Server) csrfToken(r *http.Request) string {
	c, err := r.Cookie("lightmem_csrf")
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
}

// ensureCSRF 为 GET 页面准备 CSRF：无 cookie 则生成并设置。
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if tok := s.csrfToken(r); tok != "" {
		return tok
	}
	tok := newSecret()
	http.SetCookie(w, &http.Cookie{
		Name:     "lightmem_csrf",
		Value:    tok,
		Path:     "/",
		HttpOnly: false, // 模板需读取，交给双提交校验
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// backendReady 返回后端客户端（未配置则为 nil）。
func (s *Server) backendReady() *backendClient {
	if s.backend != nil && s.backend.IsConfigured() {
		return s.backend
	}
	return nil
}

// proxy 把后台响应透传给前台客户端。
func proxy(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) bgctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 15*time.Second)
}

func logErr(prefix string, err error) {
	if err != nil {
		log.Printf("[frontend] %s: %v", prefix, err)
	}
}