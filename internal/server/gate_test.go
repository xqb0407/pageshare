// P1-5 治理测试：scope 门禁、每密钥滑动窗口限流、审计落盘/滚动/查询，
// 以及经真实 HTTP streamable 传输的端到端拦截（InMemory 传 nil 头，测不到门禁）。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"pageshare/internal/store"
)

// issueKey 直接向密钥池塞一把指定 scopes 的密钥，返回明文（nil scopes = 存量全权语义）。
func issueKey(t *testing.T, srv *Server, name string, scopes []string) (string, *store.MCPKey) {
	t.Helper()
	plain, prefix, hash, err := newMCPKey()
	if err != nil {
		t.Fatal(err)
	}
	k := &store.MCPKey{ID: newShortID(), Name: name, Prefix: prefix, KeyHash: hash, Scopes: scopes}
	if err := srv.Store.CreateMCPKey(k); err != nil {
		t.Fatal(err)
	}
	return plain, k
}

func bearerHeader(tok string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + tok}}
}

func TestScopeGate(t *testing.T) {
	_, srv, adminToken := newTestServerFull(t)

	// stdio（nil 头）与管理 token：全权放行
	if err := srv.Authorize(nil, "delete_site"); err != nil {
		t.Fatalf("nil 头应全权: %v", err)
	}
	if err := srv.Authorize(bearerHeader(adminToken), "delete_site"); err != nil {
		t.Fatalf("管理 token 应全权: %v", err)
	}
	// 无凭据：交给外层 mcpAuth 拒绝，门禁不重复报错
	if err := srv.Authorize(http.Header{}, "publish_site"); err != nil {
		t.Fatalf("空凭据应交由外层处理: %v", err)
	}
	// 非 psm_ 前缀的陌生凭据：拒
	if err := srv.Authorize(bearerHeader("sk-whatever"), "list_sites"); err == nil ||
		!strings.Contains(err.Error(), "无法识别") {
		t.Fatalf("陌生凭据应报无法识别: %v", err)
	}
	// 无效密钥
	if err := srv.Authorize(bearerHeader("psm_"+strings.Repeat("f", 40)), "list_sites"); err == nil ||
		!strings.Contains(err.Error(), "已吊销") {
		t.Fatalf("无效密钥应报已吊销: %v", err)
	}

	// 只读密钥：读放行，写/删拒绝且点名缺的 scope
	ro, _ := issueKey(t, srv, "只读", []string{ScopeRead})
	if err := srv.Authorize(bearerHeader(ro), "get_site"); err != nil {
		t.Fatalf("只读密钥应可查: %v", err)
	}
	if err := srv.Authorize(bearerHeader(ro), "publish_site"); err == nil ||
		!strings.Contains(err.Error(), "缺少 publish") {
		t.Fatalf("只读密钥发布应被拒: %v", err)
	}
	if err := srv.Authorize(bearerHeader(ro), "delete_site"); err == nil ||
		!strings.Contains(err.Error(), "缺少 delete") {
		t.Fatalf("只读密钥删站应被拒: %v", err)
	}

	// 升级前的存量密钥（scopes 为空）：保持全权，不被静默断权
	legacy, _ := issueKey(t, srv, "存量", nil)
	for _, tool := range []string{"publish_site", "get_site", "delete_site", "rollback_site"} {
		if err := srv.Authorize(bearerHeader(legacy), tool); err != nil {
			t.Fatalf("空 scopes 存量密钥应全权（%s）: %v", tool, err)
		}
	}

	// publish+delete 组合密钥
	pw, _ := issueKey(t, srv, "运维", []string{ScopePublish, ScopeDelete})
	if err := srv.Authorize(bearerHeader(pw), "rollback_site"); err != nil {
		t.Fatalf("publish 密钥应可回滚: %v", err)
	}
	if err := srv.Authorize(bearerHeader(pw), "delete_site"); err != nil {
		t.Fatalf("delete 密钥应可删站: %v", err)
	}
	if err := srv.Authorize(bearerHeader(pw), "list_sites"); err == nil ||
		!strings.Contains(err.Error(), "缺少 read") {
		t.Fatalf("未授予 read 的密钥查询应被拒: %v", err)
	}

	// 限流经 Authorize 生效：发布类第 11 次拒，读类不受牵连
	pubKey, _ := issueKey(t, srv, "刷量", []string{ScopePublish, ScopeRead})
	h := bearerHeader(pubKey)
	for i := 0; i < ratePub; i++ {
		if err := srv.Authorize(h, "publish_site"); err != nil {
			t.Fatalf("发布第 %d 次不该被限流: %v", i+1, err)
		}
	}
	if err := srv.Authorize(h, "publish_site"); err == nil ||
		!strings.Contains(err.Error(), "请求过快") {
		t.Fatalf("发布第 %d 次应触发限流: %v", ratePub+1, err)
	}
	if err := srv.Authorize(h, "list_sites"); err != nil {
		t.Fatalf("发布窗满不应饿死读类: %v", err)
	}
}

// TestKeyLimiter 滑动窗口：发布类第 11 次拒、总量第 61 次拒、密钥间互不影响。
func TestKeyLimiter(t *testing.T) {
	l := newKeyLimiter()
	for i := 0; i < ratePub; i++ {
		if _, ok := l.hit("ka", true); !ok {
			t.Fatalf("发布类第 %d 次不应被拒", i+1)
		}
	}
	d, ok := l.hit("ka", true)
	if ok || d <= 0 || d > rateWindow {
		t.Fatalf("发布类第 %d 次应被拒且给出 retry-after，得到 ok=%v d=%v", ratePub+1, ok, d)
	}
	// 发布窗满不拖累读类：总量还宽裕
	if _, ok := l.hit("ka", false); !ok {
		t.Fatal("发布窗满时读类应仍可放行")
	}
	// 打满总量（发布 ratePub + 上一步探测读 1 + 补齐）后再一发拒
	for i := 0; i < rateTotal-ratePub-1; i++ {
		if _, ok := l.hit("ka", false); !ok {
			t.Fatalf("总量第 %d 次不应被拒", ratePub+i+1)
		}
	}
	if _, ok := l.hit("ka", false); ok {
		t.Fatal("总量超 60/min 应被拒")
	}
	// 别的密钥不受影响
	if _, ok := l.hit("kb", true); !ok {
		t.Fatal("限流必须按密钥隔离")
	}
}

func TestAuditWriter(t *testing.T) {
	dir := t.TempDir()
	w := newAuditWriter(dir)
	mk := func(tool, site, keyID, key string) auditEntry {
		return auditEntry{
			Time:   time.Now().UTC().Format(time.RFC3339Nano),
			SiteID: site, KeyID: keyID, KeyName: key, Tool: tool, Outcome: "ok",
		}
	}
	if err := w.write(mk("get_site", "abc123", "k1", "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := w.write(mk("list_sites", "", "k2", "beta")); err != nil {
		t.Fatal(err)
	}
	if err := w.write(mk("publish_site", "def456", "k1", "alpha")); err != nil {
		t.Fatal(err)
	}

	// 文件权限 0600
	if fi, err := os.Stat(w.path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("审计文件应为 0600: %v %v", fi, err)
	}

	all, err := w.query("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Tool != "publish_site" || all[2].Tool != "get_site" {
		t.Fatalf("应新→旧 3 条: %+v", all)
	}
	// site 过滤大小写不敏感（条目存小写）
	bySite, _ := w.query("ABC123", "", "", 0)
	if len(bySite) != 1 || bySite[0].Tool != "get_site" {
		t.Fatalf("site 过滤异常: %+v", bySite)
	}
	// key 过滤：按名称或 id 都行
	byName, _ := w.query("", "alpha", "", 0)
	byID, _ := w.query("", "k1", "", 0)
	if len(byName) != 2 || len(byID) != 2 {
		t.Fatalf("key 过滤异常: name=%d id=%d", len(byName), len(byID))
	}
	// limit 截最新
	lim, _ := w.query("", "", "", 1)
	if len(lim) != 1 || lim[0].Tool != "publish_site" {
		t.Fatalf("limit 应截最新: %+v", lim)
	}
	// since 下界
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if got, err := w.query("", "", future, 0); err != nil || len(got) != 0 {
		t.Fatalf("未来 since 应空: %d %v", len(got), err)
	}
	if _, err := w.query("", "", "not-a-time", 0); err == nil {
		t.Fatal("非法 since 应报错")
	}

	// 滚动：写一条轮一次，超上限后最老一份丢弃、其余跨文件仍可查
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for i := 0; i < auditMaxRolling+2; i++ {
		if err := w.write(auditEntry{
			Time: time.Now().UTC().Format(time.RFC3339Nano),
			Tool: fmt.Sprintf("r%02d", i), Outcome: "ok",
		}); err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		err := w.rotateLocked()
		w.mu.Unlock()
		if err != nil {
			t.Fatalf("rotate %d: %v", i, err)
		}
	}
	if _, err := os.Stat(w.path() + fmt.Sprintf(".%d", auditMaxRolling+1)); err == nil {
		t.Fatal("滚动副本不得超过上限份数")
	}
	rolled, _ := w.query("", "", past, 100)
	// active 轮转后是空/不存在，可查到的最多是 auditMaxRolling 份；最早两条（r00、r01）已被挤掉
	if len(rolled) != auditMaxRolling {
		t.Fatalf("滚动后应剩 %d 条，实得 %d: %+v", auditMaxRolling, len(rolled), rolled)
	}
	if rolled[0].Tool != fmt.Sprintf("r%02d", auditMaxRolling+1) {
		t.Fatalf("跨滚动文件应按新→旧: %+v", rolled[0])
	}
}

// TestGovernanceE2E 走真实 HTTP streamable 传输：/mcp 上的 tools/call 被接收中间件
// 拦截——只读密钥读放行、发布被拒且给出缺的 scope；被放行的调用落审计并可在
// GET /api/v1/mcp/audit 查到（含 agent 归因）。签发 API 的 scopes 回显也在此覆盖。
func TestGovernanceE2E(t *testing.T) {
	ts, _, adminToken := newTestServerFull(t)

	// 1. 真实站点供 get_site 查询与审计 site 过滤
	resp, raw := postZip(ts, adminToken, "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "g"}), map[string]string{"name": "gate"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建站失败: %d %s", resp.StatusCode, raw)
	}
	siteID := mustJSON(t, raw)["id"].(string)

	// 2. 签发只读密钥（默认不含 publish/delete）；非法 scope 名 400
	resp, raw = mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"只读e2e","scopes":["read"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("签发应 201: %d %s", resp.StatusCode, raw)
	}
	var created struct {
		Token  string   `json:"token"`
		Scopes []string `json:"scopes"`
	}
	_ = json.Unmarshal(raw, &created)
	if len(created.Scopes) != 1 || created.Scopes[0] != "read" {
		t.Fatalf("scopes 应回显 [read]: %s", raw)
	}
	if resp, raw = mcpKeyAPI(ts.URL, adminToken, "POST", "/api/v1/mcp/keys", `{"name":"x","scopes":["teleport"]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 scope 应 400: %d %s", resp.StatusCode, raw)
	}

	// 3. 握手并调用：get_site 放行、publish_site 被门禁拒
	status, sid := mcpInitialize(t, ts.URL, created.Token, "gate-e2e")
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize 失败: %d %q", status, sid)
	}
	mcpNotify(t, ts.URL, created.Token, sid, "notifications/initialized")

	res := mcpCall(t, ts.URL, created.Token, sid, 2, "tools/call", `{"name":"get_site","arguments":{"site_id":"`+siteID+`"}}`)
	if res["isError"] == true {
		t.Fatalf("只读密钥 get_site 应放行: %v", res)
	}
	res = mcpCall(t, ts.URL, created.Token, sid, 3, "tools/call", `{"name":"publish_site","arguments":{"path":"/tmp"}}`)
	txt := toolResultText(res)
	if res["isError"] != true || !strings.Contains(txt, "缺少 publish") {
		t.Fatalf("只读密钥发布应被拒且点名 publish: %v %q", res, txt)
	}

	// 4. 审计：只有放行的 get_site 落盘（门禁拒绝不记账）；含键名/agent 归因/小写 site_id
	got := adminGet(t, ts, adminToken, "/api/v1/mcp/audit?site="+siteID)
	entries := got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("site 过滤应恰 1 条（被拒调用不落审计）: %v", got)
	}
	e := entries[0].(map[string]any)
	if e["tool"] != "get_site" || e["key"] != "只读e2e" || e["outcome"] != "ok" {
		t.Fatalf("审计条目异常: %v", e)
	}
	if !strings.HasPrefix(e["agent"].(string), "gate-e2e") {
		t.Fatalf("agent 归因失败: %v", e)
	}
	got = adminGet(t, ts, adminToken, "/api/v1/mcp/audit")
	all := got["entries"].([]any)
	if len(all) != 1 || all[0].(map[string]any)["tool"] != "get_site" {
		t.Fatalf("全量审计此刻应只有放行的 get_site: %v", got)
	}
	if got["count"].(float64) != 1 {
		t.Fatalf("count 字段异常: %v", got)
	}

	// 5. 未带管理 token 查审计 → 401
	if resp, _ = mcpKeyAPI(ts.URL, "", "GET", "/api/v1/mcp/audit", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("审计查询必须管理 token: %d", resp.StatusCode)
	}
}

// ---- streamable JSON-RPC 小工具（与 mcpkeys_test 的 ping 不同，这里要解析 SSE 帧）----

func mcpNotify(t *testing.T, base, token, sid, method string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":`+fmt.Sprintf("%q", method)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%s 通知应 202，得到 %d", method, resp.StatusCode)
	}
}

// mcpCall 发一次请求并等待 SSE 里 id 匹配的响应帧。
func mcpCall(t *testing.T, base, token, sid string, id int, method, params string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &msg) != nil {
			continue
		}
		if v, ok := msg["id"].(float64); !ok || int(v) != id {
			continue
		}
		if e, has := msg["error"]; has {
			t.Fatalf("JSON-RPC 层报错: %v", e)
		}
		res, _ := msg["result"].(map[string]any)
		return res
	}
	t.Fatalf("未读到 id=%d 的响应（HTTP %d）", id, resp.StatusCode)
	return nil
}

// toolResultText 拼接 CallToolResult 里全部 TextContent。
func toolResultText(res map[string]any) string {
	out := ""
	if arr, ok := res["content"].([]any); ok {
		for _, c := range arr {
			if m, ok := c.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					out += s
				}
			}
		}
	}
	return out
}
