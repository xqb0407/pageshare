package site

import (
	"regexp"
	"strings"
)

// hashAssetRe 匹配 Vite/webpack 风格带内容 hash 的构建产物名（如 app-BWmhuAJZ.js）。
var hashAssetRe = regexp.MustCompile(`-[A-Za-z0-9_-]{6,12}\.(js|mjs|css|map|woff2?|png|jpe?g|svg|webp|avif|gif|ico|txt)$`)

// CacheControlFor 给出 nginx 风格缓存策略：html 每次校验，带 hash 的构建产物长缓存，其余 1 小时。
func CacheControlFor(rel, ct string) string {
	if strings.HasPrefix(ct, "text/html") {
		return "no-cache"
	}
	if hashAssetRe.MatchString(rel) {
		return "public, max-age=31536000, immutable"
	}
	return "public, max-age=3600"
}
