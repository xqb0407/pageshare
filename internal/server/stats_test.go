package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pageshare/internal/store"
)

// adminGet 带管理 token 发 GET，返回解析后的 JSON。
func adminGet(t *testing.T, ts *httptest.Server, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s 应 200: %d %s", path, resp.StatusCode, b)
	}
	return mustJSON(t, b)
}

func getSiteRow(t *testing.T, srv *Server, id string) *store.Site {
	t.Helper()
	st, err := srv.Store.GetSite(id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestPageViewTracking 验证页览计数：入口页/.html/SPA 深链计入，静态资源不计，
// pending 读取即并入，flushVisits 持久化到 store。
func TestPageViewTracking(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	zipb := zipSite(map[string]string{
		"index.html": `<h1>stats</h1>`,
		"about.html": `<h1>about</h1>`,
		"app.css":    `body{}`,
	})
	_, body := postZip(ts, token, "POST", "/api/v1/sites", zipb, map[string]string{"spa": "1"})
	id, _ := mustJSON(t, body)["id"].(string)

	get := func(p string) {
		r, err := ts.Client().Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	base := "/s/" + id + "/"

	get(base)                // 入口计
	get(base + "about.html") // .html 计
	get(base + "some-route") // SPA 深链计
	get(base + "app.css")    // 资源不计

	// 3 次页览：内存 pending 立即可见（未刷盘）
	if got := srv.liveVisits(getSiteRow(t, srv, id)); got != 3 {
		t.Fatalf("页览应计 3（css 不计）: %d", got)
	}

	// 管理 API 输出含 pending
	if v, _ := adminGet(t, ts, token, "/api/v1/sites/"+id)["visits"].(float64); v != 3 {
		t.Fatalf("API visits 应实时含 pending=3")
	}

	// 刷盘并验证持久化
	srv.flushVisits()
	st := getSiteRow(t, srv, id)
	if st.Visits != 3 {
		t.Fatalf("flush 后 store visits 应为 3: %d", st.Visits)
	}
	if st.LastVisitedAt.IsZero() {
		t.Fatal("flush 后应记录 last_visit_at")
	}
	// 再访问一次并刷盘：累计到 4
	get(base)
	srv.flushVisits()
	if got := getSiteRow(t, srv, id).Visits; got != 4 {
		t.Fatalf("第二轮 flush 后应为 4: %d", got)
	}
	// 空增量 flush 不应报错也不改动
	srv.flushVisits()
	if got := getSiteRow(t, srv, id).Visits; got != 4 {
		t.Fatalf("空 flush 不应改动: %d", got)
	}
}

// TestPageViewGateNotCounted 验证密码站未过 gate 的请求不计页览，过 gate 后计入。
func TestPageViewGateNotCounted(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	zipb := zipSite(map[string]string{"index.html": `<h1>gate</h1>`})
	_, body := postZip(ts, token, "POST", "/api/v1/sites", zipb, map[string]string{"password": "pw"})
	id, _ := mustJSON(t, body)["id"].(string)

	// 未过 gate：401，不计
	r, err := ts.Client().Get(ts.URL + "/s/" + id + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("未过 gate 应 401: %d", r.StatusCode)
	}
	if got := srv.liveVisits(getSiteRow(t, srv, id)); got != 0 {
		t.Fatalf("gate 拒绝不应计页览: %d", got)
	}

	// 过 gate：POST /s/{id}/gate 拿 cookie（不跟随 303，cookie 在跳转响应上），再带 cookie 访问
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	gresp, err := client.Post(ts.URL+"/s/"+id+"/gate",
		"application/x-www-form-urlencoded", strings.NewReader("password=pw&next="))
	if err != nil {
		t.Fatal(err)
	}
	gresp.Body.Close()
	var cookie *http.Cookie
	for _, c := range gresp.Cookies() {
		if c.Name == gateCookieName(id) {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("gate 应下发会话 cookie")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/s/"+id+"/", nil)
	req.AddCookie(cookie)
	r2, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("带 cookie 应 200: %d", r2.StatusCode)
	}
	if got := srv.liveVisits(getSiteRow(t, srv, id)); got != 1 {
		t.Fatalf("过 gate 后页览应为 1: %d", got)
	}
}
