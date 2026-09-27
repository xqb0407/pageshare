# AI Agent 能力：新功能分析与设计

> 状态：**P0 全部与 P1-4、P1-5 已实现**；P1-6 及以后仍为设计。基于 2026-09-27 代码盘点。
> 范围：MCP server、密钥池、连接池及与之配套的 HTTP API / 存储层。
> 管理台 UI、部署形态不在此文档范围。

---

## 1. 现状盘点

agent 侧目前的能力矩阵（√ 已有）：

| 能力 | MCP 工具 | HTTP API | 代码位置 |
| --- | --- | --- | --- |
| 发布/覆盖（新建、传 site_id 覆盖） | √ `publish_site` | √ `POST/PUT /api/v1/sites` | `internal/mcpserver/mcpserver.go` `internal/server/mcp.go` |
| 列表/详情 | √ `list_sites` `get_site` | √ `GET /api/v1/sites[/{id}]` | 同上 |
| 吊销删除 | √ `delete_site` | √ `DELETE /api/v1/sites/{id}` | 同上 |
| TTL / 密码 / SPA 开关 | √ 发布时随归档设置；√ `update_site` 不重传单改（已实现） | √ `POST/PUT` 发布时设置；√ `PATCH /api/v1/sites/{id}`（已实现） | `sites.go` `updateSiteMeta` |
| 两阶段上传（远程 agent 发布） | √ `begin_upload` + `commit_upload`（已实现） | √ `PUT /api/v1/uploads/{id}` | `uploads.go` `mcp.go` |
| 访问统计（页览） | √ `get_site_stats`（已实现，含未刷盘实时值） | √ `GET /api/v1/sites[/{id}]` 输出 `visits`/`last_visit_at` | `stats.go` `store.MergeVisits` |
| 版本保留与回滚 | √ `list_site_versions` + `rollback_site`（已实现） | √ 配置 `versions_kept`（默认 1=即删旧版） | `versions.go` `store.PrunedTo` |
| 接入鉴权 | √ 管理 token + `psm_` 密钥池（签发带 scopes/吊销/最近使用，上限 20） | √ `GET/POST/DELETE /api/v1/mcp/keys` | `mcpkeys.go` |
| 工具治理（scope 门禁/限流/审计） | √ 接收中间件拦截 tools/call：缺权限即拒、每密钥 60/min（发布类 10/min）、放行调用落 `audit.jsonl`（10MB×5 滚动）（已实现） | √ 签发带 `scopes`；`GET /api/v1/mcp/audit?site=&key=&since=&limit=` | `gate.go` `audit.go` |
| 多 agent 连接编排 | √ 会话池（上限 10、agent 名登记、空闲回收、503+Retry-After） | √ `GET /api/v1/mcp/sessions` | `mcpsessions.go` |
| 并发安全 | √ 逐站互斥锁 + 建站全局锁 | 同左 | `server.go` `lockSite` |
| 传输形态 | √ stdio + Streamable HTTP | — | `mcpserver.go` |

**结构性约束**（新功能设计必须绕开或改造的点）：

1. `publish_site` 的 `path` 是 **pageshare 进程所在机器的路径**。stdio 模式下没问题；HTTP 模式下远程 agent 的文件和服务器不共享文件系统，`path` 形同虚设——远程 agent 目前只能退回 curl 上传。
2. 所有可变属性（name/ttl/password/spa）**只能随一次完整发布修改**，改个密码也要重传整个归档。
3. 覆盖发布时**旧版本对象立即删除**，没有回滚余地；`versions` 表只是流水账。
4. 服务端对"发布之后发生了什么"完全失明：访客有没有打开链接、链接快到期，agent 无从得知，MCP 是请求-响应模型，没有事件通道。
5. 密钥池权限是全有或全无：一把 `psm_` 密钥可以删任何站。（P1-5 已填补：scopes 分级）

---

## 2. 差距分析（按 agent 旅程）

agent 使用 pageshare 的典型旅程：**发布 → 管理 → 观察 → 协作/治理**。

- 发布：远程通道已落地（P0-3 两阶段上传）；
- 管理：轻量的"只改设置不重传"缺失（约束 2，P0-1 已填补）；误发不可回滚（约束 3，P1-4 已填补：保留窗口 + 指针回滚）、想用自己认得的站点名而不是随机 8 位 id；
- 观察：访问统计已落地（P0-2）、到期预警空白（约束 4）；
- 治理：分级与审计已落地（约束 5，P1-5：scopes 门禁 + `audit.jsonl` 按密钥/agent/站点归因）；配额治理（每密钥限站点数/总字节）仍空白。

以下功能按此归类，P0 = 填补主流程断点，P1 = 明显增值，P2 = 锦上添花。

---

## 3. 功能设计

### P0-1 `update_site`：不重传改元数据（✅ 已实现）

**动机**：agent 最高频的"善后"操作——`ttl=72h` 到期前续期、给已发布的站加密码、改个名、补开 SPA 回退。现在全部要重传归档，既浪费带宽又涨版本号，还可能被并发发布覆盖。

**MCP 工具**：

```jsonc
// update_site
{
  "site_id": "ABCD1234",          // 必填
  "name":     "Q3 路演",           // 可选：出现即替换
  "ttl":      "72h",              // 可选："never"/"0" 清为永久；"72h" 从当前时刻重新起算
  "password": "",                 // 可选：出现即替换，空串清除；缺失不动
  "spa":      true                // 可选：出现即设置
}
// → { id, name, url, version, has_password, spa, expires_at }   version 不变
```

语义与现有 `PublishOpts` 的指针约定完全一致（缺失=不动，出现=替换），agent 的心智模型零迁移。

**HTTP**：`PATCH /api/v1/sites/{id}`，JSON body，字段同上（区分"缺失"与"显式 null"用键存在性）。给非 MCP 的脚本用，和 PUT 形成互补。

**实现落点**：
- store 加 `UpdateSiteMeta(id, name, passwordHash, ttl, spa)`——不插 version 行、不动 current_version；JSON/SQLite 两实现都是 UPDATE 语句级别的改动。密码哈希仍走 `hashPassword`（bcrypt）。
- 复用 `lockSite(id)` 与覆盖发布互斥，避免"改密码 vs 发布"竞态；站点锁内先 `GetSite` 再更新。
- `mcpserver.Backend` 加同名方法。

**注意**：
- 续期语义选"从当前时刻重新起算"而非"在剩余时长上叠加"，与首发 TTL 一致，agent 好预期。
- 过期站点能不能救回来？建议**不能**：sweeper 每小时物理删除，`update_site` 对已删站点返回"不存在"就是自然行为，不要特判。

**工作量**：S（一个下午，含测试）。

**实现注记（2026-09-27）**：与设计一致，落点为 `store.UpdateSiteMeta`（JSON/SQLite 双实现）、`server.updateSiteMeta`/`handlePatchSite`（持 `lockSite`，复用 `parseTTLField`）、MCP 工具 `update_site`（`internal/mcpserver/mcpserver.go`，校验 TTL/MaxTTL）。测试：`internal/store/store_test.go` `TestSiteMetaUpdateAndVisits`、`internal/server/updatesite_test.go`。

---

### P0-2 访问统计 + `get_site_stats`（✅ 已实现）

**动机**：agent 把链接发出去之后，唯一的反馈闭环是"发没发对人"。给 agent 一个可查询的信号：浏览量、最近访问时间，它能自己决定续期、重发还是撤销。这也是密钥池之外第一个"数据回流"能力。

**模型**：按**页览（pageview）**计数，不做到 IP 去重、不做referrer/UA 分析——agent 要的是"有没有人看"，不是 GA。避免滑向另一个产品。

**埋点**（`handleServeSite`，serve.go）：HTML 形态请求计一次——`rel` 为空、以 `/` 结尾、`.html/.htm` 结尾，以及 SPA 站点的无扩展名深链。静态资源（js/css/图）不计。密码站在过门之后才计（gate 页展示不算访问）。302 presign 与代理回源两条路径都在决策点之前，一处埋点即可。

**写放大控制**：访客请求路径上**只加内存计数器**（`map[siteID]{count,last}` + 互斥锁，模式同 `siteLocks`），后台 goroutine 每 1 分钟（含优雅退出时）把增量 `MergeVisits` 进元数据。崩溃最多丢最后一分钟的计数，可接受；绝不在请求路径同步落盘。

**数据面**：`Site` 加 `Visits int64` + `LastVisitedAt time.Time`；siteJSON 输出两字段（管理台列表顺带可看，属免费增值）。MCP 工具读的时候把内存未刷部分并上，保证刚发生的访问立刻可见：

```jsonc
// get_site_stats
{ "site_id": "ABCD1234" }        // 可选：缺省返回全部站点摘要
// → { site_id, name, url, live, visits, last_visit_at, version, updated_at, expires_at }
// 缺省时 → { sites: [...], total_sites, total_visits, busiest: {...} }
```

**防刷**：v1 不做限流（链接本来就是给"想看的人"的，刷量损害的是发布者的判断而非系统），但在设计上留钩子：计数器入口只有一个函数签名，将来加 per-IP 令牌桶不碰调用点。

**落点**：新文件 `internal/server/stats.go`（tracker + `StartStatsFlusher`，仿 `sweeper.go` 生命周期）；`main.go` 起停一处；store 两实现各加 `MergeVisits`。

**工作量**：M（埋点简单，难在两后端一致性 + 刷盘生命周期测试）。

**实现注记（2026-09-27）**：与设计一致，落点为 `internal/server/stats.go`（`visitTracker`/`isPageView`/`StartStatsFlusher`/`flushVisits`）、`serve.go` 埋点一处、`store.MergeVisits`（JSON/SQLite；SQLite 用 `MAX(last_visit_at, ?)` 只进不退，Unix 秒存储）、`siteJSON` 输出 `visits`/`last_visit_at`（读取时并 pending，实时可见）、MCP 工具 `get_site_stats`。`main.go` 接生命周期（ctx 取消时刷完最后增量）。测试：`internal/server/stats_test.go`、`internal/server/mcp_test.go`。

---

### P0-3 远程发布通道：两阶段上传（✅ 已实现）

**动机**：结构性约束 1。HTTP MCP 是"远程 agent"的主入口，但现在远程 agent 拿到 `publish_site` 会发现 `path` 没法填——它的文件在另一台机器上。**这是当前 agent 能力最大的断点**：没有它，密钥池/连接池这套远程基建只对"查删"有用，发布还得回到 curl。

**方案对比**：

| 方案 | 说明 | 否决/采纳 |
| --- | --- | --- |
| A. 内联 base64 | `publish_site` 加 `archive_base64` 字段 | 否决：JSON-RPC 消息过网关/代理的体积放大（1.37×）+ 必须整包驻留消息，撞 100MB 上限前客户端先炸 |
| B. 服务端拉取 URL | 加 `fetch_url` 字段，pageshare 去下载 | 否决（或最后）：SSRF——服务器替 agent 请求任意 URL，内网探测面打开；白名单/协议限制/体积限制/超时，安全成本比功能本身高 |
| C. 两阶段上传 | 申请票据 → HTTP PUT 原始字节 → 回执完成 | **采纳**：归档字节走普通 HTTP 体（复用现有上限/解析代码），MCP 通道只传 JSON 控制面 |

**接口**：

```jsonc
// MCP: begin_upload
{ "site_id": "ABCD1234",       // 可选：缺省=新建
  "expected_bytes": 5242880, "name": "...", "ttl": "72h", ... }
// → { upload_id, put_url, expires_in: 900 }

// HTTP 数据面（Bearer 鉴权，与 API 同一套）
PUT /api/v1/uploads/{upload_id}        ← 原始归档字节，Content-Type: application/zip|gzip

// MCP: commit_upload
{ "upload_id": "..." }                 // 服务端读暂存归档 → 走既有 publish 流水线
// → 与 publish_site 相同的返回
```

**设计要点**：
- 票据 = 随机 id + 过期时间（15 分钟）+ 配额预检（`expected_bytes ≤ max_upload_mb`），暂存目录独立于站点前缀，`commit` 后原子搬进正式版本前缀；过期票据由 sweeper 顺带清扫——**复用现有清理循环，不加新常驻任务**。
- `commit` 拿站点锁走 `publish()` 原路径，校验/解包/失败清理逻辑零复制。
- stdio 模式下 `begin_upload` 的 `put_url` 也成立（本机回环），三种上传方式（path / 两阶段 / curl）对 agent 并存，工具描述里写清选择条件。
- 存储抽象要加"暂存区"概念：mem/disk/S3 三个后端里 S3 用 `tmp/` 前缀 + multipart 或预签名 PUT（推荐预签名，字节流直接打桶，不过应用）。对 mem 后端就是另一个 map。**这是本功能最主要的复杂度**。

**工作量**：M-L（暂存生命周期 + 三后端；控制面本身简单）。

**实现注记（2026-09-27）**：采纳 C 方案，数据面统一走应用内 `PUT /api/v1/uploads/{id}`（暂存 key 前缀 `_uploads/`，站点 id 字母表不含下划线、永不冲突），三个后端零改造；未做 S3 预签名 PUT——统一通道换来的简单性大于绕过应用的带宽收益，且 mem/disk 本就没有预签名。票据表在内存（重启即失效，agent 重新 begin 一次工具调用即可）；`commit` 复用 `publishArchive()` 新抽函数（publish_site 与 commit 零复制，锁语义一致）；过期票据由 sweeper `expireUploads` 顺带清扫。**已知边界**：进程崩溃在 PUT 后、commit 前，`_uploads/` 对象成孤儿（Storage 接口无 List，无法按 mtime 扫盘）——窗口 ≤15 分钟且仅崩溃时发生，接受。测试：`internal/server/uploads_test.go`、`mcp_test.go` TestMCPTwoStageUpload。

---

### P1-4 版本保留与回滚（✅ 已实现）

**动机**：agent 会犯错——覆盖发布了一个白屏版本，链接已经发出去了。现在版本 N 的对象在 N+1 成功瞬间物理删除，不可逆。

**设计**：
- 配置项 `versions_kept`（默认 1 = 现行为；0 = 全保留，慎用；建议 3）。发布成功后**延迟删除**：把超出保留数的最老版本对象删掉，而不是立即删前一版。
- store 的 `versions` 行本就留存（现在只是没用来找对象），加 `deleted_at` 标记或直接以 `current_version` 与保留窗口推导哪些版本还在。
- MCP 工具：`list_site_versions(site_id)` → 版本号/时间/文件数/字节数；`rollback_site(site_id, version)` → 校验版本对象还在 → 原子切 `current_version`（访问路径按 current_version 拼前缀，切指针即完成发布回退，对象无需搬移）。`update_site` 之外再一个"不动内容动指针"的操作。
- 注意与 P0-2 无关但共享锁协议：回滚也要 `lockSite`。

**成本**：磁盘/R2 占用 ×保留数。文档要写明这是对"AI 误操作率"的保险，不是历史归档功能（TTL 到期清扫仍会全删）。

**工作量**：M。依赖对 `sitePrefix` 版本语义的小心梳理，测试重点是删除时机。

**实现注记（2026-09-27）**：删除时机用"单调水位线 `Site.PrunedTo`"表达（v ≤ 水位的对象已物理删除），`versions` 行永不删、只作账本——比每行加 `deleted_at` 少一份状态，prune 也能整体跳过已删区间。prune 在 publish 成功后、响应前执行：删 `(PrunedTo, newVersion-versions_kept]` 区间内除 `CurrentVersion` 外的版本前缀（回滚后指针可能低于最新，当前版永不删），逐版尽力删后统一 `MarkPrunedTo` 推进水位（个别失败留孤儿对象，站点删除/TTL 清扫兜底）。回滚 = `lockSite` 下校验（版本行存在、`version > PrunedTo`、非当前版）后原子 `SetCurrentVersion`，不搬对象、链接不变。回滚后再发布的新版本号取 `max(CurrentVersion, 版本账本最高)+1`（`nextPublishVersion`），避免与保留中的版本撞前缀。保守边界：`versions_kept=1` 时代遗留的旧版行在、对象已没了，水位不区分来源，回滚一律被拒——拒绝方向是安全的。**已知边界**：TTL 到期与 `delete_site` 仍整站前缀全删，窗口只在站点活跃期兜误发。测试：`store_test.go` TestVersionLedgerAndPruneWatermark（双后端：升序/副本/ErrNotFound/水位单调/持久化）、`versions_test.go`（kept=3 窗口删除时机、kept=0 全保留、kept=1 即删=现行、回滚切指针+服务立即可见、回滚后发布版本号跳 4、四类校验错误）、`mcp_test.go` 6.5 步全链路 + stdio 清单 10 工具。

---

### P1-5 密钥分级 + 操作审计（✅ 已实现）

**动机**：给 Claude Desktop 的密钥和给 CI 流水线的密钥权限相同——CI 那把泄了能删光所有站。连接池已经登记"谁在线"，但"谁干了什么"没有落盘。

**设计**：
- `MCPKey` 加 `scopes []string`：`publish`（建+覆盖+update+rollback）/ `read`（list/get/stats）/ `delete`。签发时指定，缺省 `publish,read`（不含 delete——删除保持显式授权）。工具 handler 从连接上下文取当前密钥做门禁；stdio 模式（配置文件即凭据）视为全 scope。
- 操作日志 `audit.jsonl`（滚动，上限如 10MB×5）：时间、密钥 id、agent 名、工具名、站点 id、结果。JSON 文件起步即可，不入库。管理 API `GET /api/v1/mcp/audit?site=&key=&since=`。
- 速率限制顺手做在这里：每密钥滑动窗口（如 60 req/min，publish 类 10 次/min），超限返回 MCP error + `Retry-After`。防止跑飞的 agent 刷爆对象存储请求费。

**工作量**：S-M。scope 传递路径实现前已验证：go-sdk v1.8.0 的接收中间件能在 `tools/call` 上拿到原始 HTTP 头（见实现注记）。

**实现注记（2026-09-27）**：治理落点不是十个工具 handler，而是 `mcp.Server.AddReceivingMiddleware` 的单一拦截——已验证 v1.8.0 的 streamable `servePOST` 把原始请求头塞进 `RequestExtra.Header`（`CallToolRequest = ServerRequest[*CallToolParamsRaw]`），stdio/InMemory 则为 nil。**身份只认原始 `Authorization` 头，不依赖会话上下文**（streamable 会话跨多个 HTTP POST，ctx 不是可靠的每次请求载体）。`Authorize` 是外层 `mcpAuth` 之后的第二道防线：scope 门禁（`toolRequiredScope` 映射，未知名按无权限工具放行给 schema 层）+ 每密钥双滑动窗口限流（总量 60/min、发布类 10/min，纯内存、惰性清理，重启清零——减震器不是账本）；管理 token、stdio（nil 头）与无凭据（交给外层 401）均放行，被拒的调用以 `IsError` 工具结果返回并点名缺的 scope。兼容策略：`MCPKey.Scopes` 为空 = 全权限——升级前签发的存量密钥保持原行为、不被静默断权；新签发缺省 `publish,read`（delete 永不默认），未知 scope 名签发 API 直接 400。审计刻意**文件不入库**（追加写不碰站点元数据锁、崩溃最多丢尾部一行、运维可 `tail/grep`）：`audit.jsonl` 落 DBPath 同目录、0600、10MB×5 滚动，查询跨文件新→旧合并（site 小写匹配/key 认名称或 id/since RFC3339），agent 归因走连接池反查 `Mcp-Session-Id`，**门禁拒绝的调用不落账**（账本记的是被信任后发生的事），写失败绝不反向卡死工具调用。测试：`gate_test.go`——Authorize 矩阵（stdio/管理 token/空 scopes 存量/只读拒写删/组合密钥/吊销与陌生凭据/经门禁触发限流）、`keyLimiter` 双窗口与密钥隔离、`auditWriter` 过滤与滚动跨文件查询、真实 HTTP streamable 端到端（签发带 scopes → initialize → `get_site` 放行且落审计含 agent 归因 → `publish_site` 被拒不落账）；store 层 scopes 双后端 round-trip + 持久化。

---

### P1-6 命名发布（slug）

**动机**：agent 的记忆里没有稳定键。同一个报告反复发布得到不同 id，agent 下次只能 `list_sites` 再按名字模糊找。

**设计**：`publish_site` 加可选 `slug`（`[a-z0-9][a-z0-9-]{1,31}`），占用则**覆盖**该 slug 的站点（幂等 upsert），被占用的判定在服务端建站锁内完成。URL 从 `/s/{id}/` 变成 `/s/{slug}/`——路由层先按 slug 表查、miss 再走 8 位 id 规则，泛域名同理（slug 天然 DNS-safe）。`sites` 表加唯一索引；删除站点时 slug 释放但保留冷却（防"接管"已删站语义，可后置）。

**注意**：slug 是**用户可猜空间**，密码站撞名风险、以及"发一个 slug 恰好和别人的相似"的抢注问题，比随机 id 高——建议 slug 发布需要 `publish` scope 且保留管理员强制接管 API。

**工作量**：M（路由/两处 URL 生成/兼容性）。可放最后做，P0/P1 都不依赖它。

---

### P2 一览（暂不展开）

| 功能 | 一句话 | 前置 |
| --- | --- | --- |
| 到期预警 / 事件订阅 | 密钥上配 webhook，站点将过期/被大量访问时回调；或 MCP polling 游标 `check_events(since)` | P0-2 的数据 |
| sites 作为 MCP resources | 客户端可 @引用站点元数据，免一次工具调用 | 无 |
| 访客会话吊销 | 密码站的 HMAC 会话加代际号，agent 可"踢出所有已进门的访客" | 无 |
| 草稿/转正 | 先发到 preview 链接，agent 自查渲染后再 promote 成正式 URL | P1-4 的指针切换 |
| 配额治理 | 按密钥限站点数/总字节，防跑飞 agent 占桶 | P1-5 |

---

## 4. 工具数量与上下文预算的权衡

MCP 工具集是直接进 agent 系统提示的 token 成本。现在 4 个工具，全加完是 11 个（约 +1.5k token schema）——尚可，但要克制：

- `list_site_versions` 与 `get_site` 可合并（get 带 `include_versions=true`）；
- `begin_upload/commit_upload` 是两个，但换来远程发布主流程，值得；
- `update_site` 和 `publish_site` **不建议合并**：合并后 path 变可选、参数校验分支翻倍，工具描述反而更长更难被 agent 用对。
- 用 server `instructions` 字段写一段"怎么选工具"的决策树，比堆工具描述有效。

## 5. 建议实施顺序

```
第一批（主流程断点，均为发布后行为可控）:
  P0-1 update_site          S      ── ✅ 已实现（store.UpdateSiteMeta / PATCH / update_site 工具）
  P0-2 访问统计 + stats      M      ── ✅ 已实现（stats.go / MergeVisits / get_site_stats 工具）
第二批（远程 agent 补齐）:
  P0-3 两阶段上传            M-L    ── ✅ 已实现（uploads.go / begin_upload+commit_upload）
第三批（安全网与治理）:
  P1-4 版本保留回滚          M      ── ✅ 已实现（versions.go / PrunedTo 水位 / rollback_site 工具）
  P1-5 密钥 scope+审计       S-M    ── ✅ 已实现（gate.go / audit.go / 接收中间件）
第四批（体验）:
  P1-6 slug → P2 按反馈取用
```

每批落完更新 README「MCP」章节与本文档状态标记（设计 → 已实现 + 指向代码文件）。

---

*本文档由 2026-09-27 代码盘点产生；文中文件路径与行为描述对应当时 HEAD，实现前请以代码为准复核。*
