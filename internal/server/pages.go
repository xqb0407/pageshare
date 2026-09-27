package server

import (
	"html/template"
	"net/http"
)

// publicPage 输出与前端主题同款的提示页（404/410 等）。
func (s *Server) publicPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(statusHTML(status, msg)))
}

// baseStyle 复刻前端 index.css 的视觉语言：暖石色环境光晕 + 磨砂卡片 +
// 轻字重标题 + 墨色胶囊按钮；亮暗色跟随系统。
const baseStyle = `
<style>
  :root {
    color-scheme: light dark;
    --ps-bg: #fff;
    --ps-fg: oklch(0.205 0 0);
    --ps-card: #fff;
    --ps-muted: oklch(0.556 0 0);
    --ps-border: oklch(0.922 0 0);
    --ps-primary: oklch(0.205 0 0);
    --ps-primary-fg: oklch(0.985 0 0);
    --ps-destructive: oklch(0.577 0.245 27.325);
    --ps-card-shadow: 0 0 0 0.5px oklch(0 0 0 / 0.05),
      0 6px 16px oklch(0.42 0.04 70 / 0.07), 0 1px 3px oklch(0.42 0.04 70 / 0.04);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --ps-bg: oklch(0.145 0 0);
      --ps-fg: oklch(0.985 0 0);
      --ps-card: oklch(0.205 0 0);
      --ps-muted: oklch(0.708 0 0);
      --ps-border: oklch(1 0 0 / 10%);
      --ps-primary: oklch(0.922 0 0);
      --ps-primary-fg: oklch(0.205 0 0);
      --ps-destructive: oklch(0.704 0.191 22.216);
      --ps-card-shadow: 0 0 0 0.5px oklch(1 0 0 / 0.08), 0 6px 16px oklch(0 0 0 / 0.35);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100dvh; display: flex; align-items: center; justify-content: center;
    padding: 24px; background: var(--ps-bg); color: var(--ps-fg);
    font-family: Inter, -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI",
      "PingFang SC", "Noto Sans SC", sans-serif;
    -webkit-font-smoothing: antialiased; position: relative;
  }
  /* 环境色晕：与前端 body::before 同构 */
  body::before {
    content: ""; position: fixed; inset: 0; z-index: -1; pointer-events: none;
    background:
      radial-gradient(42% 34% at 16% 2%, oklch(0.972 0.014 85 / 0.85), transparent 70%),
      radial-gradient(38% 30% at 86% 0%, oklch(0.958 0.02 300 / 0.7), transparent 70%),
      radial-gradient(50% 38% at 50% 108%, oklch(0.965 0.02 130 / 0.75), transparent 70%);
  }
  @media (prefers-color-scheme: dark) {
    body::before {
      background:
        radial-gradient(42% 34% at 16% 2%, oklch(0.28 0.03 85 / 0.4), transparent 70%),
        radial-gradient(38% 30% at 86% 0%, oklch(0.27 0.04 290 / 0.45), transparent 70%),
        radial-gradient(50% 38% at 50% 108%, oklch(0.26 0.03 150 / 0.35), transparent 70%);
    }
  }
  .card {
    width: min(100%, 384px); text-align: center; padding: 44px 40px 36px;
    background: color-mix(in oklab, var(--ps-card) 80%, transparent);
    backdrop-filter: blur(24px) saturate(1.5);
    -webkit-backdrop-filter: blur(24px) saturate(1.5);
    border: 1px solid color-mix(in oklab, var(--ps-border) 80%, transparent);
    border-radius: 24px; box-shadow: var(--ps-card-shadow);
    animation: ps-rise 0.5s cubic-bezier(0.22, 1, 0.36, 1) both;
  }
  @media (prefers-reduced-motion: reduce) { .card { animation: none; } }
  @keyframes ps-rise { from { opacity: 0; transform: translateY(14px) scale(0.98); } }
  .eyebrow {
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 11px; letter-spacing: 0.22em; text-transform: uppercase;
    color: var(--ps-muted); margin-bottom: 10px;
  }
  .msg { font-size: 24px; font-weight: 300; letter-spacing: -0.03em; line-height: 1.25; }
  .desc { font-size: 13.5px; color: var(--ps-muted); margin: 10px 0 0; line-height: 1.6; }
  form { margin-top: 24px; display: grid; gap: 10px; text-align: left; }
  .field {
    width: 100%; padding: 12px 16px; border-radius: 13px; font-size: 15px; outline: none;
    border: 1px solid var(--ps-border); background: color-mix(in oklab, var(--ps-card) 90%, transparent);
    color: var(--ps-fg); transition: border-color 0.15s, box-shadow 0.15s;
  }
  .field::placeholder { color: color-mix(in oklab, var(--ps-muted) 75%, transparent); }
  .field:focus-visible { border-color: color-mix(in oklab, var(--ps-fg) 45%, transparent); }
  .err {
    display: flex; gap: 8px; align-items: flex-start; margin: 0; padding: 10px 12px;
    border-radius: 12px; font-size: 13px; line-height: 1.55; text-align: left;
    color: var(--ps-destructive); background: color-mix(in oklab, var(--ps-destructive) 8%, transparent);
  }
  .btn {
    margin-top: 6px; width: 100%; padding: 12px 20px; border: none; border-radius: 9999px;
    background: var(--ps-primary); color: var(--ps-primary-fg);
    font-size: 14.5px; font-weight: 500; font-family: inherit; letter-spacing: 0.01em;
    cursor: pointer; transition: opacity 0.15s, transform 0.15s;
  }
  .btn:hover { opacity: 0.88; }
  .btn:active { transform: scale(0.98); }
  .brand {
    margin-top: 28px; display: inline-flex; align-items: center; gap: 8px;
    font-size: 12.5px; font-weight: 500; color: var(--ps-muted);
  }
  .brand span {
    display: inline-flex; align-items: center; justify-content: center;
    width: 20px; height: 20px; border-radius: 7px;
    background: var(--ps-primary); color: var(--ps-primary-fg);
    font-size: 10.5px; font-weight: 700;
  }
</style>`

func statusHTML(status int, msg string) string {
	return "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + itoa(int64(status)) + " · pageshare</title>" + baseStyle + "</head><body>" +
		"<div class=\"card\"><div class=\"eyebrow\">" + itoa(int64(status)) + "</div>" +
		"<div class=\"msg\">" + template.HTMLEscapeString(msg) + "</div>" +
		"<div class=\"brand\"><span>p</span>pageshare</div></div></body></html>"
}

// gateHTML 是密码门页面。
func gateHTML(st *gateSite, next, action string) string { return gateForm(st, next, "", action) }

func gateHTMLError(st *gateSite, next, action string) string {
	return gateForm(st, next, "密码不正确，请重试", action)
}

type gateSite struct {
	ID   string
	Name string
}

func gateForm(site *gateSite, next, errMsg, action string) string {
	errBlock := ""
	if errMsg != "" {
		errBlock = `<p class="err">` + template.HTMLEscapeString(errMsg) + `</p>`
	}
	title := "此分享受密码保护"
	name := site.Name
	if name != "" {
		title = template.HTMLEscapeString(name)
	}
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + title + ` · pageshare</title>` + baseStyle + `</head><body>` +
		`<div class="card">` +
		`<div class="eyebrow">protected</div>` +
		`<div class="msg" style="font-size:21px">` + title + `</div>` +
		`<p class="desc">请输入访问密码后继续</p>` +
		`<form method="post" action="` + action + `">` +
		`<input type="hidden" name="next" value="` + template.HTMLEscapeString(next) + `">` +
		`<input class="field" name="password" type="password" autofocus required placeholder="访问密码">` +
		errBlock +
		`<button class="btn" type="submit">进入</button>` +
		`</form>` +
		`<div class="brand"><span>p</span>pageshare</div></div></body></html>`
}
