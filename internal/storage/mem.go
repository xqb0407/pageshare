package storage

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Mem 是纯内存实现，用于 -dev 试跑、单测与 e2e。
// PresignGet 返回同源绝对路径 /__memstore/<key>，由服务端挂载的只读处理器回源，
// 这样 e2e 的 302 跟随也能拿到内容。
type Mem struct {
	mu      sync.RWMutex
	objects map[string]*memObject
}

type memObject struct {
	data        []byte
	contentType string
}

func NewMem() *Mem { return &Mem{objects: map[string]*memObject{}} }

func (m *Mem) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &memObject{data: data, contentType: contentType}
	return nil
}

func (m *Mem) Get(_ context.Context, key string) (*Object, error) {
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return &Object{
		Body:        io.NopCloser(strings.NewReader(string(obj.data))),
		ContentType: obj.contentType,
		Size:        int64(len(obj.data)),
	}, nil
}

func (m *Mem) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			delete(m.objects, k)
			n++
		}
	}
	return n, nil
}

func (m *Mem) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	if _, ok := m.objects[key]; !ok {
		return "", ErrNotFound
	}
	return "/__memstore/" + key, nil
}
