// Package site 站点侧的纯逻辑：id 生成、归档解包清洗、TTL 表达。
package site

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// idAlphabet 是 Crockford Base32（去掉 I L O U），8 字符 ≈ 40 bit 随机空间。
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const idLen = 8

// NewID 生成一个随机站点 id（大写 Crockford）。
func NewID() (string, error) {
	out := make([]byte, idLen)
	max := big.NewInt(int64(len(idAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = idAlphabet[n.Int64()]
	}
	return string(out), nil
}

// NormalizeID 校验并规范用户传入的站点 id（大小写不敏感）。
func NormalizeID(raw string) (string, error) {
	id := strings.ToUpper(strings.TrimSpace(raw))
	if len(id) != idLen {
		return "", fmt.Errorf("站点 id 长度应为 %d", idLen)
	}
	for _, c := range id {
		if !strings.ContainsRune(idAlphabet, c) {
			// 容忍易混字符
			switch c {
			case 'I', 'L':
				id = replaceChar(id, strings.IndexRune(id, c), '1')
			case 'O':
				id = replaceChar(id, strings.IndexRune(id, c), '0')
			default:
				return "", fmt.Errorf("站点 id 含非法字符 %q", c)
			}
		}
	}
	return id, nil
}

func replaceChar(s string, i int, c byte) string {
	if i < 0 {
		return s
	}
	return s[:i] + string(c) + s[i+1:]
}

// ParseTTL 解析 TTL 表达式：Go duration（72h30m）或 <n>d（7d）；0 或 "never" 表示永久。
func ParseTTL(raw string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" || s == "never" || s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err != nil || days <= 0 {
			return 0, fmt.Errorf("无法解析 TTL %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("无法解析 TTL %q", raw)
	}
	return d, nil
}

const MaxTTL = 365 * 24 * time.Hour

// ErrPathUnsafe 表示归档条目路径不安全（zip-slip 类）。
var ErrPathUnsafe = errors.New("归档条目路径不安全")

// SanitizeRelPath 清洗归档内的相对路径：拒绝绝对路径、".."、反斜杠；返回 unix 风格路径。
func SanitizeRelPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: 空路径", ErrPathUnsafe)
	}
	if strings.Contains(name, "\\") || strings.Contains(name, "\x00") {
		return "", fmt.Errorf("%w: %q", ErrPathUnsafe, name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: 绝对路径 %q", ErrPathUnsafe, name)
	}
	parts := strings.Split(name, "/")
	var out []string
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: %q 越界", ErrPathUnsafe, name)
		default:
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("%w: %q 无有效段", ErrPathUnsafe, name)
	}
	return strings.Join(out, "/"), nil
}

// IgnorableEntry 判断是否跳过系统杂物（macOS 元数据、git 目录等）。
func IgnorableEntry(path string) bool {
	if strings.EqualFold(path, ".ds_store") {
		return true
	}
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "__MACOSX", ".git", ".DS_Store":
			return true
		}
	}
	return strings.HasPrefix(path, "__MACOSX/")
}
