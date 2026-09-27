// 两阶段上传：远程 agent 的发布通道。
//
//	begin（签票据）→ PUT /api/v1/uploads/{id}（原始归档字节，暂存区）→ commit（走既有发布流水线）。
//
// 票据在内存（重启即失效，agent 重新 begin 即可，成本一次工具调用）；
// 归档字节暂存于存储的 _uploads/ 前缀，与站点前缀互不相干；过期票据由 sweeper 顺带清扫。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"pageshare/internal/storage"
	"pageshare/internal/store"
)

const (
	// UploadTicketTTL 是票据（含暂存归档）的有效期。
	UploadTicketTTL = 15 * time.Minute
	// uploadStagePrefix 是暂存归档在对象存储里的前缀；
	// 站点 id 字母表不含下划线，永不与站点前缀冲突。
	uploadStagePrefix = "_uploads/"
)

// uploadTicket 是一次两阶段上传的票据：目标站点（空=新建）+ 发布参数（不含归档体）。
type uploadTicket struct {
	ID        string
	SiteID    string
	Form      *PublishOpts // Body 由 commit 时从暂存区读出填充
	Expected  int64        // begin 时的配额预检值；0=未声明
	CreatedAt time.Time
	ExpiresAt time.Time
}

// uploadRegistry 是内存票据表。
type uploadRegistry struct {
	mu   sync.Mutex
	byID map[string]*uploadTicket
}

func newUploadRegistry() *uploadRegistry {
	return &uploadRegistry{byID: map[string]*uploadTicket{}}
}

func (r *uploadRegistry) put(t *uploadTicket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[t.ID] = t
}

// take 返回票据；不存在、已过期或 id 非法都返回 error。过期票据顺带移除。
func (r *uploadRegistry) take(id string) (*uploadTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return nil, errors.New("上传票据不存在（可能已提交、过期或服务重启）")
	}
	if time.Now().After(t.ExpiresAt) {
		delete(r.byID, id)
		return nil, errors.New("上传票据已过期，请重新 begin_upload")
	}
	return t, nil
}

func (r *uploadRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
}

// expired 返回并移除全部过期票据（sweeper 清扫用）。
func (r *uploadRegistry) expired() []*uploadTicket {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var out []*uploadTicket
	for id, t := range r.byID {
		if now.After(t.ExpiresAt) {
			delete(r.byID, id)
			out = append(out, t)
		}
	}
	return out
}

func uploadStageKey(id string) string { return uploadStagePrefix + id + "/archive" }

func newUploadID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// beginUpload 签发票据。siteID 非空时校验站点存在（fail fast，新建/覆盖在 begin 就定死）。
func (s *Server) beginUpload(siteID string, form *PublishOpts, expected int64) (*uploadTicket, error) {
	if expected < 0 {
		return nil, errors.New("expected_bytes 不能为负")
	}
	if max := s.Cfg.MaxUploadBytes(); expected > max {
		return nil, errors.New("expected_bytes 超过上传上限")
	}
	if siteID != "" {
		if _, err := s.Store.GetSite(siteID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errNotFoundSite{siteID}
			}
			return nil, err
		}
	}
	t := &uploadTicket{
		ID:        newUploadID(),
		SiteID:    siteID,
		Form:      form,
		Expected:  expected,
		CreatedAt: time.Now(),
	}
	t.ExpiresAt = t.CreatedAt.Add(UploadTicketTTL)
	s.uploads.put(t)
	return t, nil
}

type errNotFoundSite struct{ id string }

func (e errNotFoundSite) Error() string { return "站点 " + e.id + " 不存在" }

// uploadPutURL 返回数据面地址：配了 public_base_url 用绝对 URL，否则同源相对路径。
func (s *Server) uploadPutURL(id string) string {
	path := "/api/v1/uploads/" + id
	if s.Cfg.PublicBaseURL != "" {
		return s.Cfg.PublicBaseURL + path
	}
	return path
}

// handlePutUpload 是数据面 PUT：原始归档字节写入暂存区（Bearer 管理 token 或 psm_ 密钥）。
func (s *Server) handlePutUpload(w http.ResponseWriter, r *http.Request) {
	t, err := s.uploads.take(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.Cfg.MaxUploadBytes()+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "读取上传体: "+err.Error())
		return
	}
	if int64(len(body)) > s.Cfg.MaxUploadBytes() {
		httpError(w, http.StatusRequestEntityTooLarge, "上传体超过上限")
		return
	}
	if len(body) == 0 {
		httpError(w, http.StatusBadRequest, "上传体为空")
		return
	}
	if err := s.Storage.Put(r.Context(), uploadStageKey(t.ID),
		bytes.NewReader(body), int64(len(body)), r.Header.Get("Content-Type")); err != nil {
		httpError(w, http.StatusInternalServerError, "暂存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id": t.ID,
		"bytes":     len(body),
		"next":      "PUT 完成后调用 commit_upload 发布",
	})
}

// commitUpload 从暂存区读出归档，走与 PublishSite 完全相同的流水线；
// 成功即消费票据。失败保留票据与过期时间（暂存对象已清，agent 重新 PUT 后可再 commit）。
func (s *Server) commitUpload(ctx context.Context, uploadID string) (map[string]any, error) {
	t, err := s.uploads.take(uploadID)
	if err != nil {
		return nil, err
	}
	obj, err := s.Storage.Get(ctx, uploadStageKey(t.ID))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errors.New("暂存归档不存在：请先 PUT 归档字节再 commit")
		}
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(obj.Body, s.Cfg.MaxUploadBytes()+1))
	obj.Body.Close()
	if err != nil {
		return nil, err
	}
	// 暂存即清理：无论成败都清，重试由 agent 重新 PUT（不留孤儿对象）
	_, _ = s.Storage.DeletePrefix(ctx, uploadStagePrefix+t.ID+"/")
	if int64(len(body)) > s.Cfg.MaxUploadBytes() {
		s.uploads.drop(t.ID)
		return nil, errors.New("归档超过上限")
	}
	form := *t.Form
	form.Body = body
	out, err := s.publishArchive(ctx, t.SiteID, &form)
	if err != nil {
		return nil, err
	}
	s.uploads.drop(t.ID)
	return out, nil
}

// expireUploads 清扫过期票据的暂存对象（sweeper 顺带调用，不加新常驻任务）。
func (s *Server) expireUploads(ctx context.Context) {
	for _, t := range s.uploads.expired() {
		if n, err := s.Storage.DeletePrefix(ctx, uploadStagePrefix+t.ID+"/"); err == nil && n > 0 {
			s.Logger.Info("上传票据过期清理", "upload", t.ID, "objects", n)
		}
	}
}
