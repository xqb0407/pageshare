// MCP 连接池：多 agent 同时连接时的会话治理。
//
// 官方 SDK 的 StreamableHTTPHandler 自管会话（Mcp-Session-Id + 空闲定时器），
// 但不暴露会话清单与容量钩子，所以池在 HTTP 中间件层实现：
//   - 注册：initialize 响应的 Mcp-Session-Id 回填进池，记录 agent（clientInfo）、
//     使用的密钥、连接时长与最后活跃；
//   - 容量：池满时拒绝新的 initialize（503 + Retry-After），已有会话不受影响；
//   - 回收：空闲超过 idleTTL 的会话出池（释放容量配额）；客户端 DELETE /mcp
//     显式关闭时立即出池。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// MaxMCPSessions 是连接池容量：同时在线的 MCP agent 会话上限。
	MaxMCPSessions = 10
	// mcpSessionIdleTTL 是会话空闲出池时间（只影响容量配额，SDK 侧会话由其自身管理）。
	mcpSessionIdleTTL = 30 * time.Minute
	// sessionHeader 是 Streamable HTTP 的会话标识头。
	sessionHeader = "Mcp-Session-Id"
)

type mcpSession struct {
	ID        string    `json:"-"`
	Agent     string    `json:"agent"`
	KeyID     string    `json:"key_id,omitempty"`
	KeyName   string    `json:"key_name"`
	Connected time.Time `json:"connected_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// mcpSessionPool 是活动会话登记簿（容量按 initialize 计）。
type mcpSessionPool struct {
	mu       sync.Mutex
	sessions map[string]*mcpSession
	gcOnce   sync.Once
}

func newMCPSessionPool() *mcpSessionPool {
	return &mcpSessionPool{sessions: map[string]*mcpSession{}}
}

func (p *mcpSessionPool) startGC() {
	p.gcOnce.Do(func() {
		go func() {
			for range time.Tick(time.Minute) {
				p.sweep()
			}
		}()
	})
}

func (p *mcpSessionPool) sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range p.sessions {
		if time.Since(s.LastSeen) > mcpSessionIdleTTL {
			delete(p.sessions, id)
		}
	}
}

func (p *mcpSessionPool) full() bool { return len(p.sessions) >= MaxMCPSessions }

func (p *mcpSessionPool) register(s *mcpSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[s.ID] = s
}

// touch 刷新会话活跃时间；会话不在池中（如刚被空闲回收）则忽略——
// 容量配额只在 initialize 时计算，不为任意会话头凭空造坑（防垃圾头占满池）。
func (p *mcpSessionPool) touch(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.LastSeen = time.Now()
	}
}

func (p *mcpSessionPool) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
}

func (p *mcpSessionPool) list() []mcpSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]mcpSession, 0, len(p.sessions))
	for _, s := range p.sessions {
		out = append(out, *s)
	}
	// 最后活跃倒序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastSeen.After(out[i].LastSeen) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// mcpIdentity 是通过鉴权的调用方身份（密钥池密钥或管理 token）。
type mcpIdentity struct {
	KeyID   string
	KeyName string
}

type mcpIdentityCtx struct{}

// mcpSessionPoolMid 包住 MCP handler：登记会话、限容量、回收空闲。
// 必须包在 mcpAuth 之内（依赖身份透传）。
func (s *Server) mcpSessionPoolMid(p *mcpSessionPool, next http.Handler) http.Handler {
	p.startGC()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ident, _ := r.Context().Value(mcpIdentityCtx{}).(mcpIdentity)
		if ident.KeyName == "" {
			ident = mcpIdentity{KeyName: "未知来源"}
		}

		switch {
		case r.Method == http.MethodDelete:
			// 客户端显式关闭：转发后立即出池
			if sid := r.Header.Get(sessionHeader); sid != "" {
				defer p.release(sid)
			}
			next.ServeHTTP(w, r)
			return

		case r.Method == http.MethodGet:
			// SSE 事件流：刷新活跃即可
			if sid := r.Header.Get(sessionHeader); sid != "" {
				p.touch(sid)
			}
			next.ServeHTTP(w, r)
			return
		}

		// POST：带会话头 = 已建立的会话，刷新活跃直接放行（容量只卡新会话）
		if sid := r.Header.Get(sessionHeader); sid != "" {
			p.touch(sid)
			next.ServeHTTP(w, r)
			return
		}

		// POST 无会话头：initialize 尝试。窥探请求体做容量检查并识别 agent。
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "请求体过大或不可读")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))

		var probe struct {
			Method string `json:"method"`
			Params struct {
				ClientInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"clientInfo"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &probe)
		isInitialize := probe.Method == "initialize"

		if isInitialize {
			p.mu.Lock()
			if len(p.sessions) >= MaxMCPSessions {
				p.mu.Unlock()
				w.Header().Set("Retry-After", "5")
				httpError(w, http.StatusServiceUnavailable,
					"MCP 连接池已满（上限 10 个会话），请稍后重试或联系管理员清理空闲会话")
				return
			}
			p.mu.Unlock()
		}

		next.ServeHTTP(w, r)

		// initialize 的响应头里带新会话 ID，回填登记
		if isInitialize {
			if sid := w.Header().Get(sessionHeader); sid != "" {
				agent := strings.TrimSpace(probe.Params.ClientInfo.Name)
				if agent == "" {
					agent = "未知 agent"
				}
				if v := strings.TrimSpace(probe.Params.ClientInfo.Version); v != "" {
					agent += " v" + v
				}
				now := time.Now()
				p.register(&mcpSession{
					ID: sid, Agent: agent, KeyID: ident.KeyID, KeyName: ident.KeyName,
					Connected: now, LastSeen: now,
				})
			}
		}
	})
}

// handleListMCPSessions GET /api/v1/mcp/sessions —— 活动会话清单（会话 ID 只给前 8 位）。
func (s *Server) handleListMCPSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := s.mcpPool.list()
	type row struct {
		ID         string `json:"id"`
		Agent      string `json:"agent"`
		KeyName    string `json:"key_name"`
		Connected  string `json:"connected_at"`
		LastSeen   string `json:"last_seen"`
	}
	out := make([]row, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, row{
			ID:        s.ID[:min(8, len(s.ID))],
			Agent:     s.Agent,
			KeyName:   s.KeyName,
			Connected: s.Connected.UTC().Format(time.RFC3339),
			LastSeen:  s.LastSeen.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "max": MaxMCPSessions})
}
