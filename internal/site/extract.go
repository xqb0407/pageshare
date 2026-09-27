package site

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"
)

// Limits 约束一次上传解包的规模，全部超限立即中止。
type Limits struct {
	MaxCompressed int64 // 压缩体字节数上限
	MaxFiles      int   // 文件数上限
	MaxTotalBytes int64 // 解压后总量上限
}

// ArchiveSummary 一次成功解包的摘要。
type ArchiveSummary struct {
	Files      int
	TotalBytes int64
	// Entry 是站点入口页：优先根 index.html，否则第一个 *.html。
	Entry string
}

// PutFile 由解包方注入的落盘回调：key 为相对路径，body 只在本调用内有效。
type PutFile func(relPath string, body []byte, contentType string) error

// Extract 识别并解包 zip / tar.gz 上传体，逐文件调用 put 落盘。
// 整个上传体先按限额读入内存（≤ MaxCompressed），再流式展开。
func Extract(body []byte, limits Limits, put PutFile) (ArchiveSummary, error) {
	var sum ArchiveSummary
	if int64(len(body)) > limits.MaxCompressed {
		return sum, fmt.Errorf("上传体 %d 字节超过上限 %d", len(body), limits.MaxCompressed)
	}
	if len(body) >= 4 && bytes.Equal(body[:4], []byte("PK\x03\x04")) {
		return extractZip(body, limits, put)
	}
	return extractTarGz(body, limits, put)
}

func extractZip(body []byte, limits Limits, put PutFile) (ArchiveSummary, error) {
	var sum ArchiveSummary
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return sum, fmt.Errorf("无法解析 zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, err := SanitizeRelPath(f.Name)
		if err != nil {
			return sum, err
		}
		if IgnorableEntry(rel) {
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // 拒绝符号链接
		}
		rc, err := f.Open()
		if err != nil {
			return sum, fmt.Errorf("打开 %s: %w", rel, err)
		}
		data, ok, err := readCapped(rc, limits.MaxTotalBytes-sum.TotalBytes)
		rc.Close()
		if err != nil {
			return sum, fmt.Errorf("读取 %s: %w", rel, err)
		}
		if !ok {
			return sum, fmt.Errorf("解压总量超过上限 %d 字节", limits.MaxTotalBytes)
		}
		if err := accept(&sum, rel, data, limits, put); err != nil {
			return sum, err
		}
	}
	if sum.Files == 0 {
		return sum, fmt.Errorf("压缩包中没有可用文件")
	}
	return sum, nil
}

func extractTarGz(body []byte, limits Limits, put PutFile) (ArchiveSummary, error) {
	var sum ArchiveSummary
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return sum, fmt.Errorf("上传体既不是 zip 也不是 tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, fmt.Errorf("解析 tar: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			continue
		default: // 符号链接、硬链接等一律拒绝
			continue
		}
		rel, err := SanitizeRelPath(hdr.Name)
		if err != nil {
			return sum, err
		}
		if IgnorableEntry(rel) {
			continue
		}
		data, ok, err := readCapped(tr, limits.MaxTotalBytes-sum.TotalBytes)
		if err != nil {
			return sum, fmt.Errorf("读取 %s: %w", rel, err)
		}
		if !ok {
			return sum, fmt.Errorf("解压总量超过上限 %d 字节", limits.MaxTotalBytes)
		}
		if err := accept(&sum, rel, data, limits, put); err != nil {
			return sum, err
		}
	}
	if sum.Files == 0 {
		return sum, fmt.Errorf("压缩包中没有可用文件")
	}
	return sum, nil
}

// accept 校验计数并落盘单个文件。
func accept(sum *ArchiveSummary, rel string, data []byte, limits Limits, put PutFile) error {
	if sum.Files >= limits.MaxFiles {
		return fmt.Errorf("文件数超过上限 %d", limits.MaxFiles)
	}
	ct := ContentTypeFor(rel)
	if err := put(rel, data, ct); err != nil {
		return fmt.Errorf("写入 %s: %w", rel, err)
	}
	sum.Files++
	sum.TotalBytes += int64(len(data))
	// 入口页候选：跳过 404.html（它是错误页，不能当站点入口）
	if sum.Entry == "" && strings.HasSuffix(rel, ".html") && rel != "404.html" {
		sum.Entry = rel
	}
	if rel == "index.html" {
		sum.Entry = "index.html"
	}
	return nil
}

// readCapped 最多读 cap 字节；ok=false 表示源内容超出 cap。
func readCapped(r io.Reader, cap int64) ([]byte, bool, error) {
	if cap < 0 {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > cap {
		return nil, false, nil
	}
	return data, true, nil
}

// ContentTypeFor 按扩展名给出对象 Content-Type。
func ContentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
