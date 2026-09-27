// Package config 装配 pageshare 的运行配置：默认值 → JSON 配置文件 → 环境变量覆盖。
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Storage 是对象存储配置；backend 取 "s3" 或 "mem"。
// s3 后端凭标准 S3 兼容 API 工作（AWS S3 / Cloudflare R2 / MinIO）。
type Storage struct {
	Backend         string `json:"backend"`  // "s3" | "disk" | "mem"
	Root            string `json:"root"`     // disk 后端的本地目录
	Endpoint        string `json:"endpoint"` // 可选，R2/MinIO 需要；留空用 AWS 默认
	Region          string `json:"region"`   // R2 常填 "auto"
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"` // 可选，桶内全局 key 前缀，如 "pageshare/"
}

// Config 是服务完整配置。
type Config struct {
	Listen           string `json:"listen"`             // 如 ":8300"
	PublicBaseURL    string `json:"public_base_url"`    // 对外分享链接的基地址，如 https://share.example.com
	SiteWildcardHost string `json:"site_wildcard_host"` // 可选，如 s.example.com：站点按 {id}.s.example.com 整域托管（nginx 语义，Vite base:/ 产物零改动）
	AuthToken        string `json:"auth_token"`         // 管理面 Bearer token，必填
	CookieSecret     string `json:"cookie_secret"`      // 密码门会话 cookie 的 HMAC 密钥，必填
	DBPath           string `json:"db_path"`            // 站点元数据文件：.json（默认，如 sites.json）或 .db（SQLite 兼容）
	MaxUploadMB      int64  `json:"max_upload_mb"`      // 上传压缩体上限，默认 100
	// VersionsKept 覆盖发布保留的版本数（含当前版）：1=现行行为（发布成功即删旧版），
	// 2..N 保留近 N 版可回滚，0=全保留（慎用，TTL 到期清扫仍会全删）。
	VersionsKept int64   `json:"versions_kept"`
	Storage      Storage `json:"storage"`

	// shareScheme 由 public_base_url 推导（泛域名分享链接用），不经 JSON。
	shareScheme string `json:"-"`

	// Dev 模式（-dev）：mem 存储 + 随机 token，只用于本地试跑与 e2e。
	Dev bool `json:"-"`

	// ConfigPath 是本次实际使用的配置文件路径（-config 的值，未传则为默认 pageshare.json），
	// 供引导模式写入配置，不经 JSON。
	ConfigPath string `json:"-"`

	// NeedsSetup 为 true 表示进入首次使用引导模式：非 -dev 且 token/cookie 密钥均为空，
	// validate 会放行这三项空值，待向导完成后落盘正式配置，不经 JSON。
	NeedsSetup bool `json:"-"`
}

// Defaults 返回一份可直接使用的基线配置。
func Defaults() Config {
	return Config{
		Listen:        ":8300",
		PublicBaseURL: "http://127.0.0.1:8300",
		DBPath:        "sites.json",
		MaxUploadMB:   100,
		VersionsKept:  1,
		Storage:       Storage{Backend: "mem", Prefix: ""},
	}
}

// defaultConfigPath 是未传 -config 时的默认配置文件路径。
const defaultConfigPath = "pageshare.json"

// Load 按优先级装配配置：内置默认 → -config 指定的 JSON → 环境变量 → 命令行标志。
// 未传 -config 时尝试读取默认 pageshare.json：不存在不算错误（由此进入引导模式），
// 存在但解析失败则报错。
func Load() (Config, error) {
	cfg := Defaults()

	var (
		configPath string
		dev        bool
	)
	flag.StringVar(&configPath, "config", "", "JSON 配置文件路径")
	flag.BoolVar(&dev, "dev", false, "开发模式：mem 存储 + 随机 token/cookie 密钥")
	flag.Parse()
	cfg.Dev = dev
	cfg.ConfigPath = configPath
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = defaultConfigPath
	}

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return cfg, fmt.Errorf("读取配置文件: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("解析配置文件 %s: %w", configPath, err)
		}
	} else if raw, err := os.ReadFile(cfg.ConfigPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("解析配置文件 %s: %w", cfg.ConfigPath, err)
		}
	}
	applyEnv(&cfg)

	if cfg.Dev {
		// -dev：未提供固定 token/cookie 密钥时自动生成（与存储后端无关）
		if strings.TrimSpace(cfg.AuthToken) == "" {
			cfg.AuthToken = randomHex(16)
			cfg.CookieSecret = randomHex(32)
		}
	}

	// 引导模式判定：非 -dev 且 token/cookie 密钥均空（经 env 覆盖后）。
	// 此时 validate 放行必填三项，等向导完成后落盘正式配置。
	cfg.NeedsSetup = !cfg.Dev &&
		strings.TrimSpace(cfg.AuthToken) == "" &&
		strings.TrimSpace(cfg.CookieSecret) == ""

	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	envStr := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	envStr("PS_LISTEN", &cfg.Listen)
	envStr("PS_PUBLIC_BASE_URL", &cfg.PublicBaseURL)
	envStr("PS_SITE_WILDCARD_HOST", &cfg.SiteWildcardHost)
	envStr("PS_AUTH_TOKEN", &cfg.AuthToken)
	envStr("PS_COOKIE_SECRET", &cfg.CookieSecret)
	envStr("PS_DB_PATH", &cfg.DBPath)
	envStr("PS_S3_ENDPOINT", &cfg.Storage.Endpoint)
	envStr("PS_STORAGE_ROOT", &cfg.Storage.Root)
	envStr("PS_S3_REGION", &cfg.Storage.Region)
	envStr("PS_S3_BUCKET", &cfg.Storage.Bucket)
	envStr("PS_S3_ACCESS_KEY_ID", &cfg.Storage.AccessKeyID)
	envStr("PS_S3_SECRET_ACCESS_KEY", &cfg.Storage.SecretAccessKey)
	envStr("PS_S3_PREFIX", &cfg.Storage.Prefix)
	if v := os.Getenv("PS_STORAGE_BACKEND"); v != "" {
		cfg.Storage.Backend = v
	}
	if v := os.Getenv("PS_VERSIONS_KEPT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.VersionsKept = n
		}
	}
}

func (c *Config) validate() error {
	if c.VersionsKept < 0 {
		return errors.New("versions_kept 不能为负（1=即删旧版，0=全保留）")
	}
	if !c.NeedsSetup {
		// 引导模式下这三项由向导补齐，先放行空值
		if strings.TrimSpace(c.AuthToken) == "" {
			return errors.New("auth_token 不能为空（-config、PS_AUTH_TOKEN 或 -dev 提供其一）")
		}
		if strings.TrimSpace(c.CookieSecret) == "" {
			return errors.New("cookie_secret 不能为空（-config、PS_COOKIE_SECRET 或 -dev 提供其一）")
		}
		if c.PublicBaseURL == "" {
			return errors.New("public_base_url 不能为空：分享链接以它拼接")
		}
	}
	c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	c.SiteWildcardHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.SiteWildcardHost), "."))
	if c.SiteWildcardHost != "" && strings.Contains(c.SiteWildcardHost, "/") {
		return errors.New("site_wildcard_host 只填主机名（如 s.example.com），不带协议和路径")
	}
	if c.SiteWildcardHost != "" {
		if u, err := url.Parse(c.PublicBaseURL); err == nil {
			c.shareScheme = u.Scheme
		}
		if c.shareScheme == "" {
			c.shareScheme = "https"
		}
	}
	switch c.Storage.Backend {
	case "s3":
		if c.Storage.Bucket == "" {
			return errors.New("storage.bucket 不能为空（s3 后端）")
		}
	case "disk":
		if strings.TrimSpace(c.Storage.Root) == "" {
			return errors.New("storage.root 不能为空（disk 后端）")
		}
	case "mem":
	default:
		return fmt.Errorf("storage.backend 只支持 s3 | disk | mem，得到 %q", c.Storage.Backend)
	}
	return nil
}

// Validate 校验并规范化配置；引导完成时也用它对向导提交的配置做兜底校验。
func (c *Config) Validate() error { return c.validate() }

// Save 将配置序列化为两个空格缩进的 JSON 写入 path：
// 先写临时文件再 rename，保证原子性；文件含密钥，权限收窄到 0600。
func (c Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时配置文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("落盘配置文件: %w", err)
	}
	return nil
}

// MaxUploadBytes 返回上传体字节数上限。
func (c Config) MaxUploadBytes() int64 { return c.MaxUploadMB << 20 }

// SiteURL 返回某站点的对外分享链接：配了泛域名用 {id}.host 整域，否则用子路径。
func (c Config) SiteURL(siteID string) string {
	id := strings.ToLower(siteID)
	if c.SiteWildcardHost != "" {
		return c.shareScheme + "://" + id + "." + c.SiteWildcardHost + "/"
	}
	return c.PublicBaseURL + "/s/" + id + "/"
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := cryptorandRead(b); err != nil {
		panic(err)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, nBytes*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0xf]
	}
	return string(out)
}

// RandomHex 返回 n 字节密码学随机数的十六进制串（小写），供引导模式生成
// 引导码、建议 token 与 cookie 密钥。
func RandomHex(n int) string { return randomHex(n) }
