# pageshare 设计文档

v1 · 2026-09-24

## 背景与目标

AI（agent）在工作区里产出静态页面（HTML/CSS/JS 站点）后，用户需要拿到一个**公网可访问的链接**分享给他人。
pageshare 是一个自托管的分享控制面：接收整站归档，转存 S3 兼容对象存储（AWS S3 / Cloudflare R2 / MinIO），
生成形如 `https://host/s/{id}/` 的分享链接。

v1 交付：服务本体 + 内嵌管理 UI。agent 侧插件（skill + 上传脚本）后续做，
对接面就是管理 API（README 有 curl 示例）。

## 决策记录

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 存储后端 | S3/R2 私有桶，服务只做控制面 | 内容不入服务盘；R2 出口流量免费；桶私有避免公开桶被扫 |
| 访问路径 | 无密码 302 presigned；有密码代理回源 | 无密码不吃服务带宽；密码站必须经服务校验口令，无法用 presigned |
| 元数据 | JSON 文件（sites.json，原子写）；`.db` 后缀兼容 SQLite | 个人服务体量下登记簿就几十行，JSON 可读可备份零依赖；SQLite 保留为兼容选项 |
| 管理 UI | Vite+React 内嵌 go:embed | 单二进制自带 UI；组件与主题直接复用 slide-canvas 的 Wise 体系 |
| 站点 id | 8 位 Crockford Base32 | 40bit 随机空间、URL 安全、防误读（I/L→1，O→0） |
| 版本模型 | 每次覆盖发布新版本号，旧版本对象发布成功后清除 | URL 永不变化是硬需求；回滚不在 v1 范围 |
| 托管形态 | 子路径 `/s/{id}/` + 可选泛域名 `{id}.host` 整域 | 泛域模式下站点在根路径运行，Vite `base:/` 产物零改动（nginx 语义）；保留根路径（/api、/admin 等）不参与改写 |
| SPA/404 | spa 站点 HTML 未命中回入口页；普通站点输出 404.html | 客户端路由深链接全通；404 渲染归前端路由 |
| 缓存 | 上传时按文件名写 CacheControl 对象元数据 | html no-cache / hash 产物 immutable / 其余 1h；密码站点代理一律 no-store |

## 架构

```
HTTP（Go 1.22 ServeMux）
├── /api/v1/sites        管理 API（Bearer token）
├── /s/{id}/…            公开分享面（TTL/密码门/presigned/代理）
├── /admin/…             管理 UI（内嵌静态）
└── /__memstore/…        仅 mem 后端：presigned 同源回源（dev/e2e）

依赖装配：config → store(SQLite) + storage(接口：S3|Mem) → Server
```

关键接口：

```go
type Storage interface {
    Put(ctx, key, r, size, contentType) error
    Get(ctx, key) (*Object, error)          // 密码站点代理回源用
    DeletePrefix(ctx, prefix) (int, error)  // 覆盖/吊销/清扫
    PresignGet(ctx, key, ttl) (string, error)
}
```

对象 key 布局：`[prefix]/{siteID}/{version}/{相对路径}`，`siteID` 大写，相对路径经
`SanitizeRelPath` 清洗。删除与覆盖都是 DeletePrefix 粒度，无需维护文件清单。

## 数据模型

```
sites(id PK, name, entry, password_hash, current_version, created_at, updated_at, expires_at)
versions(site_id+version PK, created_at, file_count, total_bytes)
```

- `entry`：入口 html。建站时取根 `index.html`，否则第一个 `*.html`。
- `expires_at`：0 = 永久。覆盖发布可带 ttl 替换；访问时惰性判断 410，后台每小时清扫删除。
- 删除是硬删（元数据 + 对象前缀一起清），v1 不做软删除。

## 发布流程（POST/PUT 共用 publish）

1. 读归档：multipart（`file` 字段 + `name`/`ttl`/`password`）或原始 body + query 参数。
2. 压缩体入内存（≤ MaxUploadMB），按 magic（`PK\x03\x04`）识别 zip，否则按 tar.gz 解析。
3. 逐条目清洗路径（拒绝 `..`/绝对路径/符号链接/反斜杠；跳过 macOS/git 杂物），
   计数限额（≤2000 文件、≤500MB 解压总量），逐文件 PutObject（Content-Type 按扩展名）。
4. 事务写版本行 + 推进 `current_version`/`updated_at`/`expires_at`/`password_hash`。
5. 失败即 DeletePrefix 清半成品；PUT 成功后清旧版本前缀。

## 密码门

- `password_hash` 用 bcrypt。访客无 cookie → 401 口令页（自绘 Wise 风格表单）。
- POST 正确 → 303 + `psg_{id}` cookie，值为 `"{exp}.{HMAC(cookieSecret, siteID|exp)}"`，
  HMAC-SHA256 + RawURLBase64，无服务端会话状态，24h 有效。
- 有密码站点所有内容响应 `Cache-Control: no-store`，防中间缓存。

## 安全清单

- 管理 token / cookie 密钥 constant-time 比较；二者独立，泄露互不影响。
- 桶私有；presigned URL 5 分钟有效期，仅对无密码站点签发。
- 防开放跳转：gate 的 `next` 只允许本站点内相对路径。
- 上传限额三层：压缩体、文件数、解压总量（流式计数，超限即断）。

## 已知取舍 / 后续路线

- v1 无访问统计、多 token/多用户、版本回滚、自定义域名映射；数据模型都留了口子。
- 纯静态页面；不做服务端渲染/函数。
- agent 插件（对 ai-teamplte 仓库的 publish-share 插件）：SKILL.md 教 agent
  `tar -czf` + curl 调 API，或 UI 面板直接拖拽上传。
- mem 后端仅用于 dev/e2e，不应在生产配置出现。
