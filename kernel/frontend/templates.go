package frontend

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

//go:embed web/templates/*.html web/static/*
var webFS embed.FS

// templates 持有各页面已解析模板。
type templates struct {
	pages map[string]*template.Template
}

// LoadTemplates 解析 web/templates 下所有模板。
// 约定：layout.html 定义 "layout" 骨架（含导航 + "content" 占位），
// _partials.html 定义可复用片段；每个页面模板定义 "content"。
// 组合方式：layout + _partials + 该页面，然后执行 "layout"。
func LoadTemplates() (*templates, error) {
	entries, err := fs.ReadDir(webFS, "web/templates")
	if err != nil {
		return nil, fmt.Errorf("read templates dir: %w", err)
	}
	t := &templates{pages: make(map[string]*template.Template)}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" || name == "_partials.html" {
			continue
		}
		files := []string{"web/templates/layout.html", "web/templates/_partials.html", "web/templates/" + name}
		tpl, err := template.New(name).Funcs(funcMap()).ParseFS(webFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		t.pages[name] = tpl
	}
	return t, nil
}

func funcMap() template.FuncMap {
	return template.FuncMap{}
}

// render 执行指定页面模板（执行 "layout"）。
func (t *templates) render(w http.ResponseWriter, page string, data any) {
	tpl, ok := t.pages[page]
	if !ok {
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "layout", data); err != nil {
		// 响应可能已部分写出，不再写状态头。
		fmt.Printf("[frontend] render %s: %v\n", page, err)
	}
}

// serveStatic 提供内嵌静态资源（CSS/JS）。
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := "web/static" + strings.TrimPrefix(r.URL.Path, "/static")
	data, err := webFS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// renderBytes 把模板渲染到内存（测试 / 错误页用）。
func (t *templates) renderBytes(page string, data any) ([]byte, error) {
	tpl, ok := t.pages[page]
	if !ok {
		return nil, fmt.Errorf("template not found: %s", page)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}