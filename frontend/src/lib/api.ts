/** 管理 API 的类型与 fetch 封装；token 存 localStorage。 */

export type SiteDTO = {
  id: string;
  name: string;
  url: string;
  version: number;
  has_password: boolean;
  spa: boolean;
  entry: string;
  created_at: string;
  updated_at: string;
  expires_at?: string;
};

export type PublishOptions = {
  name?: string;
  ttl?: string;
  password?: string;
  /** 显式清除密码（PUT 时传空串生效） */
  clearPassword?: boolean;
  /** SPA 模式：未命中路径回退入口页；undefined = 保持不变 */
  spa?: boolean;
};

const TOKEN_KEY = "ps.token";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}
export function setToken(t: string) {
  localStorage.setItem(TOKEN_KEY, t);
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const res = await fetch(path, { ...init, headers });
  if (res.status === 401) throw new ApiError(401, "token 无效或已变更");
  const text = await res.text();
  // 错误体可能是 SPA 兜底页等非 JSON 内容（如正常模式下的 setup API 404），不能让解析失败掩盖状态码
  let body: Record<string, unknown> = {};
  try {
    body = text ? JSON.parse(text) : {};
  } catch {
    body = {};
  }
  if (!res.ok) throw new ApiError(res.status, (body.error as string) ?? `请求失败（${res.status}）`);
  return body as T;
}

export function listSites(): Promise<SiteDTO[]> {
  return request<SiteDTO[]>("/api/v1/sites");
}

/** zipBytes 是整站归档；mode.create=false 走 PUT 覆盖。 */
export function publish(
  zipBytes: Uint8Array,
  opts: PublishOptions,
  existing?: SiteDTO,
): Promise<SiteDTO> {
  const form = new FormData();
  form.append("file", new Blob([zipBytes as BlobPart], { type: "application/zip" }), "site.zip");
  if (opts.name) form.append("name", opts.name);
  if (opts.ttl) form.append("ttl", opts.ttl);
  if (opts.password) form.append("password", opts.password);
  else if (opts.clearPassword) form.append("password", "");
  if (opts.spa !== undefined) form.append("spa", opts.spa ? "1" : "0");
  return request<SiteDTO>(existing ? `/api/v1/sites/${existing.id}` : "/api/v1/sites", {
    method: existing ? "PUT" : "POST",
    body: form,
  });
}

export function deleteSite(id: string): Promise<{ deleted: boolean }> {
  return request(`/api/v1/sites/${id}`, { method: "DELETE" });
}

// ---------- MCP 密钥池 ----------

export type MCPKeyDTO = {
  id: string;
  name: string;
  /** 明文前缀（psm_xxxxxxxx…），列表辨认用 */
  prefix: string;
  created_at: string;
  last_used_at?: string;
};

export function listMCPKeys(): Promise<MCPKeyDTO[]> {
  return request<{ keys: MCPKeyDTO[] }>("/api/v1/mcp/keys").then((r) => r.keys);
}

/** 签发新密钥；token 明文只在本次响应返回，服务端落盘只存 SHA-256。 */
export function createMCPKey(name: string): Promise<MCPKeyDTO & { token: string }> {
  return request("/api/v1/mcp/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
}

export function deleteMCPKey(id: string): Promise<{ deleted: boolean }> {
  return request(`/api/v1/mcp/keys/${id}`, { method: "DELETE" });
}

// ---------- MCP 连接池 ----------

export type MCPSessionDTO = {
  /** 会话 ID 前 8 位（完整 Mcp-Session-Id 不外露） */
  id: string;
  /** 客户端 initialize 时自报的 agent 名 + 版本 */
  agent: string;
  /** 走的哪把密钥；管理 token 直连时显示「管理 token」 */
  key_name: string;
  connected_at: string;
  last_seen: string;
};

export function listMCPSessions(): Promise<{ sessions: MCPSessionDTO[]; max: number }> {
  return request("/api/v1/mcp/sessions");
}

/* ── 首次使用引导（setup API：无 Authorization 头，已配置时这些端点整体关闭） ── */

export type SetupStatus = {
  needs_setup: boolean;
  config_path: string;
  suggested_auth_token: string;
  suggested_cookie_secret: string;
  listen: string;
  suggested_base_url: string;
};

export type SetupStoragePayload = {
  backend: "disk" | "s3" | "mem";
  root?: string;
  endpoint?: string;
  region?: string;
  bucket?: string;
  access_key_id?: string;
  secret_access_key?: string;
  prefix?: string;
};

export type SetupCompletePayload = {
  auth_token: string;
  cookie_secret: string;
  public_base_url: string;
  site_wildcard_host: string;
  listen: string;
  db_path: string;
  max_upload_mb: number;
  storage: SetupStoragePayload;
};

/** setup 专用请求：与业务 request 不同，绝不携带 Authorization 头。 */
async function setupRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init);
  const text = await res.text();
  // 错误体可能是 SPA 兜底页等非 JSON 内容（如正常模式下的 setup API 404），不能让解析失败掩盖状态码
  let body: Record<string, unknown> = {};
  try {
    body = text ? JSON.parse(text) : {};
  } catch {
    body = {};
  }
  if (!res.ok) throw new ApiError(res.status, (body.error as string) ?? `请求失败（${res.status}）`);
  return body as T;
}

/** 引导状态；已配置的正常模式下该端点 404，向上抛由调用方识别。 */
export function fetchSetupStatus(): Promise<SetupStatus> {
  return setupRequest<SetupStatus>("/api/v1/setup/status");
}

/** 提交向导配置：服务端校验后原子写入配置文件并触发自重启。 */
export function completeSetup(payload: SetupCompletePayload): Promise<{ ok: boolean; config_path: string }> {
  return setupRequest("/api/v1/setup/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
}

/** 一个待上传文件 + 它在 zip 内的相对路径（目录结构由此保留）。 */
export type PickedFile = { file: File; path: string };

/** 文件选择器来的 File 列表：取 webkitRelativePath（目录上传时）或文件名。 */
export function pickPaths(files: File[]): PickedFile[] {
  return files.map((f) => ({
    file: f,
    path: (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name,
  }));
}

/** 收集待上传文件（保留相对路径），fflate 打 zip。 */
export async function zipFiles(files: PickedFile[]): Promise<Uint8Array> {
  const { zipSync } = await import("fflate");
  const map: Record<string, Uint8Array> = {};
  for (const { file, path } of files) {
    if (file.size > 80 * 1024 * 1024) continue; // 单文件硬上限，防误选
    if (/(^|\/)(\.DS_Store|__MACOSX|\.git)(\/|$)/i.test(path)) continue;
    map[path] = new Uint8Array(await file.arrayBuffer());
  }
  if (Object.keys(map).length === 0) throw new Error("没有可上传的文件");
  return zipSync(map, { level: 6 });
}
