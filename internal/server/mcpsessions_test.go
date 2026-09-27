package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"pageshare/internal/config"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// mcpInitialize 对 /mcp 发起标准 initialize 握手，返回响应状态与新会话 ID。
func mcpInitialize(t *testing.T, base, token, agentName string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":"1.0"}}}`,
		agentName)
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

func mcpSessionsList(t *testing.T, base, token string) []struct {
	Agent   string `json:"agent"`
	KeyName string `json:"key_name"`
} {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/mcp/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Sessions []struct {
			Agent   string `json:"agent"`
			KeyName string `json:"key_name"`
		} `json:"sessions"`
		Max int `json:"max"`
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	return out.Sessions
}

// TestMCPSessionPool 连接池：多 agent 初始化 → 按密钥归因 → 容量满拒绝新会话 → DELETE 释放。
func TestMCPSessionPool(t *testing.T) {
	ts, adminToken := newTestServer(t)

	// 签一把密钥，用它连接的会话应归因到该密钥
	resp, raw := mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"agent-key"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("签发密钥失败: %d %s", resp.StatusCode, raw)
	}
	var created struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &created)

	// 1. 三个 agent（两种凭据）依次初始化
	agents := []struct{ token, agent string }{
		{adminToken, "claude-desktop"}, {created.Token, "cursor"}, {created.Token, "pi-agent"},
	}
	sids := map[string]string{}
	for _, c := range agents {
		status, sid := mcpInitialize(t, ts.URL, c.token, c.agent)
		if status != http.StatusOK || sid == "" {
			t.Fatalf("agent %s 初始化应 200 且带会话 ID: %d %q", c.agent, status, sid)
		}
		sids[c.agent] = sid
	}

	// 2. 会话清单：agent 名与密钥归因正确
	sessions := mcpSessionsList(t, ts.URL, adminToken)
	if len(sessions) != 3 {
		t.Fatalf("应 3 个会话: %+v", sessions)
	}
	byAgent := map[string]string{}
	for _, s := range sessions {
		name := strings.Fields(s.Agent)[0] // "cursor v1.0" → "cursor"
		byAgent[name] = s.KeyName
	}
	if byAgent["claude-desktop"] != "管理 token" || byAgent["cursor"] != "agent-key" || byAgent["pi-agent"] != "agent-key" {
		t.Fatalf("会话归因错误: %+v", byAgent)
	}

	// 3. 塞满剩余容量后，新 initialize 应 503
	for i := len(sessions); i < MaxMCPSessions; i++ {
		if status, _ := mcpInitialize(t, ts.URL, adminToken, fmt.Sprintf("filler-%d", i)); status != http.StatusOK {
			t.Fatalf("填充会话 %d 应 200: %d", i, status)
		}
	}
	if status, _ := mcpInitialize(t, ts.URL, adminToken, "overflow"); status != http.StatusServiceUnavailable {
		t.Fatalf("池满后 initialize 应 503，得到 %d", status)
	}

	// 4. 显式关闭一个会话（DELETE /mcp）→ 释放容量 → 新会话可入
	delReq, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	delReq.Header.Set(sessionHeader, sids["claude-desktop"])
	delReq.Header.Set("Authorization", "Bearer "+adminToken)
	if r, err := http.DefaultClient.Do(delReq); err != nil || r.StatusCode >= 500 {
		t.Fatalf("DELETE /mcp 失败: %v", err)
	} else {
		r.Body.Close()
	}
	if status, _ := mcpInitialize(t, ts.URL, adminToken, "after-release"); status != http.StatusOK {
		t.Fatalf("释放后 initialize 应 200，得到 %d", status)
	}
}

// TestMCPSessionIdleGC 空闲会话被回收后容量恢复。
func TestMCPSessionIdleGC(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/s.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "cfg.json") // 审计/数据落临时目录，不落仓库
	cfg.AuthToken = "tok"
	cfg.CookieSecret = "sec"
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	if status, _ := mcpInitialize(t, ts.URL, "tok", "idle-agent"); status != http.StatusOK {
		t.Fatalf("初始化应 200: %d", status)
	}
	if n := len(srv.mcpPool.list()); n != 1 {
		t.Fatalf("应登记 1 个会话，得到 %d", n)
	}
	// 把活跃时间拨回 TTL 之前 → sweep → 应被清出
	srv.mcpPool.mu.Lock()
	for _, s := range srv.mcpPool.sessions {
		s.LastSeen = s.LastSeen.Add(-2 * mcpSessionIdleTTL)
	}
	srv.mcpPool.mu.Unlock()
	srv.mcpPool.sweep()
	if n := len(srv.mcpPool.list()); n != 0 {
		t.Fatalf("空闲回收后应 0 会话，得到 %d", n)
	}
}

// TestConcurrentPublishSameSite 多 agent 并发覆盖同一站点：锁池串行化后版本号严格递增不丢。
func TestConcurrentPublishSameSite(t *testing.T) {
	ts, token := newTestServer(t)

	resp, body := postZip(ts, token, "POST", "/api/v1/sites",
		zipSite(map[string]string{"index.html": "<html>v0</html>"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建站失败: %d %s", resp.StatusCode, body)
	}
	var site struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal(body, &site)

	// 6 个 agent 并发覆盖同一站点
	const n = 6
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, b := postZip(ts, token, "PUT", "/api/v1/sites/"+site.ID,
				zipSite(map[string]string{"index.html": fmt.Sprintf("<html>v%d</html>", i+1)}), nil)
			if r.StatusCode != http.StatusOK {
				t.Errorf("并发发布 %d 应 200: %d %s", i, r.StatusCode, b)
			}
		}(i)
	}
	wg.Wait()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sites/"+site.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var final struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(raw, &final)
	if final.Version != site.Version+int64(n) {
		t.Fatalf("并发 %d 次发布后版本应 %d，得到 %d（版本被踩踏）", n, site.Version+int64(n), final.Version)
	}
}
