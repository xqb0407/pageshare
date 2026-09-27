// MCP 接入密钥池：签发/吊销 API + /mcp 鉴权中间件。
//
// 池化思路：不把管理 token 发给任何 MCP 客户端，而是为每个客户端从池里签发
// 一把独立密钥（psm_ 前缀，明文只在签发时返回一次，落盘只存 SHA-256）。
// 任何一把被吊销都不影响池里其余密钥；按密钥记录最近使用时间，控制台可见。
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"pageshare/internal/store"
)

// mcpKeyPrefix 是密钥明文前缀；鉴权时用它快速分流（非该前缀不查库）。
const mcpKeyPrefix = "psm_"

// MaxMCPKeys 是密钥池容量上限（防止无限签发）。
const MaxMCPKeys = 20

// touchInterval 限制 last_used_at 的回写频率，避免高频调用打穿存储。
const touchInterval = time.Minute

// newMCPKey 生成一把新密钥：明文 psm_<40 hex>、展示前缀（前 12 位）、SHA-256 哈希。
func newMCPKey() (plain, prefix, hash string, err error) {
	buf := make([]byte, 20)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plain = mcpKeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	return plain, plain[:12], hex.EncodeToString(sum[:]), nil
}

func hashMCPKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func newShortID() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf) // 8 hex
}

// mcpAuth 包住 /mcp：管理 token 或池内任一有效密钥皆可过；命中密钥时回写最近使用，
// 并把调用方身份放进 context 供连接池归因会话。
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.AuthToken)) == 1 {
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), mcpIdentityCtx{}, mcpIdentity{KeyName: "管理 token"})))
			return
		}
		if strings.HasPrefix(token, mcpKeyPrefix) {
			if k, err := s.Store.FindMCPKey(hashMCPKey(token)); err == nil {
				s.touchMCPKey(k)
				s.Logger.Debug("mcp 请求经密钥池鉴权", "key", k.Name)
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpIdentityCtx{},
					mcpIdentity{KeyID: k.ID, KeyName: k.Name})))
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="pageshare"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要有效的 Bearer token 或 MCP 密钥"})
	})
}

// touchMCPKey 异步回写最近使用时间；超过 touchInterval 才写一次。
func (s *Server) touchMCPKey(k *store.MCPKey) {
	if time.Since(k.LastUsedAt) < touchInterval {
		return
	}
	go func(id string) {
		_ = s.Store.TouchMCPKey(id, time.Now())
	}(k.ID)
}

// ---- 管理 API（Bearer 管理 token） ----

type mcpKeyJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes,omitempty"` // nil/空 = 全权（升级前存量密钥的兼容语义）
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
}

func toKeyJSON(k *store.MCPKey) mcpKeyJSON {
	j := mcpKeyJSON{
		ID:        k.ID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Scopes:    k.Scopes,
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !k.LastUsedAt.IsZero() {
		j.LastUsedAt = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return j
}

// handleListMCPKeys GET /api/v1/mcp/keys —— 列出池内全部密钥（不含哈希/明文）。
func (s *Server) handleListMCPKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := s.Store.ListMCPKeys()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "读取密钥池失败")
		return
	}
	out := make([]mcpKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleCreateMCPKey POST /api/v1/mcp/keys {"name": "...", "scopes": ["publish","read"]}
// —— 签发新密钥，明文只在本次响应返回。scopes 缺省为 publish+read（删除永不含）；
// 传非法 scope 名直接 400，不静默降级。
func (s *Server) handleCreateMCPKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, `请求体应为 {"name": "...", "scopes": [...]}`)
		return
	}
	name := []rune(strings.TrimSpace(in.Name))
	if len(name) == 0 {
		name = []rune("未命名密钥")
	}
	if len(name) > 40 {
		name = name[:40]
	}
	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	keys, err := s.Store.ListMCPKeys()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "读取密钥池失败")
		return
	}
	if len(keys) >= MaxMCPKeys {
		httpError(w, http.StatusBadRequest, "密钥池已满（上限 20 把），请先删除不再使用的密钥")
		return
	}

	plain, prefix, hash, err := newMCPKey()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	k := &store.MCPKey{ID: newShortID(), Name: string(name), Prefix: prefix, KeyHash: hash, Scopes: scopes}
	if err := s.Store.CreateMCPKey(k); err != nil {
		httpError(w, http.StatusInternalServerError, "保存密钥失败")
		return
	}
	out := toKeyJSON(k)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": out.ID, "name": out.Name, "prefix": out.Prefix,
		"scopes": out.Scopes, "created_at": out.CreatedAt, "token": plain,
	})
}

// handleDeleteMCPKey DELETE /api/v1/mcp/keys/{id} —— 从池中删除，持有方立即失效。
func (s *Server) handleDeleteMCPKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteMCPKey(id); err != nil {
		if err == store.ErrNotFound {
			httpError(w, http.StatusNotFound, "密钥不存在")
			return
		}
		httpError(w, http.StatusInternalServerError, "删除密钥失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}
