// Package store 是站点元数据的持久层。
// 默认用单个 JSON 文件（人类可读、原子写、零依赖）；传 .db 后缀则走 SQLite（兼容旧部署）。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound 表示站点不存在。
var ErrNotFound = errors.New("site not found")

// Site 是站点元数据。ExpiresAt 零值表示永久；PasswordHash 为空表示无密码；
// SPAFallback 为真时未命中路径回退入口页（客户端路由站点用）。
// Visits/LastVisitedAt 为页览统计，由内存计数器分钟级并入（允许滞后）。
type Site struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Entry          string `json:"entry"`
	PasswordHash   string `json:"password_hash,omitempty"`
	SPAFallback    bool   `json:"spa"`
	CurrentVersion int64  `json:"current_version"`
	// PrunedTo 是"版本对象已物理删除"的水位线：v <= PrunedTo 的对象已不在存储里，
	// 回滚仅允许指向 (PrunedTo, 最新] 的版本。由 versions_kept 保留窗口推进。
	PrunedTo      int64     `json:"pruned_to,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	Visits        int64     `json:"visits,omitempty"`
	LastVisitedAt time.Time `json:"last_visited_at,omitempty"`
}

// Version 是一次发布记录。
type Version struct {
	SiteID     string    `json:"site_id"`
	Version    int64     `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	FileCount  int       `json:"file_count"`
	TotalBytes int64     `json:"total_bytes"`
}

// MCPKey 是 MCP 接入密钥池里的一把密钥。KeyHash 存 SHA-256（明文只在签发时返回一次），
// Prefix 是明文前几位（供控制台辨认），LastUsedAt 由 /mcp 鉴权中间件按需回写。
type MCPKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	KeyHash    string    `json:"key_hash"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// Scopes 是授权面（publish/read/delete）。nil/空 = 全部：签发端默认写入
	// publish,read，而升级前的存量密钥保持原行为（全权限），不被静默断权。
	Scopes []string `json:"scopes,omitempty"`
}

// Store 是元数据存储的对外句柄；方法集与实现无关（JSON / SQLite）。
type Store struct {
	impl backend
}

// backend 是两种实现的公共方法集。
type backend interface {
	CreateSite(site *Site) error
	GetSite(id string) (*Site, error)
	ListSites() ([]*Site, error)
	PublishVersion(v *Version, entry string, passwordHash *string, ttl *time.Duration, spa *bool) error
	// UpdateSiteMeta 不重传地修改元数据；nil 字段保持，ttl==0 清为永久。返回更新后的站点。
	UpdateSiteMeta(id string, name *string, passwordHash *string, ttl *time.Duration, spa *bool) (*Site, error)
	// MergeVisits 批量并入页览增量与最近访问时间（内存计数器分钟级刷盘）。
	MergeVisits(deltas map[string]int64, lastVisited map[string]time.Time) error
	// ListVersions 返回某站点全部发布记录，按版本升序（行永不删除，物理删除由 PrunedTo 水位表达）。
	ListVersions(siteID string) ([]*Version, error)
	// SetCurrentVersion 原子切版本指针（回滚用）；不校验窗口（server 层持锁校验）。返回更新后的站点。
	SetCurrentVersion(id string, version int64) (*Site, error)
	// MarkPrunedTo 把删除水位推进到 to（仅前进）；返回是否更新。
	MarkPrunedTo(id string, to int64) error
	DeleteSite(id string) (*Site, error)
	ListExpired(now time.Time) ([]*Site, error)

	// MCP 密钥池
	CreateMCPKey(k *MCPKey) error
	ListMCPKeys() ([]*MCPKey, error)
	FindMCPKey(keyHash string) (*MCPKey, error)
	TouchMCPKey(id string, now time.Time) error
	DeleteMCPKey(id string) error

	Close() error
}

// Open 按扩展名选择实现：.json → JSON 文件（默认推荐）；.db → SQLite（兼容旧部署）。
func Open(path string) (*Store, error) {
	var (
		b   backend
		err error
	)
	if strings.EqualFold(filepath.Ext(path), ".json") {
		b, err = openJSONFile(path)
	} else {
		b, err = openSQLite(path)
	}
	if err != nil {
		return nil, err
	}
	return &Store{impl: b}, nil
}

func (s *Store) Close() error { return s.impl.Close() }

func (s *Store) CreateSite(site *Site) error { return s.impl.CreateSite(site) }
func (s *Store) GetSite(id string) (*Site, error) {
	return s.impl.GetSite(id)
}
func (s *Store) ListSites() ([]*Site, error) { return s.impl.ListSites() }
func (s *Store) UpdateSiteMeta(id string, name *string, passwordHash *string, ttl *time.Duration, spa *bool) (*Site, error) {
	return s.impl.UpdateSiteMeta(id, name, passwordHash, ttl, spa)
}
func (s *Store) MergeVisits(deltas map[string]int64, lastVisited map[string]time.Time) error {
	return s.impl.MergeVisits(deltas, lastVisited)
}
func (s *Store) ListVersions(siteID string) ([]*Version, error) { return s.impl.ListVersions(siteID) }
func (s *Store) SetCurrentVersion(id string, version int64) (*Site, error) {
	return s.impl.SetCurrentVersion(id, version)
}
func (s *Store) MarkPrunedTo(id string, to int64) error { return s.impl.MarkPrunedTo(id, to) }
func (s *Store) PublishVersion(v *Version, entry string, passwordHash *string, ttl *time.Duration, spa *bool) error {
	return s.impl.PublishVersion(v, entry, passwordHash, ttl, spa)
}
func (s *Store) DeleteSite(id string) (*Site, error) { return s.impl.DeleteSite(id) }
func (s *Store) ListExpired(now time.Time) ([]*Site, error) {
	return s.impl.ListExpired(now)
}

// ---------- MCP 密钥池 ----------

func (s *Store) CreateMCPKey(k *MCPKey) error { return s.impl.CreateMCPKey(k) }
func (s *Store) ListMCPKeys() ([]*MCPKey, error) {
	return s.impl.ListMCPKeys()
}
func (s *Store) FindMCPKey(keyHash string) (*MCPKey, error) {
	return s.impl.FindMCPKey(keyHash)
}
func (s *Store) TouchMCPKey(id string, now time.Time) error {
	return s.impl.TouchMCPKey(id, now)
}
func (s *Store) DeleteMCPKey(id string) error { return s.impl.DeleteMCPKey(id) }

// ---------- JSON 文件实现 ----------

type fileData struct {
	Sites    []*Site    `json:"sites"`
	Versions []*Version `json:"versions"`
	MCPKeys  []*MCPKey  `json:"mcp_keys,omitempty"`
}

type jsonStore struct {
	mu   sync.Mutex
	path string
	data fileData
}

// openJSONFile 打开（必要时创建）JSON 元数据文件。
func openJSONFile(path string) (*jsonStore, error) {
	js := &jsonStore{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &js.data); err != nil {
			return nil, fmt.Errorf("解析元数据文件 %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次启动：空登记簿，落一个可读的空文件
		if err := js.save(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("读取元数据文件 %s: %w", path, err)
	}
	return js, nil
}

// save 原子写：临时文件 + rename，避免半截 JSON。
func (js *jsonStore) save() error {
	raw, err := json.MarshalIndent(js.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := js.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, js.path)
}

func (js *jsonStore) Close() error { return nil }

func (js *jsonStore) CreateSite(site *Site) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, s := range js.data.Sites {
		if strings.EqualFold(s.ID, site.ID) {
			return fmt.Errorf("站点 id %s 已存在", site.ID)
		}
	}
	if site.CreatedAt.IsZero() {
		site.CreatedAt = time.Now()
	}
	if site.UpdatedAt.IsZero() {
		site.UpdatedAt = site.CreatedAt
	}
	js.data.Sites = append(js.data.Sites, site)
	return js.save()
}

func (js *jsonStore) GetSite(id string) (*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.getLocked(id)
}

func (js *jsonStore) getLocked(id string) (*Site, error) {
	for _, s := range js.data.Sites {
		if strings.EqualFold(s.ID, id) {
			return s, nil
		}
	}
	return nil, ErrNotFound
}

func (js *jsonStore) ListSites() ([]*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]*Site, len(js.data.Sites))
	copy(out, js.data.Sites)
	// 更新时间倒序，和管理 API 的展示顺序一致
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].UpdatedAt.After(out[i].UpdatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// PublishVersion 记录新版本并推进站点当前版本；pw/ttl/spa 的 nil 表示"保持不变"。
func (js *jsonStore) PublishVersion(v *Version, entry string, passwordHash *string, ttl *time.Duration, spa *bool) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	st, err := js.getLocked(v.SiteID)
	if err != nil {
		return err
	}
	now := time.Now()
	v.CreatedAt = now
	if entry != "" {
		st.Entry = entry
	}
	if passwordHash != nil {
		st.PasswordHash = *passwordHash
	}
	if ttl != nil {
		if *ttl > 0 {
			st.ExpiresAt = now.Add(*ttl)
		} else {
			st.ExpiresAt = time.Time{}
		}
	}
	if spa != nil {
		st.SPAFallback = *spa
	}
	st.CurrentVersion = max(st.CurrentVersion, v.Version)
	st.UpdatedAt = now
	js.data.Versions = append(js.data.Versions, v)
	return js.save()
}

// UpdateSiteMeta 改元数据不涨版本；nil 字段保持，ttl==0 清为永久。
func (js *jsonStore) UpdateSiteMeta(id string, name *string, passwordHash *string, ttl *time.Duration, spa *bool) (*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	st, err := js.getLocked(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if name != nil {
		st.Name = *name
	}
	if passwordHash != nil {
		st.PasswordHash = *passwordHash
	}
	if ttl != nil {
		if *ttl > 0 {
			st.ExpiresAt = now.Add(*ttl)
		} else {
			st.ExpiresAt = time.Time{}
		}
	}
	if spa != nil {
		st.SPAFallback = *spa
	}
	st.UpdatedAt = now
	if err := js.save(); err != nil {
		return nil, err
	}
	return st, nil
}

// MergeVisits 把内存计数器的增量并进站点记录；查不到的 id 静默跳过（可能已删）。
func (js *jsonStore) MergeVisits(deltas map[string]int64, lastVisited map[string]time.Time) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	changed := false
	for id, d := range deltas {
		if d <= 0 {
			continue
		}
		for _, st := range js.data.Sites {
			if !strings.EqualFold(st.ID, id) {
				continue
			}
			st.Visits += d
			if t, ok := lastVisited[id]; ok && t.After(st.LastVisitedAt) {
				st.LastVisitedAt = t
			}
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	return js.save()
}

func (js *jsonStore) ListVersions(siteID string) ([]*Version, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	var out []*Version
	for _, v := range js.data.Versions {
		if strings.EqualFold(v.SiteID, siteID) {
			cp := *v
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (js *jsonStore) SetCurrentVersion(id string, version int64) (*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	st, err := js.getLocked(id)
	if err != nil {
		return nil, err
	}
	st.CurrentVersion = version
	st.UpdatedAt = time.Now()
	if err := js.save(); err != nil {
		return nil, err
	}
	return js.getLocked(id)
}

func (js *jsonStore) MarkPrunedTo(id string, to int64) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	st, err := js.getLocked(id)
	if err != nil {
		return err
	}
	if to <= st.PrunedTo {
		return nil
	}
	st.PrunedTo = to
	return js.save()
}

func (js *jsonStore) DeleteSite(id string) (*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for i, s := range js.data.Sites {
		if strings.EqualFold(s.ID, id) {
			js.data.Sites = append(js.data.Sites[:i], js.data.Sites[i+1:]...)
			kept := js.data.Versions[:0]
			for _, v := range js.data.Versions {
				if !strings.EqualFold(v.SiteID, s.ID) {
					kept = append(kept, v)
				}
			}
			js.data.Versions = kept
			if err := js.save(); err != nil {
				return nil, err
			}
			return s, nil
		}
	}
	return nil, ErrNotFound
}

func (js *jsonStore) ListExpired(now time.Time) ([]*Site, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	var out []*Site
	for _, s := range js.data.Sites {
		if !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt) {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---------- MCP 密钥池（JSON 实现） ----------

func (js *jsonStore) CreateMCPKey(k *MCPKey) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	if k.ID == "" || k.KeyHash == "" {
		return fmt.Errorf("密钥缺少 id 或 key_hash")
	}
	for _, exist := range js.data.MCPKeys {
		if exist.ID == k.ID {
			return fmt.Errorf("密钥 id %s 已存在", k.ID)
		}
		if exist.KeyHash == k.KeyHash {
			return fmt.Errorf("密钥已存在")
		}
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	js.data.MCPKeys = append(js.data.MCPKeys, k)
	return js.save()
}

func (js *jsonStore) ListMCPKeys() ([]*MCPKey, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]*MCPKey, len(js.data.MCPKeys))
	copy(out, js.data.MCPKeys)
	// 创建时间倒序，控制台最新的在前
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (js *jsonStore) FindMCPKey(keyHash string) (*MCPKey, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, k := range js.data.MCPKeys {
		if k.KeyHash == keyHash {
			return k, nil
		}
	}
	return nil, ErrNotFound
}

func (js *jsonStore) TouchMCPKey(id string, now time.Time) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, k := range js.data.MCPKeys {
		if k.ID == id {
			k.LastUsedAt = now
			return js.save()
		}
	}
	return ErrNotFound
}

func (js *jsonStore) DeleteMCPKey(id string) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	for i, k := range js.data.MCPKeys {
		if k.ID == id {
			js.data.MCPKeys = append(js.data.MCPKeys[:i], js.data.MCPKeys[i+1:]...)
			return js.save()
		}
	}
	return ErrNotFound
}
