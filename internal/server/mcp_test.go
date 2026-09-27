package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"pageshare/internal/config"
	"pageshare/internal/mcpserver"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

func errorsNew(msg string) error { return errors.New(msg) }

// TestMCPTools 走一遍 MCP 客户端 → 工具 → 发布/列表/覆盖/删除 的完整链路。
func TestMCPTools(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "cfg.json") // 审计/数据落临时目录，不落仓库
	cfg.AuthToken = "tok"
	cfg.CookieSecret = "sec"
	cfg.SiteWildcardHost = "s.local"
	cfg.VersionsKept = 3 // 保留最近 3 版，覆盖发布后可回滚
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// 准备一个"Vite 产物"目录（含应被跳过的杂物）
	siteDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(filepath.Join(siteDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "index.html"),
		[]byte(`<html><body><h1>mcp-site</h1><script src="/assets/app-Bx91.js"></script></body></html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "assets", "app-Bx91.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	// MCP 客户端经 InMemory 传输直连服务
	ms := mcpserver.NewServer(srv, mcpserver.Config{}, "test")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := ms.Connect(context.Background(), t1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx := context.Background()

	call := func(name string, args map[string]any) (map[string]any, error) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err
		}
		if res.IsError {
			return nil, errToolResult(res)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}

	// 1. publish_site 新建（spa + ttl）
	pub, err := call("publish_site", map[string]any{
		"path": siteDir, "name": "MCP 站", "spa": true, "ttl": "48h",
	})
	if err != nil {
		t.Fatalf("publish_site: %v", err)
	}
	if pub["url"] == "" || pub["id"] == "" {
		t.Fatalf("publish 返回缺 id/url: %v", pub)
	}
	lid := strings.ToLower(pub["id"].(string))

	// 内容真的可访问（泛域名根路径语义，presigned 302 跟随）
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = lid + ".s.local"
	r, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("MCP 发布的站点应可访问，得到 %d", r.StatusCode)
	}

	// 2. list_sites → 1 项
	list, err := call("list_sites", nil)
	if err != nil {
		t.Fatalf("list_sites: %v", err)
	}
	sites, _ := list["sites"].([]any)
	if len(sites) != 1 {
		t.Fatalf("list 应 1 项: %v", list)
	}

	// 3. publish_site + site_id → 覆盖，版本 2
	pub2, err := call("publish_site", map[string]any{
		"path": siteDir, "site_id": pub["id"].(string), "spa": true,
	})
	if err != nil {
		t.Fatalf("覆盖发布: %v", err)
	}
	if v, _ := pub2["version"].(float64); v != 2 {
		t.Fatalf("覆盖后版本应 2: %v", pub2)
	}

	// 4. get_site
	got, err := call("get_site", map[string]any{"site_id": pub["id"].(string)})
	if err != nil {
		t.Fatalf("get_site: %v", err)
	}
	if got["id"] == "" {
		t.Fatalf("get_site 返回异常: %v", got)
	}

	// 5. update_site：改名 + 设密码 + 清 TTL + 关 SPA，版本不变
	up, err := call("update_site", map[string]any{
		"site_id": pub["id"].(string), "name": "改名", "password": "pw", "ttl": "never", "spa": false,
	})
	if err != nil {
		t.Fatalf("update_site: %v", err)
	}
	if up["name"] != "改名" || up["has_password"] != true || up["spa"] != false {
		t.Fatalf("update_site 未生效: %v", up)
	}
	if v, _ := up["version"].(float64); v != 2 {
		t.Fatalf("update_site 不应涨版本: %v", up)
	}
	if up["expires_at"] != nil {
		t.Fatalf("ttl=never 应清掉过期时间: %v", up)
	}
	// 公开面被 gate 拦截
	reqG, _ := http.NewRequest("GET", ts.URL+"/", nil)
	reqG.Host = lid + ".s.local"
	rG, err := ts.Client().Do(reqG)
	if err != nil {
		t.Fatal(err)
	}
	rG.Body.Close()
	if rG.StatusCode != 401 {
		t.Fatalf("设密码后应 401，得到 %d", rG.StatusCode)
	}
	// 清密码后恢复可访问，并产生页览
	if _, err := call("update_site", map[string]any{
		"site_id": pub["id"].(string), "password": "",
	}); err != nil {
		t.Fatalf("清密码: %v", err)
	}
	reqV, _ := http.NewRequest("GET", ts.URL+"/", nil)
	reqV.Host = lid + ".s.local"
	rV, err := ts.Client().Do(reqV)
	if err != nil {
		t.Fatal(err)
	}
	rV.Body.Close()
	if rV.StatusCode != 200 {
		t.Fatalf("清密码后应 200，得到 %d", rV.StatusCode)
	}

	// 6. get_site_stats：单站 + 全量汇总
	stats, err := call("get_site_stats", map[string]any{"site_id": pub["id"].(string)})
	if err != nil {
		t.Fatalf("get_site_stats: %v", err)
	}
	if ts2, _ := stats["total_sites"].(float64); ts2 != 1 {
		t.Fatalf("stats 应 1 站: %v", stats)
	}
	srow, _ := stats["sites"].([]any)
	if len(srow) != 1 {
		t.Fatalf("stats.sites 应 1 项: %v", stats)
	}
	e0, _ := srow[0].(map[string]any)
	if v, _ := e0["visits"].(float64); v < 1 {
		t.Fatalf("页览应 >=1（含未刷盘增量）: %v", e0)
	}
	if b, _ := stats["busiest"].(map[string]any); b == nil || b["id"] != e0["id"] {
		t.Fatalf("busiest 应指向该站: %v", stats["busiest"])
	}
	all, err := call("get_site_stats", nil)
	if err != nil {
		t.Fatalf("get_site_stats 全量: %v", err)
	}
	if tv, _ := all["total_visits"].(float64); tv < 1 {
		t.Fatalf("total_visits 应 >=1: %v", all)
	}
	// 错误路径：无字段 / 未知站点
	if _, err := call("update_site", map[string]any{"site_id": pub["id"].(string)}); err == nil {
		t.Fatal("update_site 无任何字段应报错")
	}
	if _, err := call("update_site", map[string]any{"site_id": "AAAAAAAA", "name": "x"}); err == nil {
		t.Fatal("update_site 未知站点应报错")
	}
	if _, err := call("get_site_stats", map[string]any{"site_id": "AAAAAAAA"}); err == nil {
		t.Fatal("get_site_stats 未知站点应报错")
	}

	// 6.5 list_site_versions / rollback_site：发布 v3 后回滚到 v2，再发布应跳 v4
	if _, err := call("publish_site", map[string]any{"path": siteDir, "site_id": pub["id"].(string)}); err != nil {
		t.Fatalf("publish v3: %v", err)
	}
	vl, err := call("list_site_versions", map[string]any{"site_id": pub["id"].(string)})
	if err != nil {
		t.Fatalf("list_site_versions: %v", err)
	}
	vs, _ := vl["versions"].([]any)
	if len(vs) != 3 {
		t.Fatalf("版本历史应 3 条: %v", vl)
	}
	v3, _ := vs[2].(map[string]any)
	if v3["current"] != true || v3["restorable"] != false {
		t.Fatalf("v3 应 current 且不可回滚: %v", v3)
	}
	v1, _ := vs[0].(map[string]any)
	if v1["restorable"] != true {
		t.Fatalf("kept=3 下 v1 应可回滚: %v", v1)
	}
	rb, err := call("rollback_site", map[string]any{"site_id": pub["id"].(string), "version": float64(2)})
	if err != nil {
		t.Fatalf("rollback_site: %v", err)
	}
	if rv, _ := rb["version"].(float64); rv != 2 {
		t.Fatalf("回滚结果版本应为 2: %v", rb)
	}
	if _, err := call("rollback_site", map[string]any{"site_id": pub["id"].(string), "version": float64(2)}); err == nil {
		t.Fatal("回滚到当前版本应报错")
	}
	if _, err := call("rollback_site", map[string]any{"site_id": pub["id"].(string), "version": float64(99)}); err == nil {
		t.Fatal("回滚到无记录版本应报错")
	}
	if _, err := call("rollback_site", map[string]any{"site_id": pub["id"].(string), "version": float64(0)}); err == nil {
		t.Fatal("version<=0 应报错")
	}
	if _, err := call("list_site_versions", map[string]any{"site_id": "AAAAAAAA"}); err == nil {
		t.Fatal("list_site_versions 未知站点应报错")
	}
	pub4, err := call("publish_site", map[string]any{"path": siteDir, "site_id": pub["id"].(string)})
	if err != nil {
		t.Fatalf("回滚后再发布: %v", err)
	}
	if v, _ := pub4["version"].(float64); v != 4 {
		t.Fatalf("回滚后新版本应为 4（不与保留版本撞号）: %v", pub4)
	}

	// 7. delete_site → 分享面 404
	del, err := call("delete_site", map[string]any{"site_id": lid})
	if err != nil {
		t.Fatalf("delete_site: %v", err)
	}
	if del["deleted"] != true {
		t.Fatalf("delete 返回异常: %v", del)
	}
	req2, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req2.Host = lid + ".s.local"
	r2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 404 {
		t.Fatalf("删除后应 404，得到 %d", r2.StatusCode)
	}

	// 8. 错误路径：目录不存在应报错
	if _, err := call("publish_site", map[string]any{"path": filepath.Join(dir, "nope")}); err == nil {
		t.Fatal("不存在的目录应报错")
	}
}

// TestMCPTwoStageUpload 走 MCP 客户端 begin_upload → HTTP PUT → commit_upload 全链路。
func TestMCPTwoStageUpload(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mcp2.json"))
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

	ms := mcpserver.NewServer(srv, mcpserver.Config{}, "test")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := ms.Connect(context.Background(), t1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx := context.Background()

	call := func(name string, args map[string]any) (map[string]any, error) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err
		}
		if res.IsError {
			return nil, errToolResult(res)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}

	// begin_upload（新建，带 ttl/spa）
	begin, err := call("begin_upload", map[string]any{"name": "两阶段", "ttl": "24h", "spa": true})
	if err != nil {
		t.Fatalf("begin_upload: %v", err)
	}
	uid, _ := begin["upload_id"].(string)
	putURL, _ := begin["put_url"].(string)
	if uid == "" || !strings.Contains(putURL, "/api/v1/uploads/") {
		t.Fatalf("begin_upload 返回异常: %v", begin)
	}
	if ei, _ := begin["expires_in"].(float64); ei != 900 {
		t.Fatalf("expires_in 应 900: %v", begin)
	}

	// 数据面 PUT（管理 token 或 psm_ 密钥均可，这里用 token）
	zipb := zipSite(map[string]string{"index.html": `<h1>two-stage</h1>`})
	req, _ := http.NewRequest("PUT", ts.URL+strings.TrimPrefix(putURL, cfg.PublicBaseURL), bytes.NewReader(zipb))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT 应 200: %d", resp.StatusCode)
	}

	// commit_upload → 与 publish_site 同形状的结果
	pub, err := call("commit_upload", map[string]any{"upload_id": uid})
	if err != nil {
		t.Fatalf("commit_upload: %v", err)
	}
	if pub["id"] == "" || pub["url"] == "" || pub["name"] != "两阶段" {
		t.Fatalf("commit 结果异常: %v", pub)
	}
	if v, _ := pub["version"].(float64); v != 1 {
		t.Fatalf("版本应 1: %v", pub)
	}
	// spa/ttl 随参数生效（get_site 复核）
	got, err := call("get_site", map[string]any{"site_id": pub["id"].(string)})
	if err != nil {
		t.Fatalf("get_site: %v", err)
	}
	if got["spa"] != true || got["expires_at"] == nil {
		t.Fatalf("begin 参数应随发布生效: %v", got)
	}
	// 错误路径：未知 upload_id / 空参
	if _, err := call("commit_upload", map[string]any{"upload_id": "nope"}); err == nil {
		t.Fatal("未知 upload_id 应报错")
	}
	if _, err := call("begin_upload", map[string]any{"site_id": "AAAAAAAA"}); err == nil {
		t.Fatal("begin 未知站点应报错")
	}
}

// TestMCPStdioSmoke 编译自身并起 `pageshare mcp -dev` 子进程，验证 stdio 握手 + 工具清单。
func TestMCPStdioSmoke(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pageshare-test")
	out, err := exec.Command("go", "build", "-o", bin, "pageshare/cmd/pageshare").CombinedOutput()
	if err != nil {
		t.Fatalf("编译失败: %v\n%s", err, out)
	}
	// 子进程工作目录指到临时目录：-dev 的默认相对 db_path/审计文件不落源码树
	smoke := exec.Command(bin, "mcp", "-dev")
	smoke.Dir = t.TempDir()
	tr := &mcp.CommandTransport{Command: smoke}
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("stdio 连接失败: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"publish_site", "begin_upload", "commit_upload", "list_sites", "get_site", "update_site", "get_site_stats", "list_site_versions", "rollback_site", "delete_site"} {
		if !names[want] {
			t.Fatalf("缺工具 %s: %v", want, names)
		}
	}
}

// errToolResult 把 isError 的结果转成 error（正文在 text content 里）。
func errToolResult(res *mcp.CallToolResult) error {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if sb.Len() == 0 {
		return errorsNew("tool call failed")
	}
	return errorsNew(sb.String())
}
