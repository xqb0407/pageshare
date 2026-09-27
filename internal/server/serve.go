package server

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pageshare/internal/site"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// handleServeSite 是公开分享主路径：/s/{id}/{path...}。
func (s *Server) handleServeSite(w http.ResponseWriter, r *http.Request) {
	id := strings.ToUpper(r.PathValue("id"))
	rel := r.PathValue("path")

	st, err := s.Store.GetSite(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.publicPage(w, http.StatusNotFound, "站点不存在或已被吊销")
			return
		}
		s.publicPage(w, http.StatusInternalServerError, "服务错误")
		return
	}
	if !st.ExpiresAt.IsZero() && time.Now().After(st.ExpiresAt) {
		s.publicPage(w, http.StatusGone, "分享链接已过期")
		return
	}
	if st.PasswordHash != "" && !s.gatePass(r, st.ID) {
		s.serveGate(w, r, st, rel)
		return
	}
	// 页览埋点：只加内存计数（stats.go），静态资源与 gate 页不计
	if isPageView(rel, st.SPAFallback) {
		s.visits.add(st.ID)
	}
	// 无密码站点 302 到 presigned URL；有密码站点内容都经会话门，必须代理回源。
	s.serveObject(w, r, st, rel, st.PasswordHash == "")
}

// serveObject 取对象并回给访客。
// presign=true（无密码站点）：302 到限时签名 URL；false（密码站点）：代理回源。
// 未命中时按站点形态回退：有 404.html 输出 404 页；SPA 站点回退入口页（200）。
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, st *store.Site, rel string, presign bool) {
	if rel == "" || strings.HasSuffix(rel, "/") {
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			rel = st.Entry
		} else {
			rel = rel + "/" + st.Entry
		}
	}
	clean, err := site.SanitizeRelPath(rel)
	if err != nil {
		s.publicPage(w, http.StatusBadRequest, "非法路径")
		return
	}
	key := s.sitePrefix(st.ID, st.CurrentVersion) + clean

	if presign {
		loc, err := s.Storage.PresignGet(r.Context(), key, PresignTTL)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				s.serveFallback(w, r, st, presign, key)
				return
			}
			s.publicPage(w, http.StatusInternalServerError, "生成分享链接失败")
			return
		}
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}

	obj, err := s.Storage.Get(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.serveFallback(w, r, st, presign, key)
			return
		}
		s.publicPage(w, http.StatusInternalServerError, "读取内容失败")
		return
	}
	defer obj.Body.Close()
	ct := obj.ContentType
	if ct == "" {
		ct = site.ContentTypeFor(clean)
	}
	w.Header().Set("Content-Type", ct)
	// 密码站点的内容都经会话门，禁止中间缓存
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", itoa(obj.Size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, obj.Body)
}

// serveFallback 处理未命中：SPA 站点回退入口页（200）；普通站点有 404.html 则输出（404）；
// 都没有则普通 404 页。只有 HTML 形态的请求才回退，静态资源（js/css/图片）未命中就是 404。
func (s *Server) serveFallback(w http.ResponseWriter, r *http.Request, st *store.Site, presign bool, missedKey string) {
	ext := path.Ext(missedKey)
	if ext != "" && ext != ".html" {
		s.publicPage(w, http.StatusNotFound, "页面不存在")
		return
	}
	prefix := s.sitePrefix(st.ID, st.CurrentVersion)
	cacheCtl := "no-cache"
	if !presign {
		cacheCtl = "no-store"
	}
	var candidates []struct {
		rel    string
		status int
	}
	if st.SPAFallback {
		candidates = []struct {
			rel    string
			status int
		}{
			{st.Entry, http.StatusOK},
			{"404.html", http.StatusNotFound},
		}
	} else {
		candidates = []struct {
			rel    string
			status int
		}{
			{"404.html", http.StatusNotFound},
		}
	}
	for _, cand := range candidates {
		obj, err := s.Storage.Get(r.Context(), prefix+cand.rel)
		if err != nil {
			continue
		}
		defer obj.Body.Close()
		ct := obj.ContentType
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", cacheCtl)
		w.Header().Set("Content-Length", itoa(obj.Size))
		w.WriteHeader(cand.status)
		_, _ = io.Copy(w, obj.Body)
		return
	}
	s.publicPage(w, http.StatusNotFound, "页面不存在")
}

// onWildcardHost 判断请求是否来自 {id}.<site_wildcard_host> 整域托管。
func (s *Server) onWildcardHost(r *http.Request) bool {
	if s.Cfg.SiteWildcardHost == "" {
		return false
	}
	_, ok := wildcardSiteID(r.Host, s.Cfg.SiteWildcardHost)
	return ok
}

// ---------- 密码门 ----------

func (s *Server) gatePass(r *http.Request, siteID string) bool {
	c, err := r.Cookie(gateCookieName(siteID))
	if err != nil || c.Value == "" {
		return false
	}
	return s.gateTokenValid(siteID, c.Value)
}

func (s *Server) serveGate(w http.ResponseWriter, r *http.Request, st *store.Site, rel string) {
	action := "/s/" + strings.ToLower(st.ID) + "/gate"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(gateHTML(&gateSite{ID: st.ID, Name: st.Name}, rel, action)))
}

func (s *Server) handleGatePage(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSite(strings.ToUpper(r.PathValue("id")))
	if err != nil {
		s.publicPage(w, http.StatusNotFound, "站点不存在或已被吊销")
		return
	}
	s.serveGate(w, r, st, r.URL.Query().Get("next"))
}

func (s *Server) handleGatePost(w http.ResponseWriter, r *http.Request) {
	id := strings.ToUpper(r.PathValue("id"))
	st, err := s.Store.GetSite(id)
	if err != nil {
		s.publicPage(w, http.StatusNotFound, "站点不存在或已被吊销")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.publicPage(w, http.StatusBadRequest, "表单解析失败")
		return
	}
	pw := r.PostFormValue("password")
	next := sanitizeNext(r.PostFormValue("next"))
	action := "/s/" + strings.ToLower(id) + "/gate"
	if pw == "" || bcryptMismatch(st.PasswordHash, pw) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(gateHTMLError(&gateSite{ID: st.ID, Name: st.Name}, next, action)))
		return
	}
	cookiePath := "/s/" + strings.ToLower(id) + "/"
	loc := "/s/" + strings.ToLower(id) + "/"
	if next != "" {
		loc += next
	}
	if s.onWildcardHost(r) {
		// 整域托管：会话 cookie 覆盖整个子域，回跳保持在根路径上
		cookiePath = "/"
		loc = "/" + next
	}
	http.SetCookie(w, &http.Cookie{
		Name:     gateCookieName(id),
		Value:    s.gateToken(id, time.Now().Add(24*time.Hour)),
		Path:     cookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// sanitizeNext 防开放跳转：只允许本站点内的相对路径。
func sanitizeNext(next string) string {
	next = strings.TrimPrefix(next, "/")
	if next == "" || strings.Contains(next, "..") || strings.Contains(next, "://") {
		return ""
	}
	return path.Clean(next)
}

func bcryptMismatch(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil
}
