// Package mcpserver 把 pageshare 的发布/管理能力暴露为 MCP 工具，
// 供任何 MCP 客户端（Harness、Claude Desktop、Cursor、pi-agent…）直连：
//   - stdio：./pageshare mcp -config pageshare.json
//   - HTTP：/mcp 端点（Streamable HTTP，Bearer 鉴权）
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"pageshare/internal/site"
)

// Backend 是 MCP 工具需要的最小服务能力（*server.Server 实现）。
type Backend interface {
	PublishSite(ctx context.Context, existingID string, opts ServerPublishOpts) (map[string]any, error)
	BeginUpload(ctx context.Context, siteID string, opts ServerUploadOpts, expectedBytes int64) (map[string]any, error)
	CommitUpload(ctx context.Context, uploadID string) (map[string]any, error)
	UpdateSite(ctx context.Context, id string, opts ServerUpdateOpts) (map[string]any, error)
	SiteStatsJSON(ctx context.Context, siteID string) (map[string]any, error)
	ListSiteVersions(ctx context.Context, siteID string) ([]map[string]any, error)
	RollbackSite(ctx context.Context, siteID string, version int64) (map[string]any, error)
	ListSitesJSON(ctx context.Context) ([]map[string]any, error)
	DeleteSiteJSON(ctx context.Context, id string) error

	// Authorize 是工具调用前的治理门禁：解析 Authorization 头对应的密钥身份，
	// 校验该密钥对 tool 是否有 scope、是否触发限流。返回 error 即拒绝（消息含 Retry-After）。
	// header 为 nil（stdio / InMemory）视为本地可信 = 全权、不限流。
	Authorize(header http.Header, tool string) error
	// RecordAudit 在工具调用后落一行操作审计（args 为原始 JSON 参数，outcome 是结果摘要：
	// "ok" / "error: ..."）。无论成败都记。
	RecordAudit(header http.Header, tool string, args json.RawMessage, outcome string)
}

// serverPublishOpts 是跨包的发布参数（避免暴露 server 内部类型）。
type ServerPublishOpts struct {
	Name     string
	TTL      *time.Duration
	Password *string
	SPAFalls *bool
	Body     []byte
}

// ServerUploadOpts 是 begin_upload 携带的发布参数（指针语义同 ServerUpdateOpts）。
type ServerUploadOpts struct {
	Name     string
	TTL      *time.Duration
	Password *string
	SPAFalls *bool
}

// ServerUpdateOpts 是跨包的元数据修改参数：指针字段 nil = 不改动；
// Password 空串 = 清除密码；TTL 0 = 清为永久（新 TTL 从当前时刻起算）。
type ServerUpdateOpts struct {
	Name     *string
	TTL      *time.Duration
	Password *string
	SPA      *bool
}

// Config 是 MCP 工具运行所需配置。
type Config struct {
	MaxUploadMB int64
}

// ---- 工具入参/出参（结构体字段即 JSON Schema） ----

type publishIn struct {
	// path 是站点目录或 zip/tar.gz 归档路径（在 pageshare 所在机器上）
	Path string `json:"path" jsonschema:"站点目录或 zip/tar.gz 归档的本地路径（必填）"`
	// siteID 提供时为覆盖更新该站点；缺省为新建
	SiteID string `json:"site_id,omitempty" jsonschema:"要覆盖更新的站点 id（8 位）；缺省为新建站点"`
	Name   string `json:"name,omitempty" jsonschema:"站点名称（新建时显示用）"`
	TTL    string `json:"ttl,omitempty" jsonschema:"有效期，如 72h、7d；缺省永久（覆盖时缺省为不变）"`
	// password 覆盖时传空串即清除密码；指针语义：字段缺失表示不变，出现（含空串）表示替换
	Password *string `json:"password,omitempty" jsonschema:"访问密码；覆盖时传空串即清除"`
	SPA      *bool   `json:"spa,omitempty" jsonschema:"SPA 站点：路由未命中回退入口页（客户端路由勾选）"`
}

type publishOut struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Version int64  `json:"version"`
	Name    string `json:"name"`
}

type listOut struct {
	Sites []map[string]any `json:"sites"`
}

type idIn struct {
	SiteID string `json:"site_id" jsonschema:"站点 id（8 位，大小写不敏感）"`
}

type deletedOut struct {
	Deleted bool   `json:"deleted"`
	ID      string `json:"id"`
}

// updateIn 是 update_site 的入参：指针/可选字段缺失 = 不改动。
type updateIn struct {
	SiteID string `json:"site_id" jsonschema:"站点 id（8 位，必填）"`
	// Name 出现即替换站点名
	Name *string `json:"name,omitempty" jsonschema:"新站点名；缺省不改"`
	// TTL 出现即从当前时刻重新起算；"never"/"0" 清为永久
	TTL string `json:"ttl,omitempty" jsonschema:"新有效期，如 72h、7d、never（从当前时刻重新起算）；缺省不改"`
	// Password 出现即替换，空串清除；缺失不动
	Password *string `json:"password,omitempty" jsonschema:"新访问密码；传空串清除"`
	// SPA 出现即设置/清除回退开关
	SPAFalls *bool `json:"spa,omitempty" jsonschema:"SPA 回退开关；缺省不改"`
}

// statsIn 是 get_site_stats 的入参：site_id 缺省汇总全部站点。
type statsIn struct {
	SiteID string `json:"site_id,omitempty" jsonschema:"站点 id；缺省返回全部站点统计与汇总"`
}

// rollbackIn 是 rollback_site 的入参。
type rollbackIn struct {
	SiteID  string `json:"site_id" jsonschema:"站点 id（8 位，必填）"`
	Version int64  `json:"version" jsonschema:"回滚到的版本号（来自 list_site_versions）"`
}

// versionsOut 是 list_site_versions 的出参。
type versionsOut struct {
	SiteID   string           `json:"site_id"`
	Versions []map[string]any `json:"versions"`
}

// beginIn 是 begin_upload 的入参。
type beginIn struct {
	// SiteID 提供时票据绑定覆盖该站点；缺省为新建
	SiteID        string  `json:"site_id,omitempty" jsonschema:"要覆盖更新的站点 id（8 位）；缺省为新建"`
	ExpectedBytes int64   `json:"expected_bytes,omitempty" jsonschema:"预计归档字节数（可选，用于配额预检）"`
	Name          string  `json:"name,omitempty" jsonschema:"站点名称（新建时显示用）"`
	TTL           string  `json:"ttl,omitempty" jsonschema:"有效期，如 72h、7d；缺省永久"`
	Password      *string `json:"password,omitempty" jsonschema:"访问密码；覆盖时传空串即清除"`
	SPAFalls      *bool   `json:"spa,omitempty" jsonschema:"SPA 站点回退开关"`
}

// beginOut 是 begin_upload 的出参：拿 put_url 直接 HTTP PUT 归档字节，再 commit_upload。
type beginOut struct {
	UploadID  string `json:"upload_id"`
	PutURL    string `json:"put_url"`
	ExpiresIn int64  `json:"expires_in"`
	SiteID    string `json:"site_id,omitempty"`
}

// commitIn 是 commit_upload 的入参。
type commitIn struct {
	UploadID string `json:"upload_id" jsonschema:"begin_upload 返回的 upload_id（必填）"`
}

// ---- 装配 ----

// governanceMiddleware 拦截每个 tools/call：调用前过 Backend.Authorize（scope 门禁 + 每密钥限流），
// 调用后过 Backend.RecordAudit（操作落盘，成败都记）。stdio/InMemory 传输没有 HTTP 头
// （GetExtra().Header 为 nil），后端按"本地可信 = 全权、不限流，审计记 stdio"处理。
func governanceMiddleware(b Backend) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			params, isCall := req.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !isCall {
				return next(ctx, method, req)
			}
			var header http.Header
			if extra := req.GetExtra(); extra != nil {
				header = extra.Header
			}
			if err := b.Authorize(header, params.Name); err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
					&mcp.TextContent{Text: "pageshare 拒绝了这次工具调用：" + err.Error()},
				}}, nil
			}
			res, err := next(ctx, method, req)
			outcome := "ok"
			if err != nil {
				outcome = "error: " + err.Error()
			} else if ctr, ok := res.(*mcp.CallToolResult); ok && ctr.IsError {
				text := ""
				for _, c := range ctr.Content {
					if tc, ok := c.(*mcp.TextContent); ok {
						text += tc.Text
					}
				}
				outcome = "tool_error: " + text
			}
			b.RecordAudit(header, params.Name, params.Arguments, outcome)
			return res, err
		}
	}
}

// NewServer 组装 MCP server（stdio 与 HTTP 共用）。
func NewServer(b Backend, cfg Config, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "pageshare", Version: version}, nil)
	s.AddReceivingMiddleware(governanceMiddleware(b))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "publish_site",
		Description: "发布或覆盖一个静态站点：传入本地目录（自动打包 tar.gz）或 zip/tar.gz 归档路径，返回分享链接。目录里的 .git/node_modules 等会被自动跳过。注意：path 必须位于 pageshare 进程所在机器；远程 agent（HTTP 连接）请改用 begin_upload → HTTP PUT → commit_upload 两阶段通道。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in publishIn) (*mcp.CallToolResult, publishOut, error) {
		if in.Path == "" {
			return nil, publishOut{}, fmt.Errorf("path 不能为空")
		}
		body, err := site.PackDirectory(in.Path)
		if err != nil {
			return nil, publishOut{}, fmt.Errorf("读取 %s 失败: %w", in.Path, err)
		}
		if cfg.MaxUploadMB > 0 && int64(len(body)) > cfg.MaxUploadMB<<20 {
			return nil, publishOut{}, fmt.Errorf("归档 %d 字节超过上限 %d MB", len(body), cfg.MaxUploadMB)
		}
		opts := ServerPublishOpts{Name: in.Name, Body: body}
		if in.TTL != "" {
			d, err := site.ParseTTL(in.TTL)
			if err != nil {
				return nil, publishOut{}, err
			}
			if d > site.MaxTTL {
				return nil, publishOut{}, fmt.Errorf("ttl 最长 %s", site.MaxTTL)
			}
			opts.TTL = &d
		}
		opts.Password = in.Password // nil=不变；空串即清除
		opts.SPAFalls = in.SPA
		out, err := b.PublishSite(ctx, in.SiteID, opts)
		if err != nil {
			return nil, publishOut{}, err
		}
		return nil, publishOut{
			ID:      asString(out["id"]),
			URL:     asString(out["url"]),
			Version: asInt64(out["version"]),
			Name:    asString(out["name"]),
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "begin_upload",
		Description: "两阶段上传第一步（远程 agent 发布入口，替代 publish_site 的本地 path）：签发票据，返回 put_url；把归档字节（zip/tar.gz）HTTP PUT 到 put_url（Authorization 用你的 MCP 密钥或管理 token），然后调 commit_upload 完成发布。票据 15 分钟过期。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in beginIn) (*mcp.CallToolResult, beginOut, error) {
		opts := ServerUploadOpts{Name: in.Name}
		if in.TTL != "" {
			d, err := site.ParseTTL(in.TTL)
			if err != nil {
				return nil, beginOut{}, err
			}
			if d > site.MaxTTL {
				return nil, beginOut{}, fmt.Errorf("ttl 最长 %s", site.MaxTTL)
			}
			opts.TTL = &d
		}
		opts.Password = in.Password
		opts.SPAFalls = in.SPAFalls
		out, err := b.BeginUpload(ctx, in.SiteID, opts, in.ExpectedBytes)
		if err != nil {
			return nil, beginOut{}, err
		}
		return nil, beginOut{
			UploadID:  asString(out["upload_id"]),
			PutURL:    asString(out["put_url"]),
			ExpiresIn: asInt64(out["expires_in"]),
			SiteID:    asString(out["site_id"]),
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "commit_upload",
		Description: "两阶段上传第二步：归档已 PUT 到 put_url 后调用，服务端读取暂存并走与 publish_site 相同的发布流水线，返回与 publish_site 一致的结果（id/url/version/name）。upload_id 一次性使用。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in commitIn) (*mcp.CallToolResult, publishOut, error) {
		if in.UploadID == "" {
			return nil, publishOut{}, fmt.Errorf("upload_id 不能为空")
		}
		out, err := b.CommitUpload(ctx, in.UploadID)
		if err != nil {
			return nil, publishOut{}, err
		}
		return nil, publishOut{
			ID:      asString(out["id"]),
			URL:     asString(out["url"]),
			Version: asInt64(out["version"]),
			Name:    asString(out["name"]),
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sites",
		Description: "列出全部已发布站点（id、名称、分享链接、版本、有效期等）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOut, error) {
		sites, err := b.ListSitesJSON(ctx)
		if err != nil {
			return nil, listOut{}, err
		}
		if sites == nil {
			sites = []map[string]any{}
		}
		return nil, listOut{Sites: sites}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_site",
		Description: "查询单个站点详情。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, map[string]any, error) {
		if in.SiteID == "" {
			return nil, nil, fmt.Errorf("site_id 不能为空")
		}
		sites, err := b.ListSitesJSON(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, sj := range sites {
			if equalID(asString(sj["id"]), in.SiteID) {
				return nil, sj, nil
			}
		}
		return nil, nil, fmt.Errorf("站点 %s 不存在", in.SiteID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_site",
		Description: "吊销并删除站点：分享链接立即失效，对象存储内容一并清除，不可恢复。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, deletedOut, error) {
		if in.SiteID == "" {
			return nil, deletedOut{}, fmt.Errorf("site_id 不能为空")
		}
		if err := b.DeleteSiteJSON(ctx, in.SiteID); err != nil {
			return nil, deletedOut{}, err
		}
		return nil, deletedOut{Deleted: true, ID: in.SiteID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "update_site",
		Description: "不重传地修改站点设置：name/ttl/password/spa 按出现与否逐字段生效，version 不变。TTL 从当前时刻重新起算（" +
			"never 清为永久；password 空串清除）。适合到期续期、事后加密码/开关 SPA、改名。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateIn) (*mcp.CallToolResult, map[string]any, error) {
		if in.SiteID == "" {
			return nil, nil, fmt.Errorf("site_id 不能为空")
		}
		if in.Name == nil && in.TTL == "" && in.Password == nil && in.SPAFalls == nil {
			return nil, nil, fmt.Errorf("name/ttl/password/spa 至少提供一个")
		}
		opts := ServerUpdateOpts{Name: in.Name, Password: in.Password, SPA: in.SPAFalls}
		if in.TTL != "" {
			d, err := site.ParseTTL(in.TTL)
			if err != nil {
				return nil, nil, err
			}
			if d > site.MaxTTL {
				return nil, nil, fmt.Errorf("ttl 最长 %s", site.MaxTTL)
			}
			opts.TTL = &d
		}
		out, err := b.UpdateSite(ctx, in.SiteID, opts)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_site_stats",
		Description: "查询访问统计：页览数（visits）、最近访问时间、链接是否存活（live）。site_id 缺省返回全部站点明细与汇总（total_visits、busiest）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in statsIn) (*mcp.CallToolResult, map[string]any, error) {
		out, err := b.SiteStatsJSON(ctx, in.SiteID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_site_versions",
		Description: "列出站点的发布版本历史：版本号、时间、文件数、字节数，并标注 current（当前版）与 restorable（对象仍在保留窗口内、可回滚）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, versionsOut, error) {
		if in.SiteID == "" {
			return nil, versionsOut{}, fmt.Errorf("site_id 不能为空")
		}
		vers, err := b.ListSiteVersions(ctx, in.SiteID)
		if err != nil {
			return nil, versionsOut{}, err
		}
		return nil, versionsOut{SiteID: in.SiteID, Versions: vers}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "rollback_site",
		Description: "把站点当前版本原子切回一个仍在保留窗口内的历史版本（versions_kept 决定窗口宽度）：" +
			"不重传内容、链接不变、立即生效；version 必须来自 list_site_versions 且 restorable=true。回滚后再 publish_site 会分配更高的新版本号。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in rollbackIn) (*mcp.CallToolResult, map[string]any, error) {
		if in.SiteID == "" {
			return nil, nil, fmt.Errorf("site_id 不能为空")
		}
		if in.Version < 1 {
			return nil, nil, fmt.Errorf("version 必须为正整数")
		}
		out, err := b.RollbackSite(ctx, in.SiteID, in.Version)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	return s
}

// NewStreamableHTTP 返回挂到 HTTP 服务的 /mcp 处理器（由调用方包鉴权）。
func NewStreamableHTTP(b Backend) http.Handler {
	s := NewServer(b, Config{}, MCPVersion())
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}

// RunStdio 以 stdio 传输运行 MCP server（本地客户端拉起本进程时使用）。
func RunStdio(ctx context.Context, b Backend, cfg Config, version string) error {
	s := NewServer(b, cfg, version)
	return s.Run(ctx, &mcp.StdioTransport{})
}

// MCPVersion 返回传给 MCP 客户端的版本号。
func MCPVersion() string { return "1.0.0" }

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func equalID(a, b string) bool { return strings.EqualFold(a, b) }
