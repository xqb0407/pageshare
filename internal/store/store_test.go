package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// TestJSONStorePersist JSON 存储的完整生命周期：建站 → 发布 → 重开文件 → 数据还在。
func TestJSONStorePersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sites.json")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSite(&Site{ID: "ABCD1234", Name: "测试", Entry: "index.html"}); err != nil {
		t.Fatal(err)
	}
	ttl := 72 * time.Hour
	spa := true
	if err := st.PublishVersion(&Version{SiteID: "ABCD1234", Version: 1, FileCount: 3, TotalBytes: 100},
		"index.html", strPtr("hash"), &ttl, &spa); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：数据应完整
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.GetSite("abcd1234") // 大小写不敏感
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentVersion != 1 || !got.SPAFallback || got.PasswordHash != "hash" {
		t.Fatalf("重开后字段不完整: %+v", got)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("TTL 应已记录")
	}
	if _, err := st2.GetSite("ZZZZZZZZ"); err != ErrNotFound {
		t.Fatalf("不存在的站点应 ErrNotFound，得到 %v", err)
	}
	// 文件本身可读
	if raw := readFile(t, path); len(raw) == 0 {
		t.Fatal("sites.json 应存在")
	}

	// 删除
	if _, err := st2.DeleteSite("ABCD1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.GetSite("ABCD1234"); err != ErrNotFound {
		t.Fatal("删除后应不存在")
	}
}

// TestOpenAutoDetect 后缀路由：.db 走 SQLite，.json 走 JSON。
func TestOpenAutoDetect(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "a.json")); err != nil {
		t.Fatalf("json: %v", err)
	}
	if _, err := Open(filepath.Join(dir, "b.db")); err != nil {
		t.Fatalf("sqlite: %v", err)
	}
}

// TestMCPKeyStore 密钥池生命周期：JSON 与 SQLite 两种后端行为一致。
func TestMCPKeyStore(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "k.json"), filepath.Join(dir, "k.db")} {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		// 创建两把，id/hash 重复应被拒
		if err := st.CreateMCPKey(&MCPKey{ID: "k1", Name: "claude", Prefix: "psm_aaaa1111", KeyHash: "hash-a"}); err != nil {
			t.Fatalf("%s: create k1: %v", path, err)
		}
		if err := st.CreateMCPKey(&MCPKey{ID: "k2", Name: "cursor", Prefix: "psm_bbbb2222", KeyHash: "hash-b"}); err != nil {
			t.Fatalf("%s: create k2: %v", path, err)
		}
		if err := st.CreateMCPKey(&MCPKey{ID: "k1", KeyHash: "hash-c"}); err == nil {
			t.Fatalf("%s: 重复 id 应报错", path)
		}
		if err := st.CreateMCPKey(&MCPKey{ID: "k3", KeyHash: "hash-a"}); err == nil {
			t.Fatalf("%s: 重复 hash 应报错", path)
		}

		// 按 hash 查找（鉴权路径）
		k, err := st.FindMCPKey("hash-b")
		if err != nil || k.ID != "k2" {
			t.Fatalf("%s: find hash-b: %v %+v", path, err, k)
		}
		if _, err := st.FindMCPKey("nope"); err != ErrNotFound {
			t.Fatalf("%s: 未命中应 ErrNotFound，得到 %v", path, err)
		}

		// 回写最近使用时间
		now := time.Now()
		if err := st.TouchMCPKey("k2", now); err != nil {
			t.Fatalf("%s: touch: %v", path, err)
		}
		if err := st.TouchMCPKey("ghost", now); err != ErrNotFound {
			t.Fatalf("%s: touch 不存在应 ErrNotFound", path)
		}
		k, err = st.FindMCPKey("hash-b")
		if err != nil || k.LastUsedAt.IsZero() {
			t.Fatalf("%s: touch 后 LastUsedAt 应非零: %+v", path, k)
		}

		// 列表：创建时间倒序
		list, err := st.ListMCPKeys()
		if err != nil || len(list) != 2 || list[0].ID != "k2" {
			t.Fatalf("%s: list 应 2 项且 k2 在前: %v %+v", path, err, list)
		}

		// P1-5：scope 往返（SQLite 逗号分隔落库 / JSON 直存）
		if err := st.CreateMCPKey(&MCPKey{ID: "k4", Name: "ci", Prefix: "psm_cccc3333",
			KeyHash: "hash-d", Scopes: []string{"publish", "read"}}); err != nil {
			t.Fatalf("%s: create k4: %v", path, err)
		}
		k4, err := st.FindMCPKey("hash-d")
		if err != nil || len(k4.Scopes) != 2 || k4.Scopes[0] != "publish" || k4.Scopes[1] != "read" {
			t.Fatalf("%s: scope 往返: %v %+v", path, err, k4.Scopes)
		}
		// 空 scope（全权密钥，含升级前的存量密钥）读回应为 nil
		if kf, err := st.FindMCPKey("hash-a"); err != nil || kf.Scopes != nil {
			t.Fatalf("%s: 空 scope 应读回 nil: %v %+v", path, err, kf.Scopes)
		}

		// 删除后消失
		if err := st.DeleteMCPKey("k2"); err != nil {
			t.Fatalf("%s: delete: %v", path, err)
		}
		if err := st.DeleteMCPKey("k2"); err != ErrNotFound {
			t.Fatalf("%s: 重复删除应 ErrNotFound", path)
		}
		if _, err := st.FindMCPKey("hash-b"); err != ErrNotFound {
			t.Fatalf("%s: 删除后不应命中", path)
		}

		// 重开：scope 持久化
		if err := st.Close(); err != nil {
			t.Fatalf("%s: close: %v", path, err)
		}
		st2, err := Open(path)
		if err != nil {
			t.Fatalf("%s: reopen: %v", path, err)
		}
		if kk, err := st2.FindMCPKey("hash-d"); err != nil || len(kk.Scopes) != 2 || kk.Scopes[1] != "read" {
			t.Fatalf("%s: 重开后 scope 应保留: %v %+v", path, err, kk)
		}
		st2.Close()
	}
}

func strPtr(s string) *string { return &s }

// TestSiteMetaUpdateAndVisits 元数据修改与页览并入：JSON 与 SQLite 两种后端行为一致。
func TestSiteMetaUpdateAndVisits(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "m.json"), filepath.Join(dir, "m.db")} {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := st.CreateSite(&Site{ID: "SITE0001", Name: "旧名", Entry: "index.html"}); err != nil {
			t.Fatalf("%s: create: %v", path, err)
		}
		ttl := 24 * time.Hour
		if err := st.PublishVersion(&Version{SiteID: "SITE0001", Version: 3, FileCount: 2, TotalBytes: 50},
			"", nil, &ttl, nil); err != nil {
			t.Fatalf("%s: publish: %v", path, err)
		}

		// 只改 name：其余字段不动，版本不涨
		got, err := st.UpdateSiteMeta("SITE0001", strPtr("新名"), nil, nil, nil)
		if err != nil {
			t.Fatalf("%s: update name: %v", path, err)
		}
		if got.Name != "新名" || got.CurrentVersion != 3 || got.PasswordHash != "" {
			t.Fatalf("%s: 只改 name 不应动其他字段: %+v", path, got)
		}
		if got.ExpiresAt.IsZero() {
			t.Fatalf("%s: ttl 不传应保持: %+v", path, got)
		}

		// 设密码 + 清 TTL（ttl==0 → 永久）+ 开 SPA
		if _, err := st.UpdateSiteMeta("SITE0001", nil, strPtr("hash-x"), ptrDur(0), boolPtr(true)); err != nil {
			t.Fatalf("%s: update multi: %v", path, err)
		}
		got, _ = st.GetSite("SITE0001")
		if got.PasswordHash != "hash-x" || !got.ExpiresAt.IsZero() || !got.SPAFallback {
			t.Fatalf("%s: 多字段修改未生效: %+v", path, got)
		}
		if got.CurrentVersion != 3 {
			t.Fatalf("%s: 元数据修改不应涨版本: %+v", path, got)
		}

		// 重新设 TTL
		if _, err := st.UpdateSiteMeta("SITE0001", nil, nil, ptrDur(72*time.Hour), nil); err != nil {
			t.Fatalf("%s: update ttl: %v", path, err)
		}
		// 不存在的站点
		if _, err := st.UpdateSiteMeta("NOPE0000", strPtr("x"), nil, nil, nil); err != ErrNotFound {
			t.Fatalf("%s: 不存在应 ErrNotFound，得到 %v", path, err)
		}

		// 页览并入：多次累加；last 只进不退；未知 id 静默跳过
		t1 := time.Now()
		if err := st.MergeVisits(map[string]int64{"SITE0001": 3, "GONE0001": 5},
			map[string]time.Time{"SITE0001": t1, "GONE0001": t1}); err != nil {
			t.Fatalf("%s: merge1: %v", path, err)
		}
		if err := st.MergeVisits(map[string]int64{"SITE0001": 2},
			map[string]time.Time{"SITE0001": t1.Add(-time.Hour)}); err != nil {
			t.Fatalf("%s: merge2: %v", path, err)
		}
		got, _ = st.GetSite("SITE0001")
		if got.Visits != 5 {
			t.Fatalf("%s: 页览应累加为 5: %+v", path, got)
		}
		// （SQLite 存秒级 Unix，断言容忍 1s 截断误差）
		if got.LastVisitedAt.Before(t1.Add(-time.Second)) {
			t.Fatalf("%s: 更早的 last 不应覆盖: %+v", path, got)
		}
		if _, err := st.GetSite("GONE0001"); err != ErrNotFound {
			t.Fatalf("%s: MergeVisits 不应创建站点", path)
		}

		// 重开后统计仍在（SQLite 无重开语义差异，一并覆盖）
		if err := st.Close(); err != nil {
			t.Fatalf("%s: close: %v", path, err)
		}
		st2, err := Open(path)
		if err != nil {
			t.Fatalf("%s: reopen: %v", path, err)
		}
		got, err = st2.GetSite("SITE0001")
		if err != nil || got.Visits != 5 || got.Name != "新名" {
			t.Fatalf("%s: 重开后应保留: %v %+v", path, err, got)
		}
		_ = st2.Close()
	}
}

func ptrDur(d time.Duration) *time.Duration { return &d }
func boolPtr(b bool) *bool                  { return &b }

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := osReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestVersionLedgerAndPruneWatermark 覆盖 P1-4 存储语义：版本账本查询、
// 指针切换不涨账本、删除水位只前进、PrunedTo 持久化——双后端同断言。
func TestVersionLedgerAndPruneWatermark(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "v.json"), filepath.Join(dir, "v.db")} {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := st.CreateSite(&Site{ID: "VER00001", Entry: "index.html"}); err != nil {
			t.Fatalf("%s: create: %v", path, err)
		}
		for _, v := range []int64{3, 1, 2} { // 乱序写入，查询应升序
			if err := st.PublishVersion(&Version{SiteID: "VER00001", Version: v, FileCount: int(v), TotalBytes: 100 * v}, "", nil, nil, nil); err != nil {
				t.Fatalf("%s: publish v%d: %v", path, v, err)
			}
		}
		vers, err := st.ListVersions("VER00001")
		if err != nil {
			t.Fatalf("%s: list: %v", path, err)
		}
		if len(vers) != 3 || vers[0].Version != 1 || vers[1].Version != 2 || vers[2].Version != 3 {
			t.Fatalf("%s: 版本应升序 1,2,3: %+v", path, vers)
		}
		if vers[0].SiteID != "VER00001" || vers[2].TotalBytes != 300 {
			t.Fatalf("%s: 版本行字段不对: %+v", path, vers[2])
		}
		// 大小写不敏感
		if vs, err := st.ListVersions("ver00001"); err != nil || len(vs) != 3 {
			t.Fatalf("%s: ListVersions 应大小写不敏感: %v %+v", path, err, vs)
		}
		// 返回副本：改动不应影响存储
		vers[0].FileCount = 999
		if again, _ := st.ListVersions("VER00001"); again[0].FileCount != 1 {
			t.Fatalf("%s: ListVersions 不应暴露内部指针: %+v", path, again[0])
		}
		// 未知站点：空列表非报错
		if vs, err := st.ListVersions("NOPE0000"); err != nil || len(vs) != 0 {
			t.Fatalf("%s: 未知站点应为空: %v %+v", path, err, vs)
		}

		// 切指针（回滚到 2）：current 变 2，updated_at 前进，版本行不增
		before, _ := st.GetSite("VER00001")
		got, err := st.SetCurrentVersion("VER00001", 2)
		if err != nil {
			t.Fatalf("%s: set current: %v", path, err)
		}
		if got.CurrentVersion != 2 || got.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("%s: 指针未切换/未刷新: %+v", path, got)
		}
		if vs, _ := st.ListVersions("VER00001"); len(vs) != 3 {
			t.Fatalf("%s: 切指针不应动版本账本", path)
		}
		if _, err := st.SetCurrentVersion("NOPE0000", 1); err != ErrNotFound {
			t.Fatalf("%s: 不存在应 ErrNotFound，得 %v", path, err)
		}

		// 水位只前进
		if err := st.MarkPrunedTo("VER00001", 1); err != nil {
			t.Fatalf("%s: prune1: %v", path, err)
		}
		if err := st.MarkPrunedTo("VER00001", 0); err != nil {
			t.Fatalf("%s: 水位回退应静默保持: %v", path, err)
		}
		if g, _ := st.GetSite("VER00001"); g.PrunedTo != 1 {
			t.Fatalf("%s: 水位应停在 1: %+v", path, g)
		}
		if err := st.MarkPrunedTo("VER00001", 2); err != nil {
			t.Fatalf("%s: prune2: %v", path, err)
		}
		if err := st.MarkPrunedTo("NOPE0000", 5); err != ErrNotFound {
			t.Fatalf("%s: 未知站点应 ErrNotFound，得 %v", path, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("%s: close: %v", path, err)
		}
		st2, err := Open(path)
		if err != nil {
			t.Fatalf("%s: reopen: %v", path, err)
		}
		if g, err := st2.GetSite("VER00001"); err != nil || g.PrunedTo != 2 || g.CurrentVersion != 2 {
			t.Fatalf("%s: 水位/指针应持久化: %+v %v", path, g, err)
		}
		_ = st2.Close()
	}
}
