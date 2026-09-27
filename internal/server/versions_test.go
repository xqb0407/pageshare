package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pageshare/internal/config"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// newKeptServer 起一台指定 versions_kept 的完整服务。
func newKeptServer(t *testing.T, kept int64) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sites.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "cfg.json") // 审计/数据落临时目录，不落仓库
	cfg.AuthToken = "test-token"
	cfg.CookieSecret = "test-cookie-secret"
	cfg.Storage = config.Storage{Backend: "mem"}
	cfg.VersionsKept = kept
	srv := New(Deps{Cfg: cfg, Store: st, Storage: storage.NewMem(), Logger: discardLogger()})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, srv, cfg.AuthToken
}

// hasObject 报告 sitePrefix(id, v) 下是否还有 index.html。
func hasObject(t *testing.T, srv *Server, id string, version int64) bool {
	t.Helper()
	obj, err := srv.Storage.Get(context.Background(), srv.sitePrefix(id, version)+"index.html")
	if err != nil {
		return false
	}
	obj.Body.Close()
	return true
}

func publishZip(t *testing.T, ts *httptest.Server, token, method, path, content string) int64 {
	t.Helper()
	resp, body := postZip(ts, token, method, path, zipSite(map[string]string{"index.html": content}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("发布 %s 失败 %d: %s", path, resp.StatusCode, body)
	}
	j := mustJSON(t, body)
	v, _ := j["version"].(float64)
	return int64(v)
}

func getServed(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestVersionRetentionWindow(t *testing.T) {
	ts, srv, token := newKeptServer(t, 3)
	_, body := postZip(ts, token, "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "v1"}), nil)
	id := mustJSON(t, body)["id"].(string)

	v := int64(1)
	for _, c := range []string{"v2", "v3", "v4"} {
		v = publishZip(t, ts, token, "PUT", "/api/v1/sites/"+id, c)
	}
	if v != 4 {
		t.Fatalf("版本应到 4，得 %d", v)
	}
	// 保留窗口 3（v4 发布后水位 = 4-3 = 1）：v1 已删，v2..v4 在
	if hasObject(t, srv, id, 1) {
		t.Fatal("v1 应已被保留策略删除")
	}
	for _, keep := range []int64{2, 3, 4} {
		if !hasObject(t, srv, id, keep) {
			t.Fatalf("v%d 应在保留窗口内", keep)
		}
	}
	st, _ := srv.Store.GetSite(id)
	if st.PrunedTo != 1 {
		t.Fatalf("水位应为 1，得 %d", st.PrunedTo)
	}

	// kept=0：全保留
	ts0, srv0, token0 := newKeptServer(t, 0)
	_, body = postZip(ts0, token0, "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "v1"}), nil)
	id0 := mustJSON(t, body)["id"].(string)
	publishZip(t, ts0, token0, "PUT", "/api/v1/sites/"+id0, "v2")
	publishZip(t, ts0, token0, "PUT", "/api/v1/sites/"+id0, "v3")
	if !hasObject(t, srv0, id0, 1) || !hasObject(t, srv0, id0, 2) {
		t.Fatal("versions_kept=0 应全部保留")
	}
	if g, _ := srv0.Store.GetSite(id0); g.PrunedTo != 0 {
		t.Fatalf("全保留时水位应为 0，得 %d", g.PrunedTo)
	}

	// kept=1（现行行为）：上一版立即删
	ts1, srv1, token1 := newKeptServer(t, 1)
	_, body = postZip(ts1, token1, "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "v1"}), nil)
	id1 := mustJSON(t, body)["id"].(string)
	publishZip(t, ts1, token1, "PUT", "/api/v1/sites/"+id1, "v2")
	if hasObject(t, srv1, id1, 1) {
		t.Fatal("kept=1 时旧版应立即删除（现行行为）")
	}
}

func TestRollbackSwitchesPointer(t *testing.T) {
	ts, srv, token := newKeptServer(t, 3)
	_, body := postZip(ts, token, "POST", "/api/v1/sites", zipSite(map[string]string{"index.html": "one"}), nil)
	id := mustJSON(t, body)["id"].(string)
	publishZip(t, ts, token, "PUT", "/api/v1/sites/"+id, "two")
	publishZip(t, ts, token, "PUT", "/api/v1/sites/"+id, "three")

	// 版本列表标注
	vers, err := srv.listSiteVersions(id)
	if err != nil || len(vers) != 3 {
		t.Fatalf("listSiteVersions: %v %+v", err, vers)
	}
	if vers[2]["current"] != true || vers[2]["restorable"] != false {
		t.Fatalf("v3 应是 current 且不可回滚: %+v", vers[2])
	}
	if vers[1]["restorable"] != true {
		t.Fatalf("v2 应可回滚: %+v", vers[1])
	}

	// 回滚到 v2：访问内容立即变
	out, err := srv.rollbackSite(context.Background(), strings.ToUpper(id), 2)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if int64(out["version"].(int64)) != 2 {
		t.Fatalf("回滚结果 version 应为 2: %+v", out)
	}
	if code, got := getServed(t, ts, "/s/"+id+"/index.html"); code != 200 || got != "two" {
		t.Fatalf("回滚后应服务 v2 内容，得 %d %q", code, got)
	}
	// 账本行不增、水位不动
	if vs, _ := srv.Store.ListVersions(id); len(vs) != 3 {
		t.Fatalf("回滚不应新增版本行: %+v", vs)
	}

	// 回滚后再发布：版本号取 max(指针, 最新)+1 = 4，不覆盖 v2/v3
	v4 := publishZip(t, ts, token, "PUT", "/api/v1/sites/"+id, "four")
	if v4 != 4 {
		t.Fatalf("回滚后发布版本号应为 4，得 %d", v4)
	}
	if !hasObject(t, srv, id, 2) || !hasObject(t, srv, id, 3) {
		t.Fatal("保留窗口内版本不应被新版本发布误删")
	}
	if code, got := getServed(t, ts, "/s/"+id+"/index.html"); code != 200 || got != "four" {
		t.Fatalf("新版本应立即可见，得 %d %q", code, got)
	}

	// 校验错误路径
	if _, err := srv.rollbackSite(context.Background(), id, 4); err == nil || !strings.Contains(err.Error(), "已是当前版本") {
		t.Fatalf("回滚到当前版本应报错，得 %v", err)
	}
	if _, err := srv.rollbackSite(context.Background(), id, 99); err == nil || !strings.Contains(err.Error(), "无发布记录") {
		t.Fatalf("回滚到无记录版本应报错，得 %v", err)
	}
	if _, err := srv.rollbackSite(context.Background(), id, 1); err == nil || !strings.Contains(err.Error(), "已被保留策略删除") {
		t.Fatalf("v1 已被 prune（水位=1），回滚应被拒，得 %v", err)
	}
	if _, err := srv.rollbackSite(context.Background(), "nope0000", 1); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("未知站点应报不存在，得 %v", err)
	}
}
