package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskBackend(t *testing.T) {
	root := t.TempDir()
	d, err := NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Put → Get
	if err := d.Put(ctx, "ABC123/1/index.html", strings.NewReader("<h1>hi</h1>"), 12, "text/html"); err != nil {
		t.Fatal(err)
	}
	obj, err := d.Get(ctx, "ABC123/1/index.html")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(data) != "<h1>hi</h1>" {
		t.Fatalf("内容不符: %q", data)
	}

	// 文件真的在磁盘上
	if _, err := os.Stat(filepath.Join(root, "ABC123", "1", "index.html")); err != nil {
		t.Fatalf("文件应落盘: %v", err)
	}

	// 不存在
	if _, err := d.Get(ctx, "ABC123/1/nope.js"); err != ErrNotFound {
		t.Fatalf("应 ErrNotFound，得到 %v", err)
	}
	if _, err := d.PresignGet(ctx, "ABC123/1/nope.js", 0); err != ErrNotFound {
		t.Fatal("presign 不存在应 ErrNotFound")
	}
	loc, err := d.PresignGet(ctx, "ABC123/1/index.html", 0)
	if err != nil || loc != "/__diskstore/ABC123/1/index.html" {
		t.Fatalf("presign 路径不符: %q %v", loc, err)
	}

	// DeletePrefix 只删前缀内的
	if err := d.Put(ctx, "ABC123/2/b.js", strings.NewReader("b"), 1, ""); err != nil {
		t.Fatal(err)
	}
	n, err := d.DeletePrefix(ctx, "ABC123/1/")
	if err != nil || n != 1 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if _, err := d.Get(ctx, "ABC123/1/index.html"); err != ErrNotFound {
		t.Fatal("前缀内文件应被删")
	}
	if _, err := d.Get(ctx, "ABC123/2/b.js"); err != nil {
		t.Fatal("前缀外文件应保留")
	}

	// 路径安全
	if _, err := d.path("../escape"); err == nil {
		t.Fatal(".. 应被拒绝")
	}
}
