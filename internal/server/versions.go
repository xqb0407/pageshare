// 版本保留与回滚（versions_kept）：覆盖发布不再立即删除前一版，而是发布成功后
// 把超出保留窗口的最老版本对象删掉；回滚只切 current_version 指针（访问路径按
// current_version 拼前缀，切指针即完成发布回退，对象无需搬移），与发布共用站点锁。
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pageshare/internal/site"
	"pageshare/internal/store"
)

// nextPublishVersion 计算下一次发布的版本号：回滚会让指针落后，
// 必须取"已知最高版本+1"，避免新发布的前缀与仍保留的版本撞车。
func (s *Server) nextPublishVersion(st *store.Site) int64 {
	vers, err := s.Store.ListVersions(st.ID)
	if err != nil || len(vers) == 0 {
		return st.CurrentVersion + 1
	}
	return max(st.CurrentVersion, vers[len(vers)-1].Version) + 1
}

// pruneVersions 在发布成功后删除超出保留窗口的版本对象。
// versions_kept：0=全保留不删；1=现行行为（除当前外全删）；N=保留最近 N 版（含当前）。
// 删除时机在 publish 成功之后、返回响应之前；失败只记日志（水位不越过失败版本？不会——
// 逐版尽力删后统一推进水位，个别失败留下孤儿对象，由站点删除/TTL 清扫兜底）。
func (s *Server) pruneVersions(ctx context.Context, siteID string, newVersion int64) {
	kept := s.Cfg.VersionsKept
	if kept == 0 {
		return
	}
	st, err := s.Store.GetSite(siteID)
	if err != nil {
		return
	}
	watermark := newVersion - kept
	if watermark <= st.PrunedTo {
		return
	}
	vers, err := s.Store.ListVersions(siteID)
	if err != nil {
		return
	}
	for _, v := range vers {
		if v.Version > watermark || v.Version <= st.PrunedTo {
			continue
		}
		if v.Version == st.CurrentVersion {
			continue // 永不删指针当前指向的版本（回滚后指针可能低于最新版本）
		}
		if _, err := s.Storage.DeletePrefix(ctx, s.sitePrefix(siteID, v.Version)); err != nil {
			s.Logger.Error("清理超窗版本对象失败", "site", siteID, "version", v.Version, "err", err)
		}
	}
	if err := s.Store.MarkPrunedTo(siteID, watermark); err != nil {
		s.Logger.Error("版本删除水位推进失败", "site", siteID, "err", err)
	}
}

// rollbackSite 把站点当前版本原子切回一个仍在保留窗口内的历史版本。
// 校验：版本行存在、未被 prune 删除（version > PrunedTo）、不是当前版本；持站点锁。
func (s *Server) rollbackSite(ctx context.Context, id string, version int64) (map[string]any, error) {
	norm, err := site.NormalizeID(id)
	if err != nil {
		return nil, err
	}
	mu := s.lockSite(norm)
	mu.Lock()
	defer mu.Unlock()

	st, err := s.Store.GetSite(norm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("站点 %s 不存在", norm)
		}
		return nil, err
	}
	if version == st.CurrentVersion {
		return nil, fmt.Errorf("版本 %d 已是当前版本", version)
	}
	vers, err := s.Store.ListVersions(norm)
	if err != nil {
		return nil, err
	}
	found := false
	for _, v := range vers {
		if v.Version == version {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("版本 %d 无发布记录", version)
	}
	if version <= st.PrunedTo {
		return nil, fmt.Errorf("版本 %d 的对象已被保留策略删除（versions_kept=%d），不可回滚", version, s.Cfg.VersionsKept)
	}
	updated, err := s.Store.SetCurrentVersion(norm, version)
	if err != nil {
		return nil, err
	}
	j := s.toSiteJSON(updated)
	s.Logger.Info("站点版本回滚", "site", norm, "from", st.CurrentVersion, "to", version)
	return map[string]any{
		"id": j.ID, "name": j.Name, "url": j.URL, "version": j.Version,
		"has_password": j.HasPassword, "spa": j.SPAFallback, "expires_at": j.ExpiresAt,
	}, nil
}

// listSiteVersions 返回站点全部发布记录（MCP list_site_versions 与管理台可用）。
func (s *Server) listSiteVersions(id string) ([]map[string]any, error) {
	norm, err := site.NormalizeID(id)
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
	vers, err := s.Store.ListVersions(norm)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(vers))
	for _, v := range vers {
		out = append(out, map[string]any{
			"version":     v.Version,
			"created_at":  v.CreatedAt.UTC().Format(time.RFC3339),
			"file_count":  v.FileCount,
			"total_bytes": v.TotalBytes,
			"current":     v.Version == st.CurrentVersion,
			"restorable":  v.Version != st.CurrentVersion && v.Version > st.PrunedTo,
		})
	}
	return out, nil
}
