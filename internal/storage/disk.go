package storage

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Disk 把对象落成本地目录里的普通文件（root/<key>），
// 没有 R2 凭据时的持久化方案：备份 = 拷目录，重启无损。
// PresignGet 返回同源绝对路径 /__diskstore/<key>，由 Handler() 回源。
type Disk struct {
	root string
}

func NewDisk(root string) (*Disk, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: root}, nil
}

func (d *Disk) path(key string) (string, error) {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", ErrNotFound
	}
	return filepath.Join(d.root, filepath.FromSlash(key)), nil
}

func (d *Disk) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d *Disk) Get(_ context.Context, key string) (*Object, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Object{Body: f, Size: fi.Size()}, nil
}

func (d *Disk) DeletePrefix(_ context.Context, prefix string) (int, error) {
	n := 0
	err := filepath.WalkDir(d.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(rel), prefix) {
			if os.Remove(p) == nil {
				n++
			}
		}
		return nil
	})
	// 尽力剪掉空目录
	_ = pruneEmptyDirs(d.root)
	return n, err
}

func pruneEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // 从深到浅
		if dirs[i] == root {
			continue
		}
		_ = os.Remove(dirs[i]) // 非空目录会失败，忽略
	}
	return nil
}

func (d *Disk) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	p, err := d.path(key)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return "/__diskstore/" + key, nil
}

// Handler 挂在 GET /__diskstore/{key...}：用 http.ServeFile 回源（自带 Content-Type/Range）。
func (d *Disk) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(path.Clean(r.URL.Path), "/__diskstore/")
		p, err := d.path(key)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	})
}
