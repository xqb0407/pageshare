package site

import (
	"archive/zip"
	"bytes"
	"sort"
	"strings"
	"testing"
)

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != idLen {
			t.Fatalf("id 长度错误: %q", id)
		}
		if seen[id] {
			t.Fatalf("id 重复: %q", id)
		}
		seen[id] = true
	}
}

func TestNormalizeID(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"abcdef23", "ABCDEF23", false},
		{"abCDEF23", "ABCDEF23", false},
		{" abi-def2 ", "", true}, // '-' 不在字母表
		{"abc", "", true},
		{"ABIODEF2", "AB10DEF2", false}, // I→1 O→0
	}
	for _, c := range cases {
		got, err := NormalizeID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeID(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeID(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in   string
		want int64 // 秒；-1 = 报错
	}{
		{"", 0}, {"never", 0}, {"0", 0},
		{"72h", 72 * 3600}, {"7d", 7 * 86400}, {"30m", 1800},
		{"abc", -1}, {"-5h", -1}, {"0d", -1},
	}
	for _, c := range cases {
		d, err := ParseTTL(c.in)
		if c.want == -1 {
			if err == nil {
				t.Errorf("ParseTTL(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTTL(%q): %v", c.in, err)
			continue
		}
		if int64(d.Seconds()) != c.want {
			t.Errorf("ParseTTL(%q) = %v, want %ds", c.in, d, c.want)
		}
	}
}

func TestSanitizeRelPath(t *testing.T) {
	ok := map[string]string{
		"index.html":  "index.html",
		"./a/b/c.css": "a/b/c.css",
		"a//b///c.js": "a/b/c.js",
	}
	for in, want := range ok {
		got, err := SanitizeRelPath(in)
		if err != nil || got != want {
			t.Errorf("SanitizeRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"../evil", "a/../../evil", "..\\evil", "", "a/../..", "\\x", "a\x00b", "/abs/path.html"}
	for _, in := range bad {
		if _, err := SanitizeRelPath(in); err == nil {
			t.Errorf("SanitizeRelPath(%q) 应拒绝", in)
		}
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractZipOK(t *testing.T) {
	body := zipBytes(t, map[string]string{
		"index.html":     "<h1>hello</h1>",
		"assets/a.css":   "body{}",
		"sub/index.html": "<p>sub</p>",
		".DS_Store":      "junk",
		"__MACOSX/x":     "junk",
	})
	var got []string
	sum, err := Extract(body, Limits{MaxCompressed: 1 << 20, MaxFiles: 100, MaxTotalBytes: 1 << 20},
		func(rel string, b []byte, ct string) error {
			got = append(got, rel)
			if rel == "index.html" && ct != "text/html; charset=utf-8" {
				t.Errorf("index.html Content-Type = %q", ct)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 3 || sum.Entry != "index.html" {
		t.Errorf("summary = %+v", sum)
	}
	// 解包顺序跟随 zip 目录序（测试 zip 由 map 构造，天然随机），只约束集合
	sort.Strings(got)
	if strings.Join(got, ",") != "assets/a.css,index.html,sub/index.html" {
		t.Errorf("文件列表 = %v", got)
	}
}

func TestExtractZipSlip(t *testing.T) {
	body := zipBytes(t, map[string]string{"../../evil.txt": "pwned"})
	_, err := Extract(body, Limits{MaxCompressed: 1 << 20, MaxFiles: 10, MaxTotalBytes: 1 << 20},
		func(string, []byte, string) error { return nil })
	if err == nil {
		t.Fatal("zip-slip 应被拒绝")
	}
}

func TestExtractLimits(t *testing.T) {
	body := zipBytes(t, map[string]string{"big.bin": strings.Repeat("x", 1<<20)})
	if _, err := Extract(body, Limits{MaxCompressed: 1 << 20, MaxFiles: 10, MaxTotalBytes: 4096},
		func(string, []byte, string) error { return nil }); err == nil {
		t.Fatal("解压总量超限应报错")
	}
	// 压缩体本身超限
	if _, err := Extract(bytes.Repeat([]byte("x"), 2<<20), Limits{MaxCompressed: 1 << 20, MaxFiles: 10, MaxTotalBytes: 1 << 20},
		func(string, []byte, string) error { return nil }); err == nil {
		t.Fatal("压缩体超限应报错")
	}
}
