package server

// 引导模式 setup API 的行为测试：配置落盘语义、参数校验。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pageshare/internal/config"
)

// newTestSetupService 起一个独立引导服务，配置写到临时目录。
func newTestSetupService(t *testing.T) (*httptest.Server, *SetupService) {
	t.Helper()
	dir := t.TempDir()
	svc := NewSetupService(":8300", filepath.Join(dir, "pageshare.json"))
	ts := httptest.NewServer(svc)
	t.Cleanup(ts.Close)
	return ts, svc
}

func postJSON(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestSetupComplete(t *testing.T) {
	ts, svc := newTestSetupService(t)
	dir := filepath.Dir(svc.configPath)

	// status：报告 needs_setup 与建议值
	resp, err := http.Get(ts.URL + "/api/v1/setup/status")
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	status := mustJSON(t, sb)
	if status["needs_setup"] != true {
		t.Fatalf("引导模式应报告 needs_setup=true: %v", status)
	}
	if status["suggested_auth_token"].(string) == "" || status["listen"].(string) != ":8300" {
		t.Fatalf("status 应含建议 token 与 listen: %v", status)
	}

	// SPA 入口页：向导路径回 index.html（200）
	resp, err = http.Get(ts.URL + "/admin/setup")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "<html") {
		t.Fatalf("/admin/setup 应回入口页 200，得到 %d", resp.StatusCode)
	}

	// 合法提交 → 200 + 落盘
	cfgBody := map[string]any{
		"auth_token":         "my-token",
		"cookie_secret":      "my-secret",
		"public_base_url":    "https://share.example.com/", // 故意带尾斜杠，应被规范化
		"site_wildcard_host": "",
		"listen":             ":8300",
		"db_path":            "sites.json",
		"max_upload_mb":      100,
		"storage":            map[string]any{"backend": "disk", "root": "./data"},
	}
	raw, _ := json.Marshal(cfgBody)
	resp, body := postJSON(t, ts.URL+"/api/v1/setup/complete", string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法提交应 200，得到 %d: %s", resp.StatusCode, body)
	}
	out := mustJSON(t, body)
	if out["ok"] != true {
		t.Fatalf("应返回 ok=true: %v", out)
	}

	// 文件内容可按 config 语义解析，auth_token / 规范化结果正确
	data, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("配置应已落盘: %v", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("落盘内容应可反序列化为 config.Config: %v", err)
	}
	if cfg.AuthToken != "my-token" || cfg.CookieSecret != "my-secret" {
		t.Fatalf("auth_token/cookie_secret 应与提交一致: %v", cfg)
	}
	if cfg.PublicBaseURL != "https://share.example.com" {
		t.Fatalf("public_base_url 尾斜杠应被规范化: %q", cfg.PublicBaseURL)
	}
	if cfg.Storage.Backend != "disk" || cfg.Storage.Root != "./data" {
		t.Fatalf("storage 应与提交一致: %v", cfg.Storage)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("落盘配置应通过校验: %v", err)
	}
	// 临时文件不残留
	if _, err := os.Stat(cfgPathTmp(dir)); !os.IsNotExist(err) {
		t.Fatalf("不应残留临时文件")
	}
	// 完成信号已触发（handler 留有 200ms 响应送达窗口）
	<-svc.Done()
}

// cfgPathTmp 拼出与 Save 一致的临时文件名（仅测试断言用）。
func cfgPathTmp(dir string) string { return filepath.Join(dir, "pageshare.json.tmp") }

func TestSetupS3MissingBucket(t *testing.T) {
	ts, _ := newTestSetupService(t)
	raw, _ := json.Marshal(map[string]any{
		"auth_token":      "tok",
		"cookie_secret":   "sec",
		"public_base_url": "http://127.0.0.1:8300",
		"storage": map[string]any{
			"backend":  "s3",
			"endpoint": "http://127.0.0.1:9000",
			"region":   "auto",
			// 缺 bucket
		},
	})
	resp, body := postJSON(t, ts.URL+"/api/v1/setup/complete", string(raw))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("s3 缺 bucket 应 400，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "bucket") {
		t.Fatalf("错误信息应提到 bucket: %s", body)
	}
	// 引导服务不提供业务 API
	resp2, err := http.Get(ts.URL + "/api/v1/sites")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("引导模式不应提供业务 API，得到 %d", resp2.StatusCode)
	}
}
