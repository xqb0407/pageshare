package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pageshare/internal/mcpserver"
	"pageshare/internal/site"
	"pageshare/internal/store"
)

// UpdateSite 实现 mcpserver.Backend：不重传归档、不涨版本地修改站点设置。
func (s *Server) UpdateSite(ctx context.Context, id string, opts mcpserver.ServerUpdateOpts) (map[string]any, error) {
	norm, err := site.NormalizeID(id)
	if err != nil {
		return nil, err
	}
	j, err := s.updateSiteMeta(norm, &siteMetaUpdate{
		Name: opts.Name, Password: opts.Password, TTL: opts.TTL, SPAFalls: opts.SPA,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("站点 %s 不存在", norm)
		}
		return nil, err
	}
	return map[string]any{
		"id": j.ID, "name": j.Name, "url": j.URL, "version": j.Version,
		"has_password": j.HasPassword, "spa": j.SPAFallback, "expires_at": j.ExpiresAt,
		"visits": j.Visits,
	}, nil
}

// SiteStatsJSON 实现 mcpserver.Backend：单站详情或全部站点汇总（含未刷盘增量）。
func (s *Server) SiteStatsJSON(ctx context.Context, siteID string) (map[string]any, error) {
	var sites []*store.Site
	if siteID != "" {
		norm, err := site.NormalizeID(siteID)
		if err != nil {
			return nil, err
		}
		st, err := s.Store.GetSite(norm)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("站点 %s 不存在", norm)
			}
			return nil, err
		}
		sites = []*store.Site{st}
	} else {
		list, err := s.Store.ListSites()
		if err != nil {
			return nil, err
		}
		sites = list
	}
	entries := make([]map[string]any, 0, len(sites))
	var total int64
	var busiest map[string]any
	for _, st := range sites {
		e := s.statEntry(st)
		v, _ := e["visits"].(int64)
		total += v
		if b, _ := busiest["visits"].(int64); busiest == nil || v > b {
			busiest = map[string]any{"id": e["id"], "name": e["name"], "visits": v}
		}
		entries = append(entries, e)
	}
	out := map[string]any{"sites": entries, "total_sites": len(entries), "total_visits": total}
	if busiest != nil {
		out["busiest"] = busiest
	}
	return out, nil
}

// ListSiteVersions 实现 mcpserver.Backend：发布历史（含 current/restorable 标注）。
func (s *Server) ListSiteVersions(ctx context.Context, siteID string) ([]map[string]any, error) {
	return s.listSiteVersions(siteID)
}

// RollbackSite 实现 mcpserver.Backend：切 current_version 指针回滚。
func (s *Server) RollbackSite(ctx context.Context, siteID string, version int64) (map[string]any, error) {
	return s.rollbackSite(ctx, siteID, version)
}

// PublishSite 实现 mcpserver.Backend：existingID 为空=新建，否则覆盖。
func (s *Server) PublishSite(ctx context.Context, existingID string, opts mcpserver.ServerPublishOpts) (map[string]any, error) {
	form := &PublishOpts{Name: opts.Name, TTL: opts.TTL, Password: opts.Password, SPA: opts.SPAFalls, Body: opts.Body}
	if len(form.Body) == 0 {
		return nil, fmt.Errorf("归档内容为空")
	}
	return s.publishArchive(ctx, existingID, form)
}

// publishArchive 是发布主流程（publish_site 与 commit_upload 共用，零复制）：
// existingID 为空走建站全局锁，否则走站点锁覆盖发布。
func (s *Server) publishArchive(ctx context.Context, existingID string, form *PublishOpts) (map[string]any, error) {
	if existingID == "" {
		// 建站与 HTTP 面共用全局锁（id 查后建竞态）
		s.newSiteMu.Lock()
		defer s.newSiteMu.Unlock()
		_, out, err := s.publishNewSite(ctx, form)
		return out, err
	}
	// 覆盖发布拿站点锁：版本号读后写，多 agent 并发覆盖同一站点会互踩
	id, err := site.NormalizeID(existingID)
	if err != nil {
		return nil, err
	}
	mu := s.lockSite(id)
	mu.Lock()
	defer mu.Unlock()
	st, err := s.Store.GetSite(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("站点 %s 不存在", id)
		}
		return nil, err
	}
	newVersion := s.nextPublishVersion(st)
	if _, err := s.publish(ctx, st, form, newVersion); err != nil {
		return nil, err
	}
	s.pruneVersions(ctx, st.ID, newVersion)
	updated, err := s.Store.GetSite(st.ID)
	if err != nil {
		return nil, err
	}
	j := s.toSiteJSON(updated)
	return map[string]any{
		"id": j.ID, "name": j.Name, "url": j.URL, "version": j.Version,
		"has_password": j.HasPassword, "spa": j.SPAFallback, "expires_at": j.ExpiresAt,
	}, nil
}

// BeginUpload 实现 mcpserver.Backend：两阶段上传之签票据。
func (s *Server) BeginUpload(ctx context.Context, siteID string, opts mcpserver.ServerUploadOpts, expectedBytes int64) (map[string]any, error) {
	norm := ""
	if siteID != "" {
		var err error
		if norm, err = site.NormalizeID(siteID); err != nil {
			return nil, err
		}
	}
	form := &PublishOpts{Name: opts.Name, TTL: opts.TTL, Password: opts.Password, SPA: opts.SPAFalls}
	t, err := s.beginUpload(norm, form, expectedBytes)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"upload_id":  t.ID,
		"put_url":    s.uploadPutURL(t.ID),
		"expires_in": int64(UploadTicketTTL / time.Second),
		"site_id":    norm, // 空 = commit 时新建
	}, nil
}

// CommitUpload 实现 mcpserver.Backend：两阶段上传之回执发布。
func (s *Server) CommitUpload(ctx context.Context, uploadID string) (map[string]any, error) {
	if uploadID == "" {
		return nil, fmt.Errorf("upload_id 不能为空")
	}
	return s.commitUpload(ctx, uploadID)
}

// publishNewSite 建站 + 首发（调用方需持有 newSiteMu）。
func (s *Server) publishNewSite(ctx context.Context, form *PublishOpts) (*store.Site, map[string]any, error) {
	id, err := s.newSiteID()
	if err != nil {
		return nil, nil, err
	}
	st := &store.Site{ID: id, Name: sanitizeName(form.Name), Entry: "index.html"}
	if err := s.Store.CreateSite(st); err != nil {
		return nil, nil, err
	}
	if _, err := s.publish(ctx, st, form, 1); err != nil {
		_, _ = s.Store.DeleteSite(id)
		return nil, nil, err
	}
	updated, err := s.Store.GetSite(id)
	if err != nil {
		return nil, nil, err
	}
	j := s.toSiteJSON(updated)
	out := map[string]any{
		"id": j.ID, "name": j.Name, "url": j.URL, "version": j.Version,
		"has_password": j.HasPassword, "spa": j.SPAFallback, "expires_at": j.ExpiresAt,
	}
	return st, out, nil
}

// ListSitesJSON 实现 mcpserver.Backend。
func (s *Server) ListSitesJSON(ctx context.Context) ([]map[string]any, error) {
	sites, err := s.Store.ListSites()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(sites))
	for _, st := range sites {
		j := s.toSiteJSON(st)
		out = append(out, map[string]any{
			"id": j.ID, "name": j.Name, "url": j.URL, "version": j.Version,
			"has_password": j.HasPassword, "spa": j.SPAFallback,
			"created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "expires_at": j.ExpiresAt,
		})
	}
	return out, nil
}

// DeleteSiteJSON 实现 mcpserver.Backend。
func (s *Server) DeleteSiteJSON(ctx context.Context, id string) error {
	norm, err := site.NormalizeID(id)
	if err != nil {
		return err
	}
	// 与覆盖发布共用站点锁，避免「删除 vs 发布」赛跑留下孤儿对象
	mu := s.lockSite(norm)
	mu.Lock()
	defer mu.Unlock()
	st, err := s.Store.DeleteSite(norm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("站点 %s 不存在", norm)
		}
		return err
	}
	_, _ = s.Storage.DeletePrefix(ctx, st.ID+"/")
	return nil
}
