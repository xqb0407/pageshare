// Package storage 抽象站点内容的对象存储。
// 桶保持私有：无密码站点用 PresignGet 发 302，有密码站点走 Get 代理回源。
package storage

import (
	"context"
	"io"
	"time"
)

// ErrNotFound 表示对象或前缀下没有内容。
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "object not found" }

// Object 是一次 Get 的结果；Body 由调用方关闭。
type Object struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

// Storage 是站点对象存储的最小接口。
// 约定：key 是已清洗的相对路径（site id/version/文件路径），不含 ".." 或前导 "/"。
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (*Object, error)
	// DeletePrefix 删除某前缀下全部对象，返回删除数量。
	DeletePrefix(ctx context.Context, prefix string) (int, error)
	// PresignGet 返回一个限时 GET URL；实现可返回绝对 URL 或同源绝对路径。
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}
