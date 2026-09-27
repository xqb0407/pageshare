// Package server 装配 HTTP 面：管理 API、公开分享服务、密码门与内嵌管理 UI。
package server

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pageshare/internal/config"
	"pageshare/internal/mcpserver"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// Deps 是服务的全部依赖。
type Deps struct {
	Cfg     config.Config
	Store   *store.Store
	Storage storage.Storage
	Logger  *slog.Logger
}

// Server 持有依赖并实现 http.Handler。
type Server struct {
	Deps
	mux *http.ServeMux

	// 发布锁池：newSiteMu 串行化建站（含 id 分配，消除查后建竞态）；siteLocks
	// 按 siteID 给覆盖发布上互斥锁，防止多 agent 并发发布同一站点时版本互相踩踏。
	newSiteMu sync.Mutex
	siteLocks sync.Map // siteID(lower) -> *sync.Mutex

	// MCP 连接池：登记活动 agent 会话，限容量、回收空闲（见 mcpsessions.go）。
	mcpPool *mcpSessionPool

	// 页览内存计数器：访客请求只加计数，后台按分钟并入元数据（见 stats.go）。
	visits *visitTracker

	// 两阶段上传票据表（内存态，重启即失效；见 uploads.go）。
	uploads *uploadRegistry

	// P1-5 治理：每密钥滑动窗口限流 + 操作审计（见 gate.go / audit.go）。
	limiter *keyLimiter
	audit   *auditWriter
}

// lockSite 返回某个站点的发布互斥锁（锁池：按需创建，随站点数有界增长）。
func (s *Server) lockSite(siteID string) *sync.Mutex {
	v, _ := s.siteLocks.LoadOrStore(strings.ToLower(siteID), &sync.Mutex{})
	return v.(*sync.Mutex)
}

// New 装配全部路由。
func New(d Deps) *Server {
	s := &Server{Deps: d, mux: http.NewServeMux(), mcpPool: newMCPSessionPool(), visits: newVisitTracker(), uploads: newUploadRegistry(),
		limiter: newKeyLimiter(), audit: newAuditWriter(filepath.Dir(d.Cfg.DBPath))}
	s.routes()
	return s
}

// ServeHTTP 顶一层泛域名改写：Host 形如 {id}.<site_wildcard_host> 时，
// 内部改写为 /s/{id}/{path} 再进 mux——站点在自己域名上以根路径托管，
// Vite base:/ 产物（绝对路径 /assets/...）零改动即可运行，语义等同 nginx。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.SiteWildcardHost != "" {
		// 保留根路径（管理 API/UI、健康检查、回源通道）不参与站点改写
		first := r.URL.Path
		if i := strings.IndexByte(first[1:], '/'); i >= 0 {
			first = first[:1+i]
		}
		reserved := map[string]bool{
			"/s": true, "/api": true, "/admin": true, "/mcp": true,
			"/healthz": true, "/__memstore": true, "/domain-ask": true,
		}
		if !reserved[first] {
			if id, ok := wildcardSiteID(r.Host, s.Cfg.SiteWildcardHost); ok {
				prefix := "/s/" + strings.ToLower(id)
				// 幂等：路径已带站点前缀（如密码门表单的绝对 action）不再叠加
				if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
					r.URL.Path = prefix + r.URL.Path
				}
				if r.URL.RawPath != "" && r.URL.RawPath != prefix && !strings.HasPrefix(r.URL.RawPath, prefix+"/") {
					r.URL.RawPath = prefix + r.URL.RawPath
				}
			}
		}
	}
	s.mux.ServeHTTP(w, r)
}

// wildcardSiteID 从 Host（可带端口）解析站点 id；不属于泛域名后缀时返回 false。
func wildcardSiteID(host, suffix string) (string, bool) {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	if !strings.HasSuffix(h, "."+suffix) {
		return "", false
	}
	id := strings.TrimSuffix(h, "."+suffix)
	if id == "" || strings.Contains(id, ".") {
		return "", false // 只允许单级 {id}，二级以上不认
	}
	return id, true
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)

	// 管理 API（Bearer token）
	m.Handle("POST /api/v1/sites", s.admin(s.handleCreateSite))
	m.Handle("GET /api/v1/sites", s.admin(s.handleListSites))
	m.Handle("GET /api/v1/sites/{id}", s.admin(s.handleGetSite))
	m.Handle("PUT /api/v1/sites/{id}", s.admin(s.handlePublishToSite))
	m.Handle("PATCH /api/v1/sites/{id}", s.admin(s.handlePatchSite))
	m.Handle("DELETE /api/v1/sites/{id}", s.admin(s.handleDeleteSite))
	// 两阶段上传数据面（begin_upload → PUT 归档字节 → commit_upload）：
	// 远程 agent 持 psm_ 密钥，故走 mcpAuth（管理 token 或密钥池均可过）
	m.Handle("PUT /api/v1/uploads/{id}", s.mcpAuth(http.HandlerFunc(s.handlePutUpload)))
	// Caddy on_demand TLS 的 ask 端点：域名对应的站点存在才发证书
	m.HandleFunc("GET /domain-ask", s.handleDomainAsk)

	// 公开分享面
	m.HandleFunc("GET /s/{id}", s.handleNoSlash)
	m.HandleFunc("GET /s/{id}/gate", s.handleGatePage)
	m.HandleFunc("POST /s/{id}/gate", s.handleGatePost)
	m.HandleFunc("GET /s/{id}/{path...}", s.handleServeSite)

	// 管理 UI（内嵌静态）
	m.Handle("GET /admin/{$}", http.HandlerFunc(s.adminStatic))
	m.Handle("GET /admin", http.HandlerFunc(s.redirectAdminSlash))
	// 已配置时向导页回管理台（精确路由优先于 /admin/{path...}；引导模式由独立服务接管）
	m.HandleFunc("GET /admin/setup", s.redirectSetupDone)
	m.HandleFunc("GET /admin/setup/", s.redirectSetupDone)
	m.Handle("GET /admin/{path...}", http.HandlerFunc(s.adminStatic))
	// 其余未知路径也交给管理 UI 的 SPA 兜底（adminStatic 会带 404 状态码，前端渲染动画 404 页）
	m.Handle("GET /{path...}", http.HandlerFunc(s.adminStatic))

	// mem/disk 后端的 presigned 同源回源（仅本地模式挂载）
	if mem, ok := s.Storage.(*storage.Mem); ok {
		m.Handle("GET /__memstore/{key...}", memStoreHandler(mem))
	}
	if disk, ok := s.Storage.(*storage.Disk); ok {
		m.Handle("GET /__diskstore/{key...}", disk.Handler())
	}

	// MCP over HTTP（Streamable HTTP，标准 JSON-RPC 2.0）。
	// 鉴权（管理 token 或密钥池密钥）→ 连接池（登记会话/限容量/空闲回收）。
	mcpHandler := mcpserver.NewStreamableHTTP(s)
	pooled := s.mcpSessionPoolMid(s.mcpPool, http.HandlerFunc(mcpHandler.ServeHTTP))
	m.Handle("POST /mcp", s.mcpAuth(pooled))
	m.Handle("GET /mcp", s.mcpAuth(pooled))
	m.Handle("DELETE /mcp", s.mcpAuth(pooled))

	// MCP 密钥池 + 连接池管理（Bearer 管理 token；控制台 /admin/mcp 调用）
	m.Handle("GET /api/v1/mcp/keys", s.admin(s.handleListMCPKeys))
	m.Handle("POST /api/v1/mcp/keys", s.admin(s.handleCreateMCPKey))
	m.Handle("DELETE /api/v1/mcp/keys/{id}", s.admin(s.handleDeleteMCPKey))
	m.Handle("GET /api/v1/mcp/sessions", s.admin(s.handleListMCPSessions))
	// MCP 操作审计查询（P1-5）：读 audit.jsonl，支持 site/key/since/limit 过滤
	m.Handle("GET /api/v1/mcp/audit", s.admin(s.handleMCPAudit))

	// 根路径 → 管理 UI
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDomainAsk 供 Caddy on_demand_tls ask 使用：泛域名下的站点存在才允许签证书。
func (s *Server) handleDomainAsk(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(r.URL.Query().Get("domain"))
	if s.Cfg.SiteWildcardHost == "" || !strings.HasSuffix(domain, "."+s.Cfg.SiteWildcardHost) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	id := strings.TrimSuffix(domain, "."+s.Cfg.SiteWildcardHost)
	if _, err := s.Store.GetSite(strings.ToUpper(id)); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleNoSlash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	http.Redirect(w, r, "/s/"+id+"/", http.StatusMovedPermanently)
}

func (s *Server) redirectAdminSlash(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
}

// redirectSetupDone 在正常模式（已配置）下把向导页重定向回管理台：
// 引导只在首次部署存在，配置完成后 setup 面即关闭。
func (s *Server) redirectSetupDone(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// sitePrefix 返回某站点某版本在对象存储里的 key 前缀。
func (s *Server) sitePrefix(siteID string, version int64) string {
	return siteID + "/" + itoa(version) + "/"
}

// PresignTTL 是 presigned URL 的有效期；对访客而言只是跳转页面的生存期。
const PresignTTL = 5 * time.Minute
