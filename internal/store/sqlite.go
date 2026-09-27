// SQLite 实现（兼容旧部署）：传 .db 后缀时启用。新部署推荐 JSON 文件。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteStore struct {
	db *sql.DB
}

func openSQLite(path string) (*sqliteStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite %s: %w", path, err)
	}
	// modernc/sqlite 写并发要串行化
	db.SetMaxOpenConns(1)
	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func (s *sqliteStore) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS sites (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL DEFAULT '',
	entry           TEXT NOT NULL DEFAULT 'index.html',
	password_hash   TEXT NOT NULL DEFAULT '',
	spa             INTEGER NOT NULL DEFAULT 0,
	current_version INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL DEFAULT 0,
	visits          INTEGER NOT NULL DEFAULT 0,
	last_visit_at   INTEGER NOT NULL DEFAULT 0,
	pruned_to       INTEGER NOT NULL DEFAULT 0
);
	CREATE TABLE IF NOT EXISTS versions (
		site_id     TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		version     INTEGER NOT NULL,
		created_at  INTEGER NOT NULL,
		file_count  INTEGER NOT NULL,
		total_bytes INTEGER NOT NULL,
		PRIMARY KEY (site_id, version)
	);
	CREATE TABLE IF NOT EXISTS mcp_keys (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL DEFAULT '',
		prefix       TEXT NOT NULL DEFAULT '',
		key_hash     TEXT NOT NULL UNIQUE,
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		scopes       TEXT NOT NULL DEFAULT ''
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if !s.columnExists("sites", "spa") {
		if _, err := s.db.Exec(`ALTER TABLE sites ADD COLUMN spa INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !s.columnExists("sites", "visits") {
		if _, err := s.db.Exec(`ALTER TABLE sites ADD COLUMN visits INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !s.columnExists("sites", "last_visit_at") {
		if _, err := s.db.Exec(`ALTER TABLE sites ADD COLUMN last_visit_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !s.columnExists("mcp_keys", "scopes") {
		if _, err := s.db.Exec(`ALTER TABLE mcp_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !s.columnExists("sites", "pruned_to") {
		if _, err := s.db.Exec(`ALTER TABLE sites ADD COLUMN pruned_to INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) columnExists(table, column string) bool {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			continue
		}
		if name == column {
			return true
		}
	}
	return false
}

func (s *sqliteStore) CreateSite(site *Site) error {
	if site.CreatedAt.IsZero() {
		site.CreatedAt = time.Now()
	}
	if site.UpdatedAt.IsZero() {
		site.UpdatedAt = site.CreatedAt
	}
	_, err := s.db.Exec(
		`INSERT INTO sites (id, name, entry, password_hash, spa, current_version, created_at, updated_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		site.ID, site.Name, site.Entry, site.PasswordHash, boolInt(site.SPAFallback),
		unix(site.CreatedAt), unix(site.UpdatedAt), unix(site.ExpiresAt))
	if err != nil {
		if isUnique(err) {
			return fmt.Errorf("站点 id %s 已存在: %w", site.ID, err)
		}
		return err
	}
	return nil
}

func (s *sqliteStore) GetSite(id string) (*Site, error) {
	row := s.db.QueryRow(
		`SELECT `+siteCols+` FROM sites WHERE id = ?`, id)
	return s.scanSite(row.Scan)
}

func (s *sqliteStore) ListSites() ([]*Site, error) {
	rows, err := s.db.Query(
		`SELECT ` + siteCols + ` FROM sites ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		site, err := s.scanSite(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PublishVersion(v *Version, entry string, passwordHash *string, ttl *time.Duration, spa *bool) error {
	now := time.Now()
	v.CreatedAt = now
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var nextExp any = nil
	if ttl != nil {
		if *ttl > 0 {
			nextExp = now.Add(*ttl).Unix()
		} else {
			nextExp = int64(0)
		}
	}
	if passwordHash != nil || nextExp != nil || entry != "" || spa != nil {
		set := "updated_at = ?"
		args := []any{now.Unix()}
		if entry != "" {
			set += ", entry = ?"
			args = append(args, entry)
		}
		if passwordHash != nil {
			set += ", password_hash = ?"
			args = append(args, *passwordHash)
		}
		if nextExp != nil {
			set += ", expires_at = ?"
			args = append(args, nextExp)
		}
		if spa != nil {
			set += ", spa = ?"
			args = append(args, boolInt(*spa))
		}
		args = append(args, v.SiteID)
		if _, err := tx.Exec(`UPDATE sites SET `+set+` WHERE id = ?`, args...); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO versions (site_id, version, created_at, file_count, total_bytes) VALUES (?, ?, ?, ?, ?)`,
		v.SiteID, v.Version, v.CreatedAt.Unix(), v.FileCount, v.TotalBytes); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE sites SET current_version = MAX(current_version, ?) WHERE id = ?`,
		v.Version, v.SiteID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateSiteMeta 改元数据不涨版本；nil 字段保持，ttl==0 清为永久。
func (s *sqliteStore) UpdateSiteMeta(id string, name *string, passwordHash *string, ttl *time.Duration, spa *bool) (*Site, error) {
	if _, err := s.GetSite(id); err != nil {
		return nil, err // 统一以 ErrNotFound 报"站点不存在"
	}
	now := time.Now()
	set := "updated_at = ?"
	args := []any{now.Unix()}
	if name != nil {
		set += ", name = ?"
		args = append(args, *name)
	}
	if passwordHash != nil {
		set += ", password_hash = ?"
		args = append(args, *passwordHash)
	}
	if ttl != nil {
		var exp int64
		if *ttl > 0 {
			exp = now.Add(*ttl).Unix()
		}
		set += ", expires_at = ?"
		args = append(args, exp)
	}
	if spa != nil {
		set += ", spa = ?"
		args = append(args, boolInt(*spa))
	}
	args = append(args, id)
	if _, err := s.db.Exec(`UPDATE sites SET `+set+` WHERE id = ?`, args...); err != nil {
		return nil, err
	}
	return s.GetSite(id)
}

// MergeVisits 批量并入页览增量；last_visit_at 只进不退（MAX），查不到的 id 影响 0 行即跳过。
func (s *sqliteStore) MergeVisits(deltas map[string]int64, lastVisited map[string]time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, d := range deltas {
		if d <= 0 {
			continue
		}
		var lt int64
		if t, ok := lastVisited[id]; ok {
			lt = t.Unix()
		}
		if _, err := tx.Exec(
			`UPDATE sites SET visits = visits + ?, last_visit_at = MAX(last_visit_at, ?) WHERE id = ?`,
			d, lt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) DeleteSite(id string) (*Site, error) {
	site, err := s.GetSite(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM versions WHERE site_id = ?`, id); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM sites WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return site, nil
}

func (s *sqliteStore) ListExpired(now time.Time) ([]*Site, error) {
	rows, err := s.db.Query(
		`SELECT `+siteCols+` FROM sites WHERE expires_at > 0 AND expires_at <= ?`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		site, err := s.scanSite(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

// ---------- MCP 密钥池（SQLite 实现，兼容旧部署） ----------

func (s *sqliteStore) CreateMCPKey(k *MCPKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(
		`INSERT INTO mcp_keys (id, name, prefix, key_hash, created_at, last_used_at, scopes) VALUES (?, ?, ?, ?, ?, 0, ?)`,
		k.ID, k.Name, k.Prefix, k.KeyHash, k.CreatedAt.Unix(), joinScopes(k.Scopes))
	if err != nil {
		if isUnique(err) {
			return fmt.Errorf("密钥已存在: %w", err)
		}
		return err
	}
	return nil
}

func (s *sqliteStore) ListMCPKeys() ([]*MCPKey, error) {
	// created_at 是秒级精度，同秒创建的用 rowid（插入序）决胜，保证最新的在前
	rows, err := s.db.Query(`SELECT ` + mcpKeyCols + ` FROM mcp_keys ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPKey
	for rows.Next() {
		k, err := scanMCPKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) FindMCPKey(keyHash string) (*MCPKey, error) {
	row := s.db.QueryRow(`SELECT `+mcpKeyCols+` FROM mcp_keys WHERE key_hash = ?`, keyHash)
	return scanMCPKey(row.Scan)
}

func (s *sqliteStore) TouchMCPKey(id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE mcp_keys SET last_used_at = ? WHERE id = ?`, now.Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) DeleteMCPKey(id string) error {
	res, err := s.db.Exec(`DELETE FROM mcp_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const mcpKeyCols = `id, name, prefix, key_hash, created_at, last_used_at, scopes`

func scanMCPKey(scan func(dest ...any) error) (*MCPKey, error) {
	var (
		k                     MCPKey
		createdAt, lastUsedAt int64
		scopesCSV             string
	)
	if err := scan(&k.ID, &k.Name, &k.Prefix, &k.KeyHash, &createdAt, &lastUsedAt, &scopesCSV); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.CreatedAt = time.Unix(createdAt, 0)
	if lastUsedAt > 0 {
		k.LastUsedAt = time.Unix(lastUsedAt, 0)
	}
	k.Scopes = splitScopes(scopesCSV)
	return &k, nil
}

// splitScopes 反序列化逗号分隔 scope（空串 → nil，表示全权）。
func splitScopes(csv string) []string {
	if csv == "" {
		return nil
	}
	out := make([]string, 0, 3)
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// joinScopes 序列化 scope 列表为逗号分隔（nil → 空串）。
func joinScopes(scopes []string) string { return strings.Join(scopes, ",") }

const siteCols = `id, name, entry, password_hash, spa, current_version, created_at, updated_at, expires_at, visits, last_visit_at, pruned_to`

func (s *sqliteStore) scanSite(scan func(dest ...any) error) (*Site, error) {
	var (
		site                     Site
		spa                      int
		createdAt, updatedAt     int64
		expiresAt, lastVisitedAt int64
	)
	if err := scan(&site.ID, &site.Name, &site.Entry, &site.PasswordHash, &spa,
		&site.CurrentVersion, &createdAt, &updatedAt, &expiresAt,
		&site.Visits, &lastVisitedAt, &site.PrunedTo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	site.SPAFallback = spa != 0
	site.CreatedAt = time.Unix(createdAt, 0)
	site.UpdatedAt = time.Unix(updatedAt, 0)
	if expiresAt > 0 {
		site.ExpiresAt = time.Unix(expiresAt, 0)
	}
	if lastVisitedAt > 0 {
		site.LastVisitedAt = time.Unix(lastVisitedAt, 0)
	}
	return &site, nil
}

// ListVersions 返回某站点全部版本记录（升序）。
func (s *sqliteStore) ListVersions(siteID string) ([]*Version, error) {
	rows, err := s.db.Query(
		`SELECT site_id, version, created_at, file_count, total_bytes FROM versions WHERE site_id = ? ORDER BY version`,
		strings.ToUpper(siteID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Version
	for rows.Next() {
		var v Version
		var created int64
		if err := rows.Scan(&v.SiteID, &v.Version, &created, &v.FileCount, &v.TotalBytes); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(created, 0)
		out = append(out, &v)
	}
	return out, rows.Err()
}

// SetCurrentVersion 切版本指针（回滚/发布共用）；不涨 versions 表。
func (s *sqliteStore) SetCurrentVersion(id string, version int64) (*Site, error) {
	res, err := s.db.Exec(`UPDATE sites SET current_version = ?, updated_at = ? WHERE id = ?`,
		version, time.Now().Unix(), strings.ToUpper(id))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetSite(id)
}

// MarkPrunedTo 单调前移水位线。
func (s *sqliteStore) MarkPrunedTo(id string, to int64) error {
	res, err := s.db.Exec(`UPDATE sites SET pruned_to = ? WHERE id = ? AND pruned_to < ?`,
		to, strings.ToUpper(id), to)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 水位未动可能是"只进不退"，也可能是站点不存在——后者须与 JSON 后端一致报 ErrNotFound
		if _, err := s.GetSite(id); err != nil {
			return err
		}
	}
	return nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
