# pageshare

把 AI 生成的静态页面（多文件站点）上传到自己的服务器，转存 **S3/R2 私有桶**，生成公开分享链接。
单 Go 二进制自带管理 UI（内嵌），支持：**覆盖更新 / 吊销删除 / 有效期 TTL / 访问密码**。

```
浏览器管理页 /admin ──┐
agent/脚本 ───────────┼─zip 或 tar.gz─▶ pageshare（公网，Caddy/Nginx 反代 TLS）
                      ▼                 ├─ 元数据 SQLite
                                        └─ S3/R2 私有桶（S3 兼容 API）
访客 ──GET /s/{id}/path──▶ 无密码: 302 → presigned URL
                          有密码: 口令门 → 代理回源（HMAC 签名 cookie 会话 24h）
```

## 快速开始（本地）

```bash
go run ./cmd/pageshare -dev
# 终端会打印 "开发模式管理 token: <hex>"
open http://127.0.0.1:8300/admin/
```

- `-dev` 模式使用内存存储（重启即丢数据）+ 随机 token，只用于试跑。
- 管理 UI 开发：`cd frontend && bun install && bun run dev`（代理 API 到 127.0.0.1:8300）。

## 构建

```bash
# 前端产物 → internal/server/web/dist（go:embed 内嵌）
cd frontend && bun install && bun run build && cd ..
# 单二进制
go build -o pageshare ./cmd/pageshare
```

`internal/server/web/dist` 提交有构建产物（克隆即可 `go build` 得到带 UI 的完整二进制）。

## 配置

JSON 配置文件（`-config pageshare.json`），环境变量可逐项覆盖（`PS_*`）：

```json
{
  "listen": "127.0.0.1:8300",
  "public_base_url": "https://share.你的域名.com",
  "site_wildcard_host": "s.share.你的域名.com",
  "auth_token": "<管理 token，openssl rand -hex 16>",
  "cookie_secret": "<密码门签名密钥，openssl rand -hex 32>",
  "db_path": "sites.json",
  "max_upload_mb": 100,
  "versions_kept": 3,
  "storage": {
    "backend": "s3",
    "endpoint": "https://<account>.r2.cloudflarestorage.com",
    "region": "auto",
    "bucket": "pageshare",
    "access_key_id": "...",
    "secret_access_key": "..."
  }
}
```

> 元数据默认存 `sites.json`（人类可读、原子写、零数据库依赖，备份就是拷文件）；
> `db_path` 传 `.db` 后缀则走 SQLite（兼容旧部署）。页面内容本身存对象存储，不进元数据文件。

| 环境变量 | 对应字段 |
| --- | --- |
| `PS_LISTEN` / `PS_PUBLIC_BASE_URL` / `PS_SITE_WILDCARD_HOST` / `PS_AUTH_TOKEN` / `PS_COOKIE_SECRET` / `PS_DB_PATH` | 同名 |
| `PS_STORAGE_BACKEND` (`s3`\|`mem`) | `storage.backend` |
| `PS_S3_ENDPOINT` / `PS_S3_REGION` / `PS_S3_BUCKET` / `PS_S3_ACCESS_KEY_ID` / `PS_S3_SECRET_ACCESS_KEY` / `PS_S3_PREFIX` | `storage.*` |

> **R2**：endpoint 形如 `https://<accountid>.r2.cloudflarestorage.com`，region 填 `auto`。
> **S3**：endpoint 留空，region 填桶所在区。桶保持私有，访问一律经服务侧签名/代理。

### 两种托管形态

- **子路径模式**（默认）：`https://share.example.com/s/{id}/`。适合纯相对路径引用的页面。
- **整域模式**（配了 `site_wildcard_host`）：`https://{id}.s.share.example.com/`——站点跑在自己子域名的**根路径**上，语义等同 nginx 托管，**Vite/CRA 默认 `base:"/"` 产物（绝对路径 `/assets/...`）零改动直接可用**。需要：DNS 泛解析 `*.s.share.example.com` → 服务器；Caddy 开 on-demand TLS（pageshare 提供 `GET /domain-ask?domain=…` 作为 `ask` 端点，站点存在才签证书）：

  ```caddyfile
  *.s.share.example.com {
      tls {
          on_demand
          ask http://127.0.0.1:8300/domain-ask
      }
      reverse_proxy 127.0.0.1:8300
  }
  ```

### SPA 与缓存

- 发布时传 `spa=1`（UI 里勾选"SPA 站点"）：HTML 路径未命中回退入口页（200），React Router 等客户端路由深链接全通；404 渲染交给前端路由。
- 非 SPA 站点：HTML 未命中若包内有 `404.html` 则按 404 状态输出。
- 缓存策略（上传时写入对象元数据）：html `no-cache`；带内容 hash 的构建产物（`app-Bx91kQze.js` 这类）`max-age=31536000, immutable`；其余 `max-age=3600`。密码站点内容一律 `no-store` 代理回源。

## API（Bearer auth_token）

| 方法与路径 | 说明 |
| --- | --- |
| `POST /api/v1/sites` | 建站+发布。multipart（`file`=归档，文本字段 `name`/`ttl`/`password`）或原始 body+query 参数 |
| `PUT /api/v1/sites/{id}` | 覆盖发布新版本，URL 不变；旧版本对象清除 |
| `GET /api/v1/sites` / `GET /api/v1/sites/{id}` | 列表 / 详情 |
| `PATCH /api/v1/sites/{id}` | 纯元数据修改（JSON body：`name`/`ttl`/`password`/`spa`，只改出现的字段）：不重传归档、不涨版本；`ttl` 从当前时刻重新起算，`never` 清为永久；`password` 空串清除 |
| `DELETE /api/v1/sites/{id}` | 吊销：元数据与对象全部清除 |
| `PUT /api/v1/uploads/{upload_id}` | 两阶段上传数据面：原始归档字节进暂存区（管理 token 或 `psm_` 密钥均可） |
| `GET /healthz` | 健康检查 |

- 归档支持 zip 与 tar.gz；上传体 ≤ `max_upload_mb`（默认 100MB）、≤2000 文件、解压后 ≤500MB。
- `ttl`：`72h`、`30m`、`7d`、`never`；`password`：字段出现即替换（空串=清除密码）；`spa`：`1` 开启回退（字段出现即替换）。
- 返回 `{id, name, url, version, has_password, entry, created_at, updated_at, expires_at?, visits, last_visit_at?}`（`visits` 为页览数，含未刷盘增量）。

curl 示例（也是未来 agent 插件上传脚本的调用方式）：

```bash
# 打包当前目录并发布（tar.gz）
tar -czf /tmp/site.tgz -C dist .
curl -sS -X POST "https://share.example.com/api/v1/sites?name=Q3%E8%B7%AF%E6%BC%94&ttl=72h" \
  -H "Authorization: Bearer $PS_AUTH_TOKEN" \
  -H "Content-Type: application/gzip" --data-binary @/tmp/site.tgz
# → {"id":"ABCD1234","url":"https://share.example.com/s/abcd1234/",...}

# 覆盖更新
curl -sS -X PUT "https://share.example.com/api/v1/sites/abcd1234" \
  -H "Authorization: Bearer $PS_AUTH_TOKEN" \
  -H "Content-Type: application/gzip" --data-binary @/tmp/site.tgz

# 只改元数据：续期 + 补开 SPA（不重传，URL/版本不变）
curl -sS -X PATCH "https://share.example.com/api/v1/sites/abcd1234" \
  -H "Authorization: Bearer $PS_AUTH_TOKEN" \
  -H "Content-Type: application/json" -d '{"ttl":"72h","spa":true}'
```

## MCP：给 AI 客户端直连的能力

pageshare 内置 MCP server，任何 MCP 客户端（Harness、Claude Desktop、Cursor、pi-agent 等）都能直接发布/管理站点。

**工具集**：`publish_site`（传本地目录或 zip/tar.gz 路径 → 打包发布/覆盖，返回分享链接）、`begin_upload` + `commit_upload`（两阶段上传，见下）、`list_sites`、`get_site`、`update_site`（不重传改 name/ttl/密码/SPA，版本不变）、`get_site_stats`（页览统计：单站或全部汇总 + 最近访问，可空参看全量）、`list_site_versions`（发布历史 + 可回滚标注）、`rollback_site`（把当前版本原子切回保留窗口内的历史版本，链接不变、立即生效）、`delete_site`。

**两阶段上传（远程 agent 的发布通道）**：`publish_site` 的 `path` 必须在 pageshare 所在机器上；HTTP 连接的远程 agent 改为 `begin_upload`（签票据，15 分钟有效，返回 `put_url`）→ 把归档字节直接 `PUT` 到 `put_url`（Bearer 用 `psm_` 密钥即可）→ `commit_upload`（服务端读取暂存走与 publish_site 完全相同的发布流水线，返回同形结果）。控制面走 MCP JSON，大字节流走普通 HTTP 体；失败重试不消费票据。

**版本保留与回滚**：`versions_kept` 控制覆盖发布后保留最近几版对象（默认 1 = 发布成功即删旧版；建议 3；0 = 全保留，存储会线性增长，慎用）。窗口内的历史版本用 `rollback_site` 秒级切回（只翻 `current_version` 指针，不搬对象、链接不变），`list_site_versions` 标注每个版本是否还"在窗口内可回滚"。回滚后再发布拿更高的新版本号，不会与保留版本撞前缀。它是发布事故的回退保险，不是内容归档——超窗版本随发布自动物理删除，TTL 到期仍整体清除。

访问统计按**页览**计：入口页/目录/`.html` 与 SPA 深链计入，静态资源（js/css/图）不计；密码站只有过了口令门才计。访客路径只加内存计数，分钟级批量并入元数据，读取时实时合并未刷部分。

### 方式一：stdio（推荐本地客户端）

```bash
./pageshare mcp -config pageshare.json   # 前台运行，JSON-RPC 走 stdin/stdout
```

客户端配置示例（Claude Desktop / 通用 mcp.json 格式）：

```json
{
  "mcpServers": {
    "pageshare": {
      "command": "/opt/pageshare/pageshare",
      "args": ["mcp", "-config", "/opt/pageshare/pageshare.json"]
    }
  }
}
```

### 方式二：HTTP（远程客户端）

服务自带 `http(s)://<host>:8300/mcp`（Streamable HTTP，标准 JSON-RPC 2.0）。
鉴权支持两种 Bearer 凭据：管理 `auth_token`，或 **MCP 密钥池**里的密钥。

**密钥池**：在管理台 `https://…/admin/mcp` 为每个客户端签发独立密钥（`psm_` 前缀，
明文只在签发时展示一次，服务端只存 SHA-256）。密钥可随时吊销、互不影响，
并按密钥记录最近使用时间；池容量上限 20 把。签发时可用 `scopes` 限定授权面
（`publish` / `read` / `delete`，缺省 `publish,read`——删除永远要显式加）；
升级前签发的存量密钥保持全权限。管理 API：
`GET/POST /api/v1/mcp/keys`、`DELETE /api/v1/mcp/keys/{id}`（管理 token 鉴权）。

**工具治理**：HTTP 连接的每次 `tools/call` 先过一道按密钥的门禁——scope 不符直接拒
（错误里点名缺哪个权限）；每密钥限流每分钟 60 次总量、其中发布类（publish / 两阶段上传 /
rollback）10 次，超限返回带 `Retry-After` 的重试提示。管理 token 与本地 stdio 连接
（`pageshare mcp`）不受 scope 与限流约束——那是运维者本人的手。放行后的每次调用
（不论成败）追加一行 JSONL 落到与元数据同目录的 `audit.jsonl`（记密钥、agent、工具、
站点、结果；10MB×5 滚动上限），控制台/API 可查：`GET /api/v1/mcp/audit?site=&key=&since=&limit=`
（管理 token 鉴权，新→旧）。审计是文件不入库，写失败绝不反向卡死工具调用。

**连接池**：多个 agent 可同时连到 `/mcp`，服务端统一编排——活动会话上限 10 条，
每条按 `Mcp-Session-Id` 登记客户端自报的 agent 名与所用密钥，空闲 30 分钟自动回收，
客户端 `DELETE /mcp` 立即释放；池满时新的 `initialize` 返回 503（带 `Retry-After`），
已有连接不受影响。管理台 `/admin/mcp` 的「活动连接」区块实时展示在线 agent；
API：`GET /api/v1/mcp/sessions`（管理 token 鉴权）。同站并发发布由服务端逐站加锁
串行化（版本连号不互相覆盖），新建站的全局锁消除撞号竞态。

```json
{
  "mcpServers": {
    "pageshare": {
      "url": "https://share.example.com/mcp",
      "headers": { "Authorization": "Bearer psm_你的密钥" }
    }
  }
}
```

> stdio 模式无需 HTTP，凭据即配置文件本身；工具里的本地路径就是 pageshare 进程可见的路径。

## 公开面

- `GET /s/{id}/`：站点入口（默认 `index.html`，缺省回退到第一个 `*.html`）。
- 无密码站点 302 到 5 分钟有效的 presigned URL；有密码站点先过口令页（`/s/{id}/gate`），
  通过后 24h 会话内由服务代理回源（`Cache-Control: no-store`）。
- 过期返回 410，吊销/不存在返回 404。

## 部署

### Docker Compose（推荐）

```bash
./deploy.sh                 # 一键：自动生成随机密钥 .env → 构建 → 启动 → 等健康 → 打印访问地址
./deploy.sh --tls           # 同上 + Caddy TLS 反代（先改 deploy/Caddyfile 的域名/邮箱）
./deploy.sh update|stop|logs|status|down    # 其余子命令
```

手工方式（想自己管 `.env` 时）：

```bash
cp .env.example .env      # 填 PS_AUTH_TOKEN / PS_COOKIE_SECRET（openssl rand -hex 16/32）
docker compose up -d --build
open http://localhost:8300/admin/
```

元数据与 MCP 审计落在 named volume `pageshare-data`（容器内 `/data`），升级镜像不丢数据；
默认 `disk` 存储后端开箱即用，生产切 Cloudflare R2/任意 S3——在 `.env` 里换 `PS_STORAGE_BACKEND=s3`
并填 `PS_S3_*` 五项即可，桶保持私有。要公网 TLS 与泛分享子域（on_demand 证书 + `/domain-ask` 裁决），
改好 `deploy/Caddyfile` 里的域名与邮箱后 `docker compose --profile tls up -d`。
镜像为两阶段纯 Go 构建（`CGO_ENABLED=0`，无 cgo 依赖），运行时只有 alpine + 单二进制，非 root 用户。

### 裸二进制 + Caddy 反代

```caddyfile
share.example.com {
    reverse_proxy 127.0.0.1:8300
}
```

```bash
AUTH=$(openssl rand -hex 16); COOKIE=$(openssl rand -hex 32)
./pageshare -config /etc/pageshare/pageshare.json   # 建议配 systemd 常驻
```

SQLite/JSON 元数据按 `db_path` 落盘；TTL 过期站点由后台每小时清扫（外加访问时惰性判断）。

### 发布打包

`./scripts/release.sh`（可 `VERSION=v1.2.3` 指定）交叉编译全平台矩阵到 `dist/`：
linux/amd64、linux/arm64、darwin/arm64、darwin/amd64、windows/amd64——
每个包内含二进制、README、LICENSE、示例配置，附 `sha256sums.txt`。
`pageshare -version` 打印构建注入的版本号。

`go install` 或本地跑法见上文「构建」；纯 Go 依赖（modernc SQLite、AWS SDK）意味着任何
能跑 Go 的架构一条命令交叉编译，无需目标平台工具链。

## 安全设计

- 桶全程私有，凭据只在服务侧；上传/管理走 constant-time 比较的 Bearer token。
- 归档解包防 zip-slip：拒绝 `..`/绝对路径/符号链接/反斜杠，跳过 `__MACOSX`、`.DS_Store`、`.git`。
- 密码 bcrypt 存储；访客会话为无状态 HMAC 签名 cookie，密钥独立于管理 token。
- 覆盖/删除失败即清理半成品对象，不留孤儿。

设计文档：[docs/design.md](docs/design.md)　·　AI agent 功能设计：[docs/ai-agent-features.md](docs/ai-agent-features.md)

## License

[Apache License 2.0](LICENSE) —— 可自由使用、修改、分发（含商用）；分发时保留版权声明与许可证副本；贡献即视为按本协议授权。

Copyright 2026 The pageshare Authors
