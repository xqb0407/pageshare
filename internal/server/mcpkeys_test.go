package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mcpKeyAPI(base, token, method, path, body string) (*http.Response, []byte) {
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

// TestMCPKeyPool 密钥池 API 全链路：签发（明文仅一次）→ 列表（只有前缀）→ 密钥过 /mcp 鉴权
// → 回写最近使用 → 删除后立即失效。管理 token 兼容路径与未知密钥拒绝也一并覆盖。
func TestMCPKeyPool(t *testing.T) {
	ts, adminToken := newTestServer(t)

	// 1. 签发
	resp, raw := mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"claude desktop"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应 201，得到 %d: %s", resp.StatusCode, raw)
	}
	var created struct {
		ID, Name, Prefix, Token string
	}
	_ = json.Unmarshal(raw, &created)
	if !strings.HasPrefix(created.Token, "psm_") || len(created.Token) < 20 {
		t.Fatalf("签发的明文格式异常: %q", created.Token)
	}
	if created.Prefix != created.Token[:12] || created.Name != "claude desktop" {
		t.Fatalf("签发返回异常: %s", raw)
	}

	// 2. 列表：只有前缀与元数据，绝无明文/哈希
	resp, raw = mcpKeyAPI(ts.URL, adminToken, "GET", "/api/v1/mcp/keys", "")
	if resp.StatusCode != 200 {
		t.Fatalf("列表应 200: %d", resp.StatusCode)
	}
	if bytes.Contains(raw, []byte(created.Token)) || bytes.Contains(raw, []byte("token")) {
		t.Fatalf("列表泄漏明文: %s", raw)
	}
	var list struct {
		Keys []struct{ ID, Prefix, Name string }
	}
	_ = json.Unmarshal(raw, &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != created.ID {
		t.Fatalf("列表应 1 项: %s", raw)
	}

	// 3. 未带密钥 / 未知密钥 → /mcp 401；池内密钥与管理 token → 非 401（进入 MCP 协议层）
	for _, c := range []struct {
		name, token string
		want401     bool
	}{
		{"无 token", "", true},
		{"未知密钥", "psm_" + strings.Repeat("0", 40), true},
		{"被删密钥占位", created.Token, false}, // 此刻还在池内
		{"管理 token", adminToken, false},
	} {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if got401 := r.StatusCode == http.StatusUnauthorized; got401 != c.want401 {
			t.Fatalf("%s: 401=%d want401=%v", c.name, r.StatusCode, c.want401)
		}
	}

	// 4. 回写最近使用：走一次 /mcp 后列表应带 last_used_at
	_, _ = mcpKeyAPI(ts.URL, adminToken, "GET", "/api/v1/mcp/keys", "")
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+created.Token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	_, raw = mcpKeyAPI(ts.URL, adminToken, "GET", "/api/v1/mcp/keys", "")
	if !bytes.Contains(raw, []byte("last_used_at")) {
		t.Fatalf("调用后应回写 last_used_at: %s", raw)
	}

	// 5. 删除 → 持有方立即失效
	resp, raw = mcpKeyAPI(ts.URL, adminToken, "DELETE", "/api/v1/mcp/keys/"+created.ID, "")
	if resp.StatusCode != 200 {
		t.Fatalf("删除应 200: %d %s", resp.StatusCode, raw)
	}
	req2, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	req2.Header.Set("Authorization", "Bearer "+created.Token)
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("删除后应 401，得到 %d", r2.StatusCode)
	}

	// 6. 重复删除 → 404；无鉴权管理 API → 401
	if resp, _ = mcpKeyAPI(ts.URL, adminToken, "DELETE", "/api/v1/mcp/keys/"+created.ID, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除应 404")
	}
	if resp, _ = mcpKeyAPI(ts.URL, "", "GET", "/api/v1/mcp/keys", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("密钥管理 API 必须用管理 token")
	}
}

// TestMCPKeyPoolCap 密钥池容量：签满 20 把后拒绝再签。
func TestMCPKeyPoolCap(t *testing.T) {
	ts, adminToken := newTestServer(t)
	for i := 0; i < MaxMCPKeys; i++ {
		resp, raw := mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"k"}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("第 %d 把应签发成功: %d %s", i+1, resp.StatusCode, raw)
		}
	}
	resp, raw := mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"overflow"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超出容量应 400: %d %s", resp.StatusCode, raw)
	}
}
