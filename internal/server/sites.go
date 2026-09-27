package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode"

	"pageshare/internal/site"
	"pageshare/internal/store"
)

// getStr 取 multipart 文本字段的第一个值。
func getStr(frm *multipart.Form, key string) string {
	if vs, ok := frm.Value[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// siteJSON 是站点对象的对外形状。
type siteJSON struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	Version       int64   `json:"version"`
	HasPassword   bool    `json:"has_password"`
	SPAFallback   bool    `json:"spa"`
	Entry         string  `json:"entry"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	ExpiresAt     *string `json:"expires_at,omitempty"`
	Visits        int64   `json:"visits"`
	LastVisitedAt string  `json:"last_visit_at,omitempty"`
}

func (s *Server) toSiteJSON(st *store.Site) siteJSON {
	j := siteJSON{
		ID:          st.ID,
		Name:        st.Name,
		URL:         s.Cfg.SiteURL(st.ID),
		Version:     st.CurrentVersion,
		HasPassword: st.PasswordHash != "",
		SPAFallback: st.SPAFallback,
		Entry:       st.Entry,
		CreatedAt:   st.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   st.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !st.ExpiresAt.IsZero() {
		v := st.ExpiresAt.UTC().Format(time.RFC3339)
		j.ExpiresAt = &v
	}
	// 页览 = 持久值 + 内存未刷增量，刚发生的访问立即可见
	j.Visits = st.Visits
	last := st.LastVisitedAt
	if s.visits != nil {
		p, plast := s.visits.pending(st.ID)
		j.Visits += p
		if plast.After(last) {
			last = plast
		}
	}
	if !last.IsZero() {
		j.LastVisitedAt = last.UTC().Format(time.RFC3339)
	}
	return j
}

// PublishOpts 是一次发布（新建或覆盖）的公共参数，HTTP 表单与 MCP 工具共用。
type PublishOpts struct {
	Name     string
	TTL      *time.Duration // nil = 不改动
	Password *string        // nil = 不改动；非 nil 空串 = 清除密码
	SPA      *bool          // nil = 不改动；非 nil = 设置/清除 SPA 回退
	Body     []byte         // zip 或 tar.gz 归档
}

// publishForm 是 HTTP 侧的表单载体。
type publishForm = PublishOpts

// readPublishForm 支持两种契约：
//  1. multipart/form-data：归档放 file 字段，其余为文本字段；
//  2. 原始请求体：归档直接放 body，字段取 URL query（方便 curl --data-binary）。
func (s *Server) readPublishForm(r *http.Request) (*publishForm, error) {
	form := &publishForm{}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		mediaType, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return nil, fmt.Errorf("Content-Type 无法解析: %w", err)
		}
		_ = mediaType
		mr := multipart.NewReader(io.LimitReader(r.Body, s.Cfg.MaxUploadBytes()+1), params["boundary"])
		frm, err := mr.ReadForm(8 << 20)
		if err != nil {
			return nil, fmt.Errorf("解析 multipart: %w", err)
		}
		defer frm.RemoveAll()
		getField := func(key string) (string, bool) {
			if vs, ok := frm.Value[key]; ok && len(vs) > 0 {
				return vs[0], true
			}
			return "", false
		}
		form.Name = strings.TrimSpace(getStr(frm, "name"))
		if v, ok := getField("ttl"); ok && v != "" {
			d, err := parseTTLField(v)
			if err != nil {
				return nil, err
			}
			form.TTL = &d
		}
		if pv, ok := getField("password"); ok {
			form.Password = &pv
		}
		if sv, ok := getField("spa"); ok {
			form.SPA = boolPtr(parseOnOff(sv))
		}
		fhs, ok := frm.File["file"]
		if !ok || len(fhs) == 0 {
			return nil, fmt.Errorf("缺少 file 字段（归档）")
		}
		file, err := fhs[0].Open()
		if err != nil {
			return nil, fmt.Errorf("打开 file 字段: %w", err)
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, s.Cfg.MaxUploadBytes()+1))
		if err != nil {
			return nil, fmt.Errorf("读取上传体: %w", err)
		}
		form.Body = body
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, s.Cfg.MaxUploadBytes()+1))
		if err != nil {
			return nil, fmt.Errorf("读取上传体: %w", err)
		}
		form.Body = body
		form.Name = strings.TrimSpace(r.URL.Query().Get("name"))
		if v := r.URL.Query().Get("ttl"); v != "" {
			d, err := parseTTLField(v)
			if err != nil {
				return nil, err
			}
			form.TTL = &d
		}
		if q := r.URL.Query(); q.Has("password") {
			pv := q.Get("password")
			form.Password = &pv
		}
		if q := r.URL.Query(); q.Has("spa") {
			form.SPA = boolPtr(parseOnOff(q.Get("spa")))
		}
	}
	if int64(len(form.Body)) > s.Cfg.MaxUploadBytes() {
		return nil, fmt.Errorf("上传体超过上限 %d MB", s.Cfg.MaxUploadMB)
	}
	return form, nil
}

func parseTTLField(v string) (time.Duration, error) {
	d, err := site.ParseTTL(v)
	if err != nil {
		return 0, fmt.Errorf("ttl: %w", err)
	}
	if d > site.MaxTTL {
		return 0, fmt.Errorf("ttl 最长 %s", site.MaxTTL)
	}
	return d, nil
}

// publish 把归档解包写入存储并落一条新版本（HTTP 与 MCP 共用）。
func (s *Server) publish(ctx context.Context, st *store.Site, form *PublishOpts, newVersion int64) (*store.Version, error) {
	prefix := s.sitePrefix(st.ID, newVersion)
	limits := site.Limits{
		MaxCompressed: s.Cfg.MaxUploadBytes(),
		MaxFiles:      2000,
		MaxTotalBytes: 500 << 20,
	}
	sum, err := site.Extract(form.Body, limits, func(rel string, body []byte, ct string) error {
		return s.Storage.Put(ctx, prefix+rel, bytes.NewReader(body), int64(len(body)), ct)
	})
	if err != nil {
		// 失败即清掉半成品对象，不留孤儿
		_, _ = s.Storage.DeletePrefix(ctx, prefix)
		return nil, err
	}
	entry := sum.Entry
	if entry == "" {
		entry = st.Entry
	}
	v := &store.Version{SiteID: st.ID, Version: newVersion, FileCount: sum.Files, TotalBytes: sum.TotalBytes}
	var (
		pw  *string
		ttl *time.Duration
		spa *bool
	)
	if form != nil {
		if form.Password != nil {
			if *form.Password == "" {
				pw = strPtr("")
			} else {
				h, err := hashPassword(*form.Password)
				if err != nil {
					return nil, err
				}
				pw = strPtr(h)
			}
		}
		ttl = form.TTL
		spa = form.SPA
	}
	if err := s.Store.PublishVersion(v, entry, pw, ttl, spa); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	form, err := s.readPublishForm(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(form.Body) == 0 {
		httpError(w, http.StatusBadRequest, "上传体为空")
		return
	}
	// 建站走全局锁：id 是查后建（TOCTOU），并发建站会撞 id，串行化消除
	s.newSiteMu.Lock()
	defer s.newSiteMu.Unlock()
	id, err := s.newSiteID()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := &store.Site{ID: id, Name: sanitizeName(form.Name), Entry: "index.html"}
	if err := s.Store.CreateSite(st); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	v, err := s.publish(r.Context(), st, form, 1)
	if err != nil {
		// 建站失败即回滚站点行，避免空壳
		_, _ = s.Store.DeleteSite(id)
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.Store.GetSite(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = v
	writeJSON(w, http.StatusCreated, s.toSiteJSON(updated))
}

// pathSiteID 规范化 URL 里的站点 id（大小写不敏感、易混字符纠正）。
func pathSiteID(r *http.Request) (string, error) {
	return site.NormalizeID(r.PathValue("id"))
}

func (s *Server) handlePublishToSite(w http.ResponseWriter, r *http.Request) {
	id, err := pathSiteID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 同站覆盖发布上互斥锁：版本号是读后写（oldVersion+1），多 agent 并发
	// 覆盖同一站点会算出相同新版本，存储前缀互相覆盖；不同站点互不阻塞。
	mu := s.lockSite(id)
	mu.Lock()
	defer mu.Unlock()
	st, err := s.Store.GetSite(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "站点不存在")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	form, err := s.readPublishForm(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(form.Body) == 0 {
		httpError(w, http.StatusBadRequest, "上传体为空")
		return
	}
	newVersion := s.nextPublishVersion(st)
	v, err := s.publish(r.Context(), st, form, newVersion)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 新版本已生效，按 versions_kept 保留窗口清理超窗版本对象
	s.pruneVersions(r.Context(), st.ID, newVersion)
	updated, _ := s.Store.GetSite(id)
	_ = v
	writeJSON(w, http.StatusOK, s.toSiteJSON(updated))
}

func (s *Server) handleListSites(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.Store.ListSites()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]siteJSON, 0, len(sites))
	for _, st := range sites {
		out = append(out, s.toSiteJSON(st))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	id, err := pathSiteID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	st, err := s.Store.GetSite(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "站点不存在")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.toSiteJSON(st))
}

// siteMetaUpdate 是一次纯元数据修改（PATCH / MCP update_site 共用）；
// 指针字段 nil = 不改动。Password 空串 = 清除；TTL 0 = 清为永久（重新起算）。
type siteMetaUpdate struct {
	Name     *string
	Password *string
	TTL      *time.Duration
	SPAFalls *bool
}

// updateSiteMeta 不重传、不涨版本地修改站点设置；与发布/删除共用站点锁。
func (s *Server) updateSiteMeta(id string, u *siteMetaUpdate) (siteJSON, error) {
	mu := s.lockSite(id)
	mu.Lock()
	defer mu.Unlock()
	var (
		pwHash *string
		name   *string
	)
	if u.Password != nil {
		if *u.Password == "" {
			pwHash = strPtr("")
		} else {
			h, err := hashPassword(*u.Password)
			if err != nil {
				return siteJSON{}, err
			}
			pwHash = &h
		}
	}
	if u.Name != nil {
		n := sanitizeName(*u.Name)
		name = &n
	}
	st, err := s.Store.UpdateSiteMeta(id, name, pwHash, u.TTL, u.SPAFalls)
	if err != nil {
		return siteJSON{}, err
	}
	return s.toSiteJSON(st), nil
}

// handlePatchSite 是 PATCH /api/v1/sites/{id}：JSON 体，只改出现的字段。
func (s *Server) handlePatchSite(w http.ResponseWriter, r *http.Request) {
	id, err := pathSiteID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Name     *string `json:"name"`
		TTL      *string `json:"ttl"`
		Password *string `json:"password"`
		SP       *bool   `json:"spa"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "请求体需为 JSON: "+err.Error())
		return
	}
	u := &siteMetaUpdate{Name: body.Name, Password: body.Password, SPAFalls: body.SP}
	if body.TTL != nil {
		d, err := parseTTLField(*body.TTL)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		u.TTL = &d
	}
	if u.Name == nil && u.Password == nil && u.TTL == nil && u.SPAFalls == nil {
		httpError(w, http.StatusBadRequest, "name/ttl/password/spa 至少提供一个")
		return
	}
	j, err := s.updateSiteMeta(id, u)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "站点不存在")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	id, err := pathSiteID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 与覆盖发布共用站点锁，避免「删除 vs 发布」赛跑留下孤儿对象
	mu := s.lockSite(id)
	mu.Lock()
	defer mu.Unlock()
	st, err := s.Store.DeleteSite(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "站点不存在")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 对象存储里该站点所有版本一并清理
	_, _ = s.Storage.DeletePrefix(r.Context(), st.ID+"/")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// newSiteID 生成不冲突的站点 id。
func (s *Server) newSiteID() (string, error) {
	for range 10 {
		id, err := site.NewID()
		if err != nil {
			return "", err
		}
		if _, err := s.Store.GetSite(id); errors.Is(err, store.ErrNotFound) {
			return id, nil
		}
	}
	return "", fmt.Errorf("生成站点 id 多次冲突，请重试")
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Join(strings.Fields(name), " ")
	r := []rune(name)
	if len(r) > 80 {
		r = r[:80]
	}
	return strings.TrimFunc(string(r), func(r rune) bool { return !unicode.IsGraphic(r) })
}

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

// parseOnOff 解析开关类表单值；无法识别按 false。
func parseOnOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
