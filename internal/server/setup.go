package server

// 引导模式：首次部署（配置为空、非 -dev）时的独立轻量 HTTP 服务。
// 只暴露 setup 向导 API 与内嵌 SPA 入口页，不提供任何业务 API；
// 配置落盘后关闭 Done 通道，由 main 完成自重启进入正常模式。

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"pageshare/internal/config"
)

// SetupService 是引导模式服务，实现 http.Handler。
type SetupService struct {
	mux             *http.ServeMux
	listen          string
	configPath      string
	suggestedToken  string
	suggestedSecret string
	done            chan struct{}
	doneOnce        sync.Once
}

// NewSetupService 装配引导模式服务：listen 为监听地址（原样回显给向导），
// configPath 为向导完成后写入的配置文件路径。
func NewSetupService(listen, configPath string) *SetupService {
	s := &SetupService{
		mux:             http.NewServeMux(),
		listen:          listen,
		configPath:      configPath,
		suggestedToken:  config.RandomHex(16),
		suggestedSecret: config.RandomHex(32),
		done:            make(chan struct{}),
	}
	s.routes()
	return s
}

// Done 在配置成功落盘后关闭；main 据此自重启。
func (s *SetupService) Done() <-chan struct{} { return s.done }

// ServeHTTP 实现 http.Handler。
func (s *SetupService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *SetupService) routes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/setup/status", s.handleStatus)
	m.HandleFunc("POST /api/v1/setup/complete", s.handleComplete)
	// 业务 API 一律 404：引导模式只暴露上面两个 setup 端点
	m.HandleFunc("GET /api/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	// 其余 GET 路径全部回 SPA 入口页（向导是单页；命中真实静态资源时原样输出）
	m.HandleFunc("GET /{$}", s.serveIndex)
	m.HandleFunc("GET /admin/setup", s.serveIndex)
	m.HandleFunc("GET /admin/setup/", s.serveIndex)
	m.HandleFunc("GET /{path...}", s.serveIndex)
}

// handleStatus 报告引导状态与建议值；suggested_base_url 由请求 Host 推导。
func (s *SetupService) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"needs_setup":             true,
		"config_path":             s.configPath,
		"suggested_auth_token":    s.suggestedToken,
		"suggested_cookie_secret": s.suggestedSecret,
		"listen":                  s.listen,
		"suggested_base_url":      s.baseURLFromRequest(r),
	})
}

// baseURLFromRequest 用请求的 Host 推导 base url：scheme 按
// X-Forwarded-Proto（反代场景）与 r.TLS 判断，默认 http。
func (s *SetupService) baseURLFromRequest(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = displayHost(s.listen)
	}
	return scheme + "://" + host
}

// setupCompleteRequest 是向导提交的完整配置。
type setupCompleteRequest struct {
	AuthToken        string         `json:"auth_token"`
	CookieSecret     string         `json:"cookie_secret"`
	PublicBaseURL    string         `json:"public_base_url"`
	SiteWildcardHost string         `json:"site_wildcard_host"`
	Listen           string         `json:"listen"`
	DBPath           string         `json:"db_path"`
	MaxUploadMB      int64          `json:"max_upload_mb"`
	Storage          config.Storage `json:"storage"`
}

func (s *SetupService) handleComplete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	var req setupCompleteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httpError(w, http.StatusBadRequest, "请求体不是有效 JSON")
		return
	}

	// 从 Defaults 出发叠加请求值，再走统一校验（尾斜杠规范化等）
	cfg := config.Defaults()
	if req.Listen != "" {
		cfg.Listen = req.Listen
	}
	cfg.PublicBaseURL = strings.TrimSpace(req.PublicBaseURL)
	cfg.SiteWildcardHost = req.SiteWildcardHost
	cfg.AuthToken = strings.TrimSpace(req.AuthToken)
	cfg.CookieSecret = strings.TrimSpace(req.CookieSecret)
	if req.DBPath != "" {
		cfg.DBPath = req.DBPath
	}
	if req.MaxUploadMB > 0 {
		cfg.MaxUploadMB = req.MaxUploadMB
	}
	if req.Storage.Backend != "" {
		cfg.Storage = req.Storage
	}
	if err := cfg.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := cfg.Save(s.configPath); err != nil {
		httpError(w, http.StatusInternalServerError, "写入配置文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config_path": s.configPath})

	// 留出响应送达客户端的时间，再通知 main 自重启
	time.Sleep(200 * time.Millisecond)
	s.doneOnce.Do(func() { close(s.done) })
}

// serveIndex 是引导模式的 SPA 兜底：命中内嵌静态资源时原样输出，
// 其余路径（含 /admin/setup）回入口页 200——向导是单页。
func (s *SetupService) serveIndex(w http.ResponseWriter, r *http.Request) {
	req := strings.TrimPrefix(r.URL.Path, "/admin")
	if req == "" || req == "/" {
		req = "/index.html"
	}
	name := strings.TrimPrefix(path.Clean(req), "/")
	if name != "" && name != "." {
		if data, err := fs.ReadFile(webDist, "web/dist/"+name); err == nil {
			if ct := staticContentType(name); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
			return
		}
	}
	serveIndexHTML(w)
}

// serveIndexHTML 输出内嵌 SPA 入口页（引导模式兜底）。
func serveIndexHTML(w http.ResponseWriter) {
	data, err := fs.ReadFile(webDist, "web/dist/index.html")
	if err != nil {
		http.Error(w, "管理 UI 未构建", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// displayHost 把监听地址转成可点击的 host:port：":8300" → "localhost:8300"。
func displayHost(listen string) string {
	if listen == "" {
		return "localhost:8300"
	}
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}
