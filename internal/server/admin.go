package server

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"pageshare/internal/storage"
)

// webDist 是前端构建产物；仓库里始终提交一份可用的占位 index.html，
// 正式 UI 用 frontend 的构建输出覆盖（见 README）。
//
//go:embed all:web/dist
var webDist embed.FS

func (s *Server) adminStatic(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		http.Error(w, "embed error", http.StatusInternalServerError)
		return
	}
	req := strings.TrimPrefix(r.URL.Path, "/admin")
	if req == "" || req == "/" {
		req = "/index.html"
	}
	f, err := dist.Open(strings.TrimPrefix(path.Clean(req), "/"))
	notFound := false
	if err != nil {
		// SPA 兜底：未知路径回 index.html，但保留 404 状态码，前端据此渲染动画 404 页
		f, err = dist.Open("index.html")
		if err != nil {
			http.Error(w, "管理 UI 未构建", http.StatusInternalServerError)
			return
		}
		req = "/index.html"
		notFound = true
	}
	defer f.Close()
	data, err := fs.ReadFile(dist, strings.TrimPrefix(path.Clean(req), "/"))
	if err != nil {
		http.Error(w, "读取失败", http.StatusInternalServerError)
		return
	}
	if ct := staticContentType(req); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	if notFound {
		w.WriteHeader(http.StatusNotFound)
	}
	_, _ = w.Write(data)
}

// staticContentType 按扩展名给出静态资源的 Content-Type；未知类型返回空串。
func staticContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	}
	return ""
}

// memStoreHandler 在 mem 后端下为 presigned 同源路径提供回源（仅 dev/e2e 挂载）。
func memStoreHandler(mem *storage.Mem) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/__memstore/")
		obj, err := mem.Get(r.Context(), key)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer obj.Body.Close()
		if obj.ContentType != "" {
			w.Header().Set("Content-Type", obj.ContentType)
		}
		_, _ = io.Copy(w, obj.Body)
	})
}
