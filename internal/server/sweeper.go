// TTL 清扫：定期把过期站点的元数据与对象一并删除。
package server

import (
	"context"
	"time"
)

// StartSweeper 启动后台清扫循环，返回停止函数。
func (s *Server) StartSweeper(ctx context.Context, interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		// 启动即先扫一次
		s.sweepOnce(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				close(done)
				return
			case <-t.C:
				s.sweepOnce(ctx)
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

func (s *Server) sweepOnce(ctx context.Context) {
	s.expireUploads(ctx) // 过期上传票据的暂存对象顺带清扫（见 uploads.go）
	expired, err := s.Store.ListExpired(time.Now())
	if err != nil {
		s.Logger.Error("TTL 清扫查询失败", "err", err)
		return
	}
	for _, st := range expired {
		if _, err := s.Store.DeleteSite(st.ID); err != nil {
			s.Logger.Error("删除过期站点失败", "site", st.ID, "err", err)
			continue
		}
		n, err := s.Storage.DeletePrefix(ctx, st.ID+"/")
		if err != nil {
			s.Logger.Error("清理过期站点对象失败", "site", st.ID, "err", err)
			continue
		}
		s.Logger.Info("TTL 到期清理", "site", st.ID, "name", st.Name, "objects", n)
	}
}
