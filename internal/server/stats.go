// 访问统计：访客请求路径只加内存计数，后台按分钟批量并入元数据（MergeVisits），
// 绝不在请求路径同步落盘。崩溃最多丢最后一分钟的计数，可接受。
package server

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	"pageshare/internal/store"
)

// visitTracker 是页览（pageview）内存计数器。
type visitTracker struct {
	mu     sync.Mutex
	bySite map[string]*visitEntry
}

type visitEntry struct {
	count int64
	last  time.Time
}

func newVisitTracker() *visitTracker {
	return &visitTracker{bySite: map[string]*visitEntry{}}
}

// add 记录一次页览。
func (t *visitTracker) add(siteID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.bySite[siteID]
	if e == nil {
		e = &visitEntry{}
		t.bySite[siteID] = e
	}
	e.count++
	e.last = time.Now()
}

// pending 返回尚未刷盘的增量与最近访问时间（读取统计时并上，保证实时性）。
func (t *visitTracker) pending(siteID string) (int64, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.bySite[siteID]; ok {
		return e.count, e.last
	}
	return 0, time.Time{}
}

// drain 取走全部累积并清零（刷盘用）。
func (t *visitTracker) drain() (map[string]int64, map[string]time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	deltas := make(map[string]int64, len(t.bySite))
	lasts := make(map[string]time.Time, len(t.bySite))
	for id, e := range t.bySite {
		if e.count > 0 {
			deltas[id] = e.count
			lasts[id] = e.last
		}
	}
	t.bySite = map[string]*visitEntry{}
	return deltas, lasts
}

// isPageView 判定一次请求是否算"页览"：入口、目录、*.html，以及 SPA 站点的
// 无扩展名深链（回退渲染的也是 HTML）。静态资源（js/css/图片）不计。
func isPageView(rel string, spa bool) bool {
	if rel == "" || strings.HasSuffix(rel, "/") {
		return true
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".html", ".htm":
		return true
	case "":
		return spa
	}
	return false
}

// StartStatsFlusher 启动后台刷盘循环，返回停止函数（语义同 StartSweeper）。
func (s *Server) StartStatsFlusher(ctx context.Context, interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.flushVisits() // 退出前把最后的增量落盘
				return
			case <-t.C:
				s.flushVisits()
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			<-done
		}
	}
}

func (s *Server) flushVisits() {
	deltas, lasts := s.visits.drain()
	if len(deltas) == 0 {
		return
	}
	if err := s.Store.MergeVisits(deltas, lasts); err != nil {
		s.Logger.Error("访问统计刷盘失败", "err", err)
	}
}

// statEntry 组装单站统计（store 值 + 未刷盘增量），供管理输出与 get_site_stats。
func (s *Server) statEntry(st *store.Site) map[string]any {
	j := s.toSiteJSON(st)
	e := map[string]any{
		"id":         j.ID,
		"name":       j.Name,
		"url":        j.URL,
		"version":    j.Version,
		"live":       st.ExpiresAt.IsZero() || time.Now().Before(st.ExpiresAt),
		"visits":     j.Visits,
		"updated_at": j.UpdatedAt,
	}
	if j.LastVisitedAt != "" {
		e["last_visit_at"] = j.LastVisitedAt
	}
	if j.ExpiresAt != nil {
		e["expires_at"] = j.ExpiresAt
	}
	return e
}

// liveVisits 返回某站点的实时页览数（store 持久值 + 内存未刷增量）。
func (s *Server) liveVisits(st *store.Site) int64 {
	p, _ := s.visits.pending(st.ID)
	return st.Visits + p
}
