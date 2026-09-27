package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PackDirectory 把本地目录打包成 tar.gz（供 MCP publish_site 使用）。
// 跳过 .git、node_modules、.DS_Store、__MACOSX 等杂物；path 为 .zip/.tgz 文件时原样返回。
func PackDirectory(root string) ([]byte, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		// 已是归档文件：直接读（格式由 Extract 按魔数识别）
		return os.ReadFile(root)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignorableWalk(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil // tar 里不需要目录条目，Extract 也不认
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接等一律跳过，与解包侧策略一致
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name: rel,
			Mode: int64(d.Type().Perm()),
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ignorableWalk 判断打包时是否跳过（目录级剪枝 + 文件级过滤）。
func ignorableWalk(rel string, d fs.DirEntry) bool {
	name := d.Name()
	if name == ".git" || name == "node_modules" || name == "__MACOSX" || name == ".DS_Store" {
		return true
	}
	if strings.HasSuffix(rel, ".db") || strings.HasSuffix(rel, ".db-shm") || strings.HasSuffix(rel, ".db-wal") {
		return true
	}
	return false
}
