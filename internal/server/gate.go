// MCP 工具治理（P1-5）：密钥 scope 门禁 + 每密钥滑动窗口限流 + 操作审计入口。
// 拦截点在 mcpserver 的接收中间件（tools/call）：header 来自原始 HTTP 请求
// （go-sdk RequestExtra.Header，已验证 v1.8.0 透传），身份在此再解析一次——
// 会话级 context 不可靠（streamable 会话跨多个 HTTP 请求），头是唯一稳定载体。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"pageshare/internal/store"
)

// Scope 常量。设计：publish=建/覆盖/update/rollback，read=查，delete=删站。
const (
	ScopePublish = "publish"
	ScopeRead    = "read"
	ScopeDelete  = "delete"
)

// DefaultKeyScopes 是新签发密钥的缺省授权面：publish+read，删除永远要显式加。
var DefaultKeyScopes = []string{ScopePublish, ScopeRead}

// ValidScopes 是全部合法 scope（签发 API 校验用）。
var ValidScopes = []string{ScopePublish, ScopeRead, ScopeDelete}

// normalizeScopes 校验并归一化签发请求里的 scope 列表：大小写不敏感、去重、
// 保持首次出现顺序；空列表 → DefaultKeyScopes（新密钥永不默认拿到 delete）。
func normalizeScopes(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		sc := strings.ToLower(strings.TrimSpace(raw))
		if sc == "" {
			continue
		}
		valid := false
		for _, v := range ValidScopes {
			if sc == v {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("未知 scope %q（可选: publish / read / delete）", raw)
		}
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		out = append(out, DefaultKeyScopes...)
	}
	return out, nil
}

// toolRequiredScope 把 MCP 工具映射到所需 scope。不在表里的工具不设 scope 门槛
// （限流与审计仍生效）——新增工具必须在此登记，防「忘了配门禁」变成静默全权。
var toolRequiredScope = map[string]string{
	"publish_site":       ScopePublish,
	"begin_upload":       ScopePublish,
	"commit_upload":      ScopePublish,
	"update_site":        ScopePublish,
	"rollback_site":      ScopePublish,
	"list_sites":         ScopeRead,
	"get_site":           ScopeRead,
	"get_site_stats":     ScopeRead,
	"list_site_versions": ScopeRead,
	"delete_site":        ScopeDelete,
}

// 每密钥限流窗口：一分钟。总量防刷爆控制面，发布类防刷爆对象存储请求费。
const (
	rateWindow = time.Minute
	rateTotal  = 60
	ratePub    = 10
)

// keyLimiter 是每密钥的双滑动窗口（总 / 发布类）。纯内存，重启清零——
// 限流是减震器不是账本，与审计不同步可接受。
type keyLimiter struct {
	mu     sync.Mutex
	hits   map[[2]string][]time.Time // [keyID, class] 或 [keyID, ""]（总） -> 命中时间
	pruned time.Time
}

func newKeyLimiter() *keyLimiter {
	return &keyLimiter{hits: map[[2]string][]time.Time{}}
}

// hit 判定并记账；被拒时返回建议的 retry-after。
func (l *keyLimiter) hit(keyID string, isPublish bool) (time.Duration, bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// 惰性全表清理：一分钟一次足够（窗口只有一分钟长）
	if now.Sub(l.pruned) > time.Minute {
		for k, ts := range l.hits {
			kept := trimWindow(ts, now)
			if len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.pruned = now
	}
	totalKey := [2]string{keyID, ""}
	l.hits[totalKey] = trimWindow(l.hits[totalKey], now)
	if len(l.hits[totalKey]) >= rateTotal {
		return retryAfter(l.hits[totalKey], now), false
	}
	if isPublish {
		pubKey := [2]string{keyID, ScopePublish}
		l.hits[pubKey] = trimWindow(l.hits[pubKey], now)
		if len(l.hits[pubKey]) >= ratePub {
			return retryAfter(l.hits[pubKey], now), false
		}
		l.hits[pubKey] = append(l.hits[pubKey], now)
	}
	l.hits[totalKey] = append(l.hits[totalKey], now)
	return 0, true
}

func trimWindow(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < rateWindow {
			out = append(out, t)
		}
	}
	return out
}

func retryAfter(ts []time.Time, now time.Time) time.Duration {
	if len(ts) == 0 {
		return time.Second
	}
	d := rateWindow - now.Sub(ts[0])
	if d < time.Second {
		d = time.Second
	}
	return d.Round(time.Second)
}

// Authorize 实现 mcpserver.Backend：工具调用的前置门禁（鉴权兜底 + scope + 限流）。
// /mcp 外层已有 mcpAuth 验证过凭据合法性；这里是第二道——即使被绕过中间件，
// 身份与权限也只认原始 HTTP 头，不依赖任何会话语义。
func (s *Server) Authorize(header http.Header, tool string) error {
	if header == nil {
		return nil // stdio / InMemory：本地可信（mcp -dev / 本地客户端）
	}
	token := strings.TrimSpace(strings.TrimPrefix(header.Get("Authorization"), "Bearer "))
	switch {
	case token == "":
		// 无凭据：交由外层 mcpAuth 拒绝，门禁这里不重复报错
		return nil
	case subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.AuthToken)) == 1:
		return nil // 管理 token：全权不限流
	case strings.HasPrefix(token, mcpKeyPrefix):
		k, err := s.Store.FindMCPKey(hashMCPKey(token))
		if err != nil {
			return fmt.Errorf("密钥无效或已吊销")
		}
		if req := toolRequiredScope[tool]; req != "" && !keyHasScope(k, req) {
			return fmt.Errorf("密钥 %s 缺少 %s 权限（现有 scopes: %s）", k.Name, req, strings.Join(k.Scopes, ","))
		}
		if d, ok := s.limiter.hit(k.ID, toolIsPublish(tool)); !ok {
			return fmt.Errorf("密钥 %s 请求过快，请 %ds 后重试（Retry-After: %ds）", k.Name, int(d.Seconds()), int(d.Seconds()))
		}
		return nil
	default:
		return fmt.Errorf("无法识别的凭据")
	}
}

func keyHasScope(k *store.MCPKey, scope string) bool {
	if len(k.Scopes) == 0 {
		return true
	}
	for _, sc := range k.Scopes {
		if strings.EqualFold(sc, scope) {
			return true
		}
	}
	return false
}

func toolIsPublish(tool string) bool {
	switch tool {
	case "publish_site", "begin_upload", "commit_upload", "rollback_site":
		return true
	}
	return false
}

// RecordAudit 实现 mcpserver.Backend：每个被门禁放行的工具调用落一行（成败都记）。
// agent 归因走连接池（Mcp-Session-Id → 会话登记的 clientInfo 名）；site_id 尽力从入参提取。
func (s *Server) RecordAudit(header http.Header, tool string, args json.RawMessage, outcome string) {
	e := auditEntry{
		Time:    time.Now().UTC().Format(time.RFC3339Nano),
		Tool:    tool,
		Outcome: outcome,
	}
	if header == nil {
		e.KeyName = "stdio"
	} else {
		token := strings.TrimSpace(strings.TrimPrefix(header.Get("Authorization"), "Bearer "))
		switch {
		case token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.AuthToken)) == 1:
			e.KeyName = "管理 token"
		case strings.HasPrefix(token, mcpKeyPrefix):
			if k, err := s.Store.FindMCPKey(hashMCPKey(token)); err == nil {
				e.KeyID, e.KeyName = k.ID, k.Name
			} else {
				e.KeyName = "未知密钥"
			}
		}
		if sid := header.Get(sessionHeader); sid != "" {
			for _, sess := range s.mcpPool.list() {
				if sess.ID == sid {
					e.Agent = sess.Agent
					break
				}
			}
		}
	}
	// 入参里的 site_id（若有）记进审计，便于按站点过滤"谁动了这个站"
	if len(args) > 0 {
		var a struct {
			SiteID string `json:"site_id"`
		}
		if json.Unmarshal(args, &a) == nil {
			e.SiteID = strings.ToLower(a.SiteID)
		}
	}
	s.audit.write(e)
}
