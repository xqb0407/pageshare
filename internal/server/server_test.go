package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pageshare/internal/config"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// newTestServer 起一套 mem 存储 + 临时 sqlite 的完整服务，返回 httptest server 与 token。
func newTestServer(t *testing.T) (*httptest.Server, string) {
	ts, _, token := newTestServerFull(t)
	return ts, token
}

// newTestServerFull 额外返回 *Server，供需要触达内部状态（如 mcpPool）的测试使用。
func newTestServerFull(t *testing.T) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sites.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.AuthToken = "test-token"
	cfg.CookieSecret = "test-cookie-secret"
	cfg.Storage = config.Storage{Backend: "mem"}
	// 审计文件按 DBPath 所在目录落盘（P1-5）——指到临时目录，测试不污染仓库
	cfg.DBPath = filepath.Join(dir, "sites.json")
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, srv, cfg.AuthToken
}

func zipSite(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

// postZip 用 multipart 上传（与管理 UI 同契约）。
func postZip(ts *httptest.Server, token string, method, path string, zip []byte, fields map[string]string) (*http.Response, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if len(zip) > 0 {
		fw, _ := mw.CreateFormFile("file", "site.zip")
		fw.Write(zip)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	mw.Close()
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func mustJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("响应不是 JSON: %s", body)
	}
	return m
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestE2E(t *testing.T) {
	ts, token := newTestServer(t)

	// 1. 未带 token → 401
	resp, _ := postZip(ts, "", "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "x"}), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，得到 %d", resp.StatusCode)
	}

	// 2. 上传 zip（带密码 + TTL）→ 201
	zip1 := zipSite(map[string]string{"index.html": "<h1>v1</h1>", "app.js": "console.log(1)"})
	resp, body := postZip(ts, token, "POST", "/api/v1/sites", zip1,
		map[string]string{"name": "测试站", "ttl": "72h", "password": "secret"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应 201，得到 %d: %s", resp.StatusCode, body)
	}
	site := mustJSON(t, body)
	id := site["id"].(string)
	url := site["url"].(string)
	if site["version"].(float64) != 1 || site["has_password"] != true {
		t.Fatalf("站点数据不对: %v", site)
	}
	if !strings.HasSuffix(url, "/s/"+strings.ToLower(id)+"/") {
		t.Fatalf("url 形状不对: %q", url)
	}
	if site["expires_at"] == nil {
		t.Fatal("expires_at 应存在")
	}

	// 3. 无密码访问 → 401 口令页
	resp, _ = http.Get(ts.URL + "/s/" + strings.ToLower(id) + "/")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(page), "password") {
		t.Fatalf("无 cookie 应得口令页，得到 %d", resp.StatusCode)
	}

	// 4. 提交正确密码 → 303 → 跟随拿到内容
	form := strings.NewReader("password=secret&next=")
	resp, err := http.PostForm(ts.URL+"/s/"+strings.ToLower(id)+"/gate", nil)
	_ = form
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// PostForm 不带表单会 401；改用手动构造
	req, _ := http.NewRequest("POST", ts.URL+"/s/"+strings.ToLower(id)+"/gate",
		strings.NewReader("password=secret&next="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("密码正确应 303，得到 %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "psg_") {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("应下发会话 cookie")
	}

	// 5. 带 cookie 代理访问 → index.html 内容（密码站点必须代理而非 302）
	req, _ = http.NewRequest("GET", ts.URL+"/s/"+strings.ToLower(id)+"/", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(page) != "<h1>v1</h1>" {
		t.Fatalf("带 cookie 应得 v1 内容，得到 %d: %q", resp.StatusCode, page)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// 6. 错误密码 → 401
	req, _ = http.NewRequest("POST", ts.URL+"/s/"+strings.ToLower(id)+"/gate",
		strings.NewReader("password=wrong&next="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，得到 %d", resp.StatusCode)
	}

	// 7. 覆盖发布 → 同一 id，版本 2，内容更新
	zip2 := zipSite(map[string]string{"index.html": "<h1>v2</h1>"})
	resp, body = postZip(ts, token, "PUT", "/api/v1/sites/"+strings.ToLower(id), zip2, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("覆盖应 200，得到 %d: %s", resp.StatusCode, body)
	}
	site = mustJSON(t, body)
	if site["version"].(float64) != 2 {
		t.Fatalf("覆盖后版本应为 2: %v", site)
	}
	req, _ = http.NewRequest("GET", ts.URL+"/s/"+strings.ToLower(id)+"/", nil)
	req.AddCookie(cookie)
	resp, _ = client.Do(req)
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(page) != "<h1>v2</h1>" {
		t.Fatalf("覆盖后内容应为 v2: %q", page)
	}

	// 8. 列表 → 1 个站点
	req, _ = http.NewRequest("GET", ts.URL+"/api/v1/sites", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = client.Do(req)
	listBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var sites []map[string]any
	if err := json.Unmarshal(listBody, &sites); err != nil || len(sites) != 1 {
		t.Fatalf("列表应为 1 项: %s", listBody)
	}

	// 9. 删除 → 分享面 404
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/sites/"+strings.ToLower(id), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("删除应 200，得到 %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/s/" + strings.ToLower(id) + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后应 404，得到 %d", resp.StatusCode)
	}
}

func TestE2ENoPasswordPresignAndTTL(t *testing.T) {
	ts, token := newTestServer(t)

	// 无密码 + 短 TTL
	zip1 := zipSite(map[string]string{"index.html": "<h1>ephemeral</h1>"})
	resp, body := postZip(ts, token, "POST", "/api/v1/sites", zip1, map[string]string{"ttl": "1s"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应 201: %s", body)
	}
	site := mustJSON(t, body)
	lid := strings.ToLower(site["id"].(string))

	// 无密码 → 302 到 presigned（mem 后端 = 同源 /__memstore/ 路径），跟随得内容
	client := ts.Client()
	resp, err := client.Get(ts.URL + "/s/" + lid + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(page) != "<h1>ephemeral</h1>" {
		t.Fatalf("无密码站点应可访问: %d %q", resp.StatusCode, page)
	}

	// 等过期 → 410
	time.Sleep(1100 * time.Millisecond)
	resp, _ = http.Get(ts.URL + "/s/" + lid + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("过期应 410，得到 %d", resp.StatusCode)
	}

	// 清扫器把它删掉
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "sweep.json"))
	defer st.Close()
	_ = st
}

func TestGateToken(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "t.json"))
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "cfg.json") // 审计/数据落临时目录，不落仓库
	cfg.AuthToken = "tok"
	cfg.CookieSecret = "sec"
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})

	tok := srv.gateToken("ABCD1234", time.Now().Add(time.Hour))
	if !srv.gateTokenValid("ABCD1234", tok) {
		t.Fatal("新签的 token 应有效")
	}
	if srv.gateTokenValid("ABCD9999", tok) {
		t.Fatal("换站点 id 应失效")
	}
	if srv.gateTokenValid("ABCD1234", tok+".tampered") {
		t.Fatal("篡改应失效")
	}
	expired := srv.gateToken("ABCD1234", time.Now().Add(-time.Minute))
	if srv.gateTokenValid("ABCD1234", expired) {
		t.Fatal("过期应失效")
	}
}

func TestWildcardHostAndSPAFallback(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "w.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "cfg.json") // 审计/数据落临时目录，不落仓库
	cfg.AuthToken = "tok"
	cfg.CookieSecret = "sec"
	cfg.SiteWildcardHost = "s.local"
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// 模拟 Vite 产物：绝对路径 assets + 客户端路由 + 自定义 404 页
	zip1 := zipSite(map[string]string{
		"index.html":             `<html><body><h1>spa-home</h1><script src="/assets/app-Ab12cd34.js"></script></body></html>`,
		"assets/app-Ab12cd34.js": "console.log(42)",
		"404.html":               "<p>custom-404</p>",
	})
	resp, body := postZip(ts, "tok", "POST", "/api/v1/sites", zip1, map[string]string{"spa": "1", "name": "SPA 站"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应 201: %s", body)
	}
	site := mustJSON(t, body)
	if !strings.Contains(site["url"].(string), ".s.local/") {
		t.Fatalf("泛域名模式下 url 应为整域链接: %v", site["url"])
	}
	lid := strings.ToLower(site["id"].(string))

	hostGet := func(host, path string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = host
		r, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r, b
	}
	wh := lid + ".s.local"

	// 泛域名根路径 → 入口页（presigned 302 跟随）
	r, b := hostGet(wh, "/")
	if r.StatusCode != 200 || !strings.Contains(string(b), "spa-home") {
		t.Fatalf("泛域名首页应 200 spa-home: %d %q", r.StatusCode, b)
	}
	// 泛域名绝对路径 assets → 命中（Vite base:/ 语义）
	r, b = hostGet(wh, "/assets/app-Ab12cd34.js")
	if r.StatusCode != 200 || string(b) != "console.log(42)" {
		t.Fatalf("泛域名 assets 应 200: %d %q", r.StatusCode, b)
	}
	// SPA 深链接 → 回退入口页 200
	r, b = hostGet(wh, "/team/deep")
	if r.StatusCode != 200 || !strings.Contains(string(b), "spa-home") {
		t.Fatalf("SPA 深链接应回退入口页: %d %q", r.StatusCode, b)
	}
	// 资源未命中 → 普通 404（不回退）
	r, _ = hostGet(wh, "/nope-abc.js")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("资源未命中应 404，得到 %d", r.StatusCode)
	}
	// SPA 站点 HTML 未命中 → 一律回入口页（404 由前端路由渲染）
	r, b = hostGet(wh, "/missing-page")
	if r.StatusCode != 200 || !strings.Contains(string(b), "spa-home") {
		t.Fatalf("SPA 站点 HTML 未命中应回入口页: %d %q", r.StatusCode, b)
	}
	// 路径模式深链接同样回退（spa=1）
	req, _ := http.NewRequest("GET", ts.URL+"/s/"+lid+"/team", nil)
	r, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "spa-home") {
		t.Fatalf("路径模式 SPA 回退失效: %d %q", r.StatusCode, b)
	}

	// 非 SPA 站点：HTML 未命中输出 404.html（404 状态）
	zip2 := zipSite(map[string]string{
		"index.html": "<h1>plain</h1>",
		"404.html":   "<p>custom-404</p>",
	})
	resp, body = postZip(ts, "tok", "POST", "/api/v1/sites", zip2, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建普通站点应 201: %s", body)
	}
	plain := strings.ToLower(mustJSON(t, body)["id"].(string))
	r, b = hostGet(plain+".s.local", "/missing")
	if r.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "custom-404") {
		t.Fatalf("非 SPA 站点应输出 404.html: %d %q", r.StatusCode, b)
	}

	// domain-ask：存在的站点 200，不存在的 403
	req, _ = http.NewRequest("GET", ts.URL+"/domain-ask?domain="+wh, nil)
	r, _ = ts.Client().Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("domain-ask 应 200，得到 %d", r.StatusCode)
	}
	req, _ = http.NewRequest("GET", ts.URL+"/domain-ask?domain=zzzzzzzz.s.local", nil)
	r, _ = ts.Client().Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("domain-ask 未命中应 403，得到 %d", r.StatusCode)
	}
}
