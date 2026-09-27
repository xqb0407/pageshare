// MCP 操作审计（P1-5）：每次工具调用（门禁放行后，成败都记）落一行 JSONL。
// 存储刻意选"文件不入库"：追加写不碰站点元数据锁、崩溃丢失最多尾部一行、
// 运维直接 tail/grep；滚动上限 10MB×5，审计是保险箱不是数据湖。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	auditFileName   = "audit.jsonl"
	auditMaxBytes   = 10 << 20 // 10MB / 文件
	auditMaxRolling = 4        // 滚动副本 .1（新）.. .4（旧）：加 active 共 5 份 ≈ 50MB 上限
)

type auditEntry struct {
	Time    string `json:"time"`
	KeyID   string `json:"key_id,omitempty"`
	KeyName string `json:"key"`
	Agent   string `json:"agent,omitempty"`
	Tool    string `json:"tool"`
	SiteID  string `json:"site_id,omitempty"`
	Outcome string `json:"outcome"`
}

type auditWriter struct {
	mu  sync.Mutex
	dir string
}

func newAuditWriter(dir string) *auditWriter { return &auditWriter{dir: dir} }

func (w *auditWriter) path() string { return filepath.Join(w.dir, auditFileName) }

// write 追加一行并按需滚动。失败只返回给调用方（RecordAudit 忽略）——
// 审计不可用绝不能反过来卡死工具调用。
func (w *auditWriter) write(e auditEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	cur, _ := f.Seek(0, io.SeekCurrent)
	f.Close()
	if cur > auditMaxBytes {
		return w.rotateLocked()
	}
	return nil
}

// rotateLocked：active→.1，旧的依次后移，最老一份（.auditMaxRolling）腾位丢弃。
func (w *auditWriter) rotateLocked() error {
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path(), auditMaxRolling))
	for i := auditMaxRolling - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path(), i)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, fmt.Sprintf("%s.%d", w.path(), i+1))
		}
	}
	return os.Rename(w.path(), w.path()+".1")
}

// query 从新到旧扫描 active + 滚动文件，过滤后返回最多 limit 条（新→旧）。
// site/key 精确匹配（key 允许 id 或名称），since 是 RFC3339 时间下界。
func (w *auditWriter) query(siteID, key, since string, limit int) ([]auditEntry, error) {
	var after time.Time
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return nil, fmt.Errorf("since 需为 RFC3339 时间: %w", err)
		}
		after = t.UTC()
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	out := make([]auditEntry, 0, limit)
	scan := func(name string) bool { // 返回是否已停（达到 limit）
		f, err := os.Open(name)
		if err != nil {
			return false // 文件不存在是常态（未产生过该轮次的日志）
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var lines []string
		for sc.Scan() {
			if s := strings.TrimSpace(sc.Text()); s != "" {
				lines = append(lines, s)
			}
		}
		// 单文件内也按新→旧读（追加序倒过来）
		for i := len(lines) - 1; i >= 0; i-- {
			var e auditEntry
			if json.Unmarshal([]byte(lines[i]), &e) != nil {
				continue // 坏行跳过（崩溃时可能的半行）
			}
			if siteID != "" && e.SiteID != strings.ToLower(siteID) {
				continue
			}
			if key != "" && e.KeyID != key && e.KeyName != key {
				continue
			}
			if !after.IsZero() {
				ts, err := time.Parse(time.RFC3339Nano, e.Time)
				if err != nil || ts.UTC().Before(after) {
					continue
				}
			}
			out = append(out, e)
			if len(out) >= limit {
				return true
			}
		}
		return false
	}
	if !scan(w.path()) {
		for i := 1; i <= auditMaxRolling; i++ {
			if scan(fmt.Sprintf("%s.%d", w.path(), i)) {
				break
			}
		}
	}
	return out, nil
}

// handleMCPAudit GET /api/v1/mcp/audit?site=&key=&since=&limit=
func (s *Server) handleMCPAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			httpError(w, http.StatusBadRequest, "limit 需为正整数")
			return
		}
		limit = n
	}
	entries, err := s.audit.query(q.Get("site"), q.Get("key"), q.Get("since"), limit)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "count": len(entries)})
}
