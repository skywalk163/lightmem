package frontend

import (
	"errors"
	"log"
	"net/http"
	"strings"
)

// viewUser 是模板可见的用户视图（不暴露任何敏感字段）。
type viewUser struct {
	Username    string
	TenantID    string
	DisplayName string
}

// pageData 是页面模板的通用数据。
type pageData struct {
	User  *viewUser
	CSRF  string
	Flash string
	Error string
	Query string
}

func (s *Server) newPageData(w http.ResponseWriter, r *http.Request) pageData {
	return pageData{User: s.viewUser(r), CSRF: s.ensureCSRF(w, r)}
}

// viewUser 从非敏感 cookie 还原当前用户（仅用于导航显示；真实鉴权靠后台 token）。
func (s *Server) viewUser(r *http.Request) *viewUser {
	c, err := r.Cookie("lightmem_user")
	if err != nil || c.Value == "" {
		return nil
	}
	parts := strings.SplitN(c.Value, "|", 2)
	u := &viewUser{Username: parts[0]}
	if len(parts) >= 2 {
		u.DisplayName = parts[1]
	}
	if len(parts) >= 3 {
		u.TenantID = parts[2]
	}
	return u
}

func (s *Server) setUserCookie(w http.ResponseWriter, username, display, tenant string) {
	http.SetCookie(w, &http.Cookie{
		Name: "lightmem_user", Value: username + "|" + display + "|" + tenant, Path: "/",
		MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// ---- 页面 handler ----

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	s.tpl.render(w, "home.html", s.newPageData(w, r))
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/workspace", http.StatusSeeOther)
		return
	}
	s.tpl.render(w, "login.html", s.newPageData(w, r))
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	fail := func(msg string) {
		d := s.newPageData(w, r)
		d.Error = msg
		d.User = nil
		s.tpl.render(w, "login.html", d)
	}
	if username == "" || password == "" {
		fail("请输入用户名和密码")
		return
	}
	cred, err := s.store.GetCredential(r.Context(), username)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			fail("用户名或密码错误")
			return
		}
		log.Printf("[frontend] login get credential %q: %v", username, err)
		fail("登录失败，请稍后重试")
		return
	}
	if !verifyPassword(password, cred.PasswordHash) {
		fail("用户名或密码错误")
		return
	}
	if cred.DisabledAt != "" {
		fail("账号已停用")
		return
	}
	b := s.backendReady()
	if b == nil {
		log.Printf("[frontend] login: backend not configured; cannot issue session")
		fail("服务暂不可用")
		return
	}
	ctx, cancel := s.bgctx(r)
	defer cancel()
	token, sid, err := b.IssueSession(ctx, cred.Username, cred.TenantID, s.cfg.DefaultRoles, "")
	if err != nil {
		log.Printf("[frontend] issue session for %q: %v", cred.Username, err)
		fail("登录失败，请稍后重试")
		return
	}
	s.setSessionCookie(w, r, token, int(s.cfg.SessionTTL.Seconds()))
	s.setSessionIDCookie(w, sid)
	s.setUserCookie(w, cred.Username, cred.DisplayName, cred.TenantID)
	if err := s.store.RecordLogin(r.Context(), cred.Username, clientIP(r), r.UserAgent()); err != nil {
		logErr("record login", err)
	}
	http.Redirect(w, r, "/workspace", http.StatusSeeOther)
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/workspace", http.StatusSeeOther)
		return
	}
	s.tpl.render(w, "register.html", s.newPageData(w, r))
}

func (s *Server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	display := strings.TrimSpace(r.FormValue("display_name"))
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	fail := func(msg string) {
		d := s.newPageData(w, r)
		d.Error = msg
		d.User = nil
		s.tpl.render(w, "register.html", d)
	}
	if len(username) < 3 {
		fail("用户名至少 3 位")
		return
	}
	if len(password) < 8 {
		fail("密码至少 8 位")
		return
	}
	if password != password2 {
		fail("两次输入的密码不一致")
		return
	}
	hash, err := hashPassword(password, passwordHashIter)
	if err != nil {
		logErr("hash password", err)
		fail("注册失败，请稍后重试")
		return
	}
	err = s.store.CreateCredential(r.Context(), Credential{
		Username: username, PasswordHash: hash, TenantID: s.cfg.DefaultTenant, DisplayName: display,
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			fail("用户名已存在")
			return
		}
		logErr("create credential", err)
		fail("注册失败，请稍后重试")
		return
	}
	b := s.backendReady()
	if b == nil {
		log.Printf("[frontend] register: backend not configured; cannot issue session")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx, cancel := s.bgctx(r)
	defer cancel()
	token, sid, err := b.IssueSession(ctx, username, s.cfg.DefaultTenant, s.cfg.DefaultRoles, "")
	if err != nil {
		logErr("issue session after register", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.setSessionCookie(w, r, token, int(s.cfg.SessionTTL.Seconds()))
	s.setSessionIDCookie(w, sid)
	s.setUserCookie(w, username, display, s.cfg.DefaultTenant)
	logErr("record login", s.store.RecordLogin(r.Context(), username, clientIP(r), r.UserAgent()))
	http.Redirect(w, r, "/workspace", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.sessionID(r); sid != "" {
		if b := s.backendReady(); b != nil {
			ctx, cancel := s.bgctx(r)
			defer cancel()
			logErr("revoke session", b.RevokeSession(ctx, sid))
		}
	}
	s.clearSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	s.tpl.render(w, "workspace.html", s.newPageData(w, r))
}

func (s *Server) handleGoals(w http.ResponseWriter, r *http.Request) {
	s.tpl.render(w, "goals.html", s.newPageData(w, r))
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	s.tpl.render(w, "discover.html", s.newPageData(w, r))
}

// ---- session cookie 辅助 ----

func (s *Server) setSessionIDCookie(w http.ResponseWriter, sid string) {
	http.SetCookie(w, &http.Cookie{
		Name: "lightmem_sid", Value: sid, Path: "/",
		MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) sessionID(r *http.Request) string {
	c, err := r.Cookie("lightmem_sid")
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{s.cfg.SessionName, "lightmem_sid", "lightmem_user"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name != "lightmem_user"})
	}
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.IndexByte(xf, ','); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	return r.RemoteAddr
}
