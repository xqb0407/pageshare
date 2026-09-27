package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// patchSite 发 JSON PATCH 请求（管理鉴权）。
func patchSite(ts *httptest.Server, token, id string, body map[string]any) (*http.Response, []byte) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/sites/"+id, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TestPatchSiteMeta 验证不重传的元数据修改：改名/开关 SPA/TTL/密码，版本不变。
func TestPatchSiteMeta(t *testing.T) {
	ts, token := newTestServer(t)
	zipb := zipSite(map[string]string{"index.html": `<h1>patch</h1>`})
	_, body := postZip(ts, token, "POST", "/api/v1/sites", zipb, map[string]string{"name": "old"})
	created := mustJSON(t, body)
	id, _ := created["id"].(string)
	v0, _ := created["version"].(float64)

	// 改名 + 开 SPA（版本不涨）
	resp, body := patchSite(ts, token, id, map[string]any{"name": "新名", "spa": true})
	if resp.StatusCode != 200 {
		t.Fatalf("PATCH 应 200: %d %s", resp.StatusCode, body)
	}
	got := mustJSON(t, body)
	if got["name"] != "新名" || got["spa"] != true {
		t.Fatalf("改名/SPA 未生效: %s", body)
	}
	if got["version"].(float64) != v0 {
		t.Fatalf("元数据修改不应涨版本: %s", body)
	}

	// 设 TTL → expires_at 出现；never → 消失
	_, body = patchSite(ts, token, id, map[string]any{"ttl": "1h"})
	if exp := mustJSON(t, body)["expires_at"]; exp == nil || exp == "" {
		t.Fatalf("ttl 后应有 expires_at: %s", body)
	}
	_, body = patchSite(ts, token, id, map[string]any{"ttl": "never"})
	if exp := mustJSON(t, body)["expires_at"]; exp != nil {
		t.Fatalf("never 应清掉 expires_at: %s", body)
	}
	// 非法 ttl
	resp, _ = patchSite(ts, token, id, map[string]any{"ttl": "bogus"})
	if resp.StatusCode != 400 {
		t.Fatalf("非法 ttl 应 400: %d", resp.StatusCode)
	}

	// 设密码 → 公开面 401（gate）；清密码 → 200
	_, body = patchSite(ts, token, id, map[string]any{"password": "pw"})
	if mustJSON(t, body)["has_password"] != true {
		t.Fatalf("设密码未生效: %s", body)
	}
	rpw, err := ts.Client().Get(ts.URL + "/s/" + id + "/")
	if err != nil {
		t.Fatal(err)
	}
	rpw.Body.Close()
	if rpw.StatusCode != 401 {
		t.Fatalf("有密码站未登录应 401: %d", rpw.StatusCode)
	}
	_, body = patchSite(ts, token, id, map[string]any{"password": ""})
	if mustJSON(t, body)["has_password"] != false {
		t.Fatalf("清密码未生效: %s", body)
	}
	r2, err := ts.Client().Get(ts.URL + "/s/" + id + "/")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("清密码后应可访问: %d", r2.StatusCode)
	}

	// 空 body / 未知站点 / 未鉴权
	if resp, _ = patchSite(ts, token, id, map[string]any{}); resp.StatusCode != 400 {
		t.Fatalf("空 PATCH 应 400: %d", resp.StatusCode)
	}
	if resp, _ = patchSite(ts, token, "ZZZZZZZZ", map[string]any{"name": "x"}); resp.StatusCode != 404 {
		t.Fatalf("未知站点应 404: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/sites/"+id, strings.NewReader(`{"name":"x"}`))
	r401, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r401.Body.Close()
	if r401.StatusCode != 401 {
		t.Fatalf("未鉴权应 401: %d", r401.StatusCode)
	}
}
