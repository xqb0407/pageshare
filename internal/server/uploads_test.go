package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pageshare/internal/mcpserver"
	"pageshare/internal/storage"
)

// putArchivePath 从 begin 返回的 put_url 提取 httptest 可用的路径。
func putArchivePath(u string) string {
	if i := strings.Index(u, "/api/v1/uploads/"); i >= 0 {
		return u[i:]
	}
	return u
}

func putUpload(ts *httptest.Server, token, path string, body []byte) (*http.Response, []byte) {
	req, _ := http.NewRequest("PUT", ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/zip")
	resp, err := ts.Client().Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func durPtr(d time.Duration) *time.Duration { return &d }

// TestTwoStageUploadNewSite 验证 begin → PUT → commit 全流程（新建）。
func TestTwoStageUploadNewSite(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	ctx := context.Background()

	begin, err := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{
		Name: "远程站", TTL: durPtr(48 * time.Hour),
	}, 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	uid := begin["upload_id"].(string)
	if uid == "" || begin["expires_in"].(int64) != 900 {
		t.Fatalf("begin 返回异常: %v", begin)
	}

	zipb := zipSite(map[string]string{"index.html": `<h1>remote</h1>`})
	resp, body := putUpload(ts, token, putArchivePath(begin["put_url"].(string)), zipb)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT 应 200: %d %s", resp.StatusCode, body)
	}

	out, err := srv.CommitUpload(ctx, uid)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if out["name"] != "远程站" || out["version"].(int64) != 1 {
		t.Fatalf("commit 返回异常: %v", out)
	}
	// 内容真的可访问
	lid := strings.ToLower(out["id"].(string))
	r, err := ts.Client().Get(ts.URL + "/s/" + lid + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !bytes.Contains(page, []byte("remote")) {
		t.Fatalf("站点应可访问: %d %q", r.StatusCode, page)
	}
	// TTL 随发布参数生效
	if out["expires_at"] == nil {
		t.Fatalf("ttl 应写入: %v", out)
	}
	// 票据一次性
	if _, err := srv.CommitUpload(ctx, uid); err == nil {
		t.Fatal("重复 commit 应报错")
	}
}

// TestTwoStageUploadOverwrite 验证绑定已有站点的覆盖发布（版本 +1，URL 不变）。
func TestTwoStageUploadOverwrite(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	_, body := postZip(ts, token, "POST", "/api/v1/sites",
		zipSite(map[string]string{"index.html": `<h1>v1</h1>`}), map[string]string{"name": "覆盖"})
	created := mustJSON(t, body)
	id := created["id"].(string)

	// 小写 id 也可（NormalizeID）
	begin, err := srv.BeginUpload(context.Background(), strings.ToLower(id), mcpserver.ServerUploadOpts{}, 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if begin["site_id"].(string) != id {
		t.Fatalf("site_id 应规范化回传: %v", begin)
	}
	_, pbody := putUpload(ts, token, putArchivePath(begin["put_url"].(string)),
		zipSite(map[string]string{"index.html": `<h1>v2</h1>`}))
	respOK := strings.Contains(string(pbody), "upload_id")
	if !respOK {
		t.Fatalf("PUT 失败: %s", pbody)
	}
	out, err := srv.CommitUpload(context.Background(), begin["upload_id"].(string))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if out["version"].(int64) != 2 || out["id"] != id {
		t.Fatalf("覆盖应版本 2 且同 id: %v", out)
	}

	// begin 绑不存在的站点 → fail fast
	if _, err := srv.BeginUpload(context.Background(), "AAAAAAAA", mcpserver.ServerUploadOpts{}, 0); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("未知站点 begin 应报不存在: %v", err)
	}
}

// TestTwoStageUploadErrors 覆盖错误路径。
func TestTwoStageUploadErrors(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	ctx := context.Background()

	// PUT 未知票据 → 404
	resp, _ := putUpload(ts, token, "/api/v1/uploads/0123456789abcdef01234567", []byte("x"))
	if resp.StatusCode != 404 {
		t.Fatalf("未知票据 PUT 应 404: %d", resp.StatusCode)
	}
	// 无鉴权 → 401
	req, _ := http.NewRequest("PUT", ts.URL+"/api/v1/uploads/deadbeef", bytes.NewReader([]byte("x")))
	r401, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r401.Body.Close()
	if r401.StatusCode != 401 {
		t.Fatalf("未鉴权应 401: %d", r401.StatusCode)
	}
	// 空体 → 400
	begin, err := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ = putUpload(ts, token, putArchivePath(begin["put_url"].(string)), nil); resp.StatusCode != 400 {
		t.Fatalf("空上传体应 400: %d", resp.StatusCode)
	}
	// 未 PUT 直接 commit → 报错
	if _, err := srv.CommitUpload(ctx, begin["upload_id"].(string)); err == nil ||
		!strings.Contains(err.Error(), "暂存") {
		t.Fatalf("先 commit 应提示未上传: %v", err)
	}
	// 非法 expected_bytes / 超上限
	if _, err := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{}, -5); err == nil {
		t.Fatal("负 expected_bytes 应报错")
	}
	if _, err := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{}, srv.Cfg.MaxUploadBytes()+1); err == nil {
		t.Fatal("超上限 expected_bytes 应报错")
	}
	// 非归档字节：PUT 垃圾再 commit → 发布流水线拒绝
	begin2, _ := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{}, 0)
	putUpload(ts, token, putArchivePath(begin2["put_url"].(string)), []byte("not-an-archive"))
	if _, err := srv.CommitUpload(ctx, begin2["upload_id"].(string)); err == nil {
		t.Fatal("垃圾归档 commit 应报错")
	}
	// 失败的 commit 不消费票据：重新 PUT 正常归档可以成功
	_, pbody := putUpload(ts, token, putArchivePath(begin2["put_url"].(string)),
		zipSite(map[string]string{"index.html": `<h1>retry</h1>`}))
	if !strings.Contains(string(pbody), "upload_id") {
		t.Fatalf("重传失败: %s", pbody)
	}
	if out, err := srv.CommitUpload(ctx, begin2["upload_id"].(string)); err != nil || out["version"].(int64) != 1 {
		t.Fatalf("重试 commit 应成功: %v %v", out, err)
	}
}

// TestUploadExpirySweep 验证过期票据：take 拒绝 + sweeper 清暂存对象。
func TestUploadExpirySweep(t *testing.T) {
	ts, srv, token := newTestServerFull(t)
	ctx := context.Background()

	begin, _ := srv.BeginUpload(ctx, "", mcpserver.ServerUploadOpts{}, 0)
	uid := begin["upload_id"].(string)
	putUpload(ts, token, putArchivePath(begin["put_url"].(string)),
		zipSite(map[string]string{"index.html": `<h1>x</h1>`}))

	// 人为把票据弄过期
	srv.uploads.mu.Lock()
	srv.uploads.byID[uid].ExpiresAt = time.Now().Add(-time.Second)
	srv.uploads.mu.Unlock()

	srv.expireUploads(ctx)
	if _, err := srv.Storage.Get(ctx, uploadStageKey(uid)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("过期暂存对象应被清掉，得到 err=%v", err)
	}
	if _, err := srv.CommitUpload(ctx, uid); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期 commit 应报错: %v", err)
	}
}
