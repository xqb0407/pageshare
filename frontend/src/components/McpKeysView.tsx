// MCP 服务控制台页：密钥池管理 + 接入信息。挂在 /admin/mcp，路由切换见 App.tsx。
import { useCallback, useEffect, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import {
  AlertTriangle, Bot, Check, Copy, KeyRound, Trash2,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { AnimatedToastStack, useAnimatedToastStack } from "@/components/motion/animated-toast-stack";
import {
  createMCPKey, deleteMCPKey, listMCPKeys, listMCPSessions,
  type MCPKeyDTO, type MCPSessionDTO,
} from "@/lib/api";

const spring = { type: "spring" as const, stiffness: 320, damping: 28 };

const MCP_TOOLS = ["publish_site", "list_sites", "get_site", "delete_site"] as const;

function relTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  const m = Math.floor(diff / 60_000);
  if (m < 1) return "刚刚";
  if (m < 60) return `${m} 分钟前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时前`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d} 天前`;
  return new Date(iso).toLocaleDateString("zh-CN");
}

/** 从时刻数起的简短时长：连接了多久用。 */
function upTime(iso: string): string {
  const s = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} 分钟`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时 ${m % 60} 分`;
  return `${Math.floor(h / 24)} 天`;
}

export default function McpKeysView() {
  const [keys, setKeys] = useState<MCPKeyDTO[] | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState<MCPKeyDTO | null>(null);
  const { toasts, showToast, dismissToast } = useAnimatedToastStack({ limit: 4 });

  // 连接池状态：短轮询展示在线 agent；失败不弹 toast（太频繁），页内有降级提示
  const [sessions, setSessions] = useState<MCPSessionDTO[] | null>(null);
  const [sessionsMax, setSessionsMax] = useState(10);
  const [sessionsErr, setSessionsErr] = useState(false);

  const loadSessions = useCallback(async () => {
    try {
      const r = await listMCPSessions();
      setSessions(r.sessions);
      setSessionsMax(r.max);
      setSessionsErr(false);
    } catch {
      setSessionsErr(true);
    }
  }, []);

  useEffect(() => {
    void loadSessions();
    const t = setInterval(() => void loadSessions(), 8000);
    return () => clearInterval(t);
  }, [loadSessions]);

  const load = useCallback(async () => {
    try {
      setKeys(await listMCPKeys());
    } catch (e) {
      showToast({ status: "error", title: "读取密钥池失败", description: String((e as Error)?.message ?? e) });
    }
  }, [showToast]);

  useEffect(() => {
    void load();
  }, [load]);

  const endpoint = `${window.location.origin}/mcp`;
  const clientConfig = JSON.stringify(
    {
      mcpServers: {
        pageshare: {
          url: endpoint,
          headers: { Authorization: "Bearer psm_你的密钥" },
        },
      },
    },
    null,
    2,
  );

  return (
    <>
      {/* 页头 */}
      <motion.div
        initial={{ opacity: 0, y: 14 }}
        animate={{ opacity: 1, y: 0 }}
        transition={spring}
        className="mt-9 mb-7 flex flex-wrap items-end justify-between gap-4"
      >
        <div>
          <h1 className="heading-display text-[32px] leading-[1.1] tracking-[-0.02em]">MCP 服务</h1>
          <p className="mt-1.5 text-[14px] text-muted-foreground">
            任何标准 MCP 客户端都能直连发布与管理站点。为每个客户端签发独立密钥，随时吊销。
          </p>
        </div>
        <Button className="h-9 rounded-full px-4" onClick={() => setCreateOpen(true)}>
          <KeyRound className="size-4" />
          创建密钥
        </Button>
      </motion.div>

      {/* 接入信息 */}
      <motion.div
        initial={{ opacity: 0, y: 14 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ ...spring, delay: 0.05 }}
        className="glass grid gap-4 rounded-[20px] border border-white/40 p-5 shadow-[var(--card-shadow)] dark:border-white/10 sm:grid-cols-2"
      >
        <div>
          <p className="text-[13px] font-medium text-muted-foreground">Streamable HTTP 端点</p>
          <CopyRow value={endpoint} className="mt-2" />
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            标准 MCP 协议（JSON-RPC 2.0），Authorization: Bearer 密钥池密钥或管理 token。
          </p>
        </div>
        <div>
          <p className="text-[13px] font-medium text-muted-foreground">stdio 本地模式</p>
          <CopyRow value="./pageshare mcp -config pageshare.json" className="mt-2" mono />
          <div className="mt-3 flex flex-wrap items-center gap-1.5">
            <span className="text-xs text-muted-foreground">可用工具：</span>
            {MCP_TOOLS.map((t) => (
              <span
                key={t}
                className="inline-flex h-5 items-center rounded-full bg-muted px-2 font-mono text-[11px] font-medium text-muted-foreground"
              >
                {t}
              </span>
            ))}
          </div>
        </div>
        <div className="sm:col-span-2">
          <p className="text-[13px] font-medium text-muted-foreground">客户端配置示例（Claude Desktop / Cursor / 通用 mcp.json）</p>
          <CopyRow value={clientConfig} className="mt-2" mono multiline />
        </div>
      </motion.div>

      {/* 活动连接（多 agent 连接池） */}
      <motion.div
        initial={{ opacity: 0, y: 10 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ ...spring, delay: 0.08 }}
        className="mt-8 mb-4 flex items-center justify-between"
      >
        <div className="flex items-center gap-2">
          <h2 className="text-[15px] font-semibold tracking-[-0.01em]">活动连接</h2>
          <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-700 dark:text-emerald-400">
            <span className="relative flex size-1.5">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-500 opacity-60" />
              <span className="relative inline-flex size-1.5 rounded-full bg-emerald-500" />
            </span>
            实时
          </span>
        </div>
        {sessions !== null && (
          <span
            className={`text-[13px] tabular-nums ${
              sessions.length >= sessionsMax ? "font-medium text-destructive" : "text-muted-foreground"
            }`}
          >
            {sessions.length} / {sessionsMax} 连接
          </span>
        )}
      </motion.div>

      {sessions === null ? (
        <div className="glass rounded-[20px] border border-white/40 shadow-[var(--card-shadow)] dark:border-white/10">
          {sessionsErr ? (
            <p className="px-5 py-6 text-[13px] leading-relaxed text-muted-foreground">
              连接状态读取失败——服务端可能是旧版本二进制，重启 pageshare 后即可显示。
            </p>
          ) : (
            [0, 1].map((i) => (
              <div key={i} className="flex items-center gap-3 border-b border-border/60 px-5 py-4 last:border-b-0">
                <div className="size-9 animate-pulse rounded-[11px] bg-muted" />
                <div className="flex-1 space-y-2">
                  <div className="h-3.5 w-40 animate-pulse rounded-full bg-muted" />
                  <div className="h-3 w-28 animate-pulse rounded-full bg-muted/70" />
                </div>
              </div>
            ))
          )}
        </div>
      ) : sessions.length === 0 ? (
        <div className="glass rounded-[20px] border border-white/40 px-5 py-6 shadow-[var(--card-shadow)] dark:border-white/10">
          <p className="text-[13px] leading-relaxed text-muted-foreground">
            当前没有 agent 在线。客户端通过 <span className="font-mono text-foreground">/mcp</span> 端点
            initialize 建连后，会实时出现在这里。
          </p>
        </div>
      ) : (
        <div className="glass overflow-hidden rounded-[20px] border border-white/40 shadow-[var(--card-shadow)] dark:border-white/10">
          <AnimatePresence initial={false} mode="popLayout">
            {sessions.map((sess) => (
              <motion.div
                key={sess.id}
                layout
                initial={{ opacity: 0, y: 10 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, height: 0, scale: 0.98, filter: "blur(4px)" }}
                transition={spring}
                className="flex items-center gap-3 border-b border-border/60 px-5 py-4 last:border-b-0"
              >
                <span className="flex size-9 shrink-0 items-center justify-center rounded-[11px] bg-primary/10 text-primary">
                  <Bot className="size-4" />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex min-w-0 items-center gap-2">
                    <p className="truncate text-[14px] font-medium">{sess.agent}</p>
                    <span className="shrink-0 rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                      {sess.key_name}
                    </span>
                  </div>
                  <p className="mt-0.5 text-xs text-muted-foreground">
                    已连接 {upTime(sess.connected_at)}
                    {" · "}
                    最后活跃 {relTime(sess.last_seen)}
                    {" · "}
                    会话 {sess.id}
                  </p>
                </div>
                <span className="size-2 shrink-0 rounded-full bg-emerald-500" title="在线" />
              </motion.div>
            ))}
          </AnimatePresence>
          <p className="border-t border-border/60 px-5 py-2.5 text-[11px] leading-relaxed text-muted-foreground">
            连接池上限 {sessionsMax} 条会话；空闲 30 分钟自动回收。池满时新的 initialize 请求会被拒绝（503），已有连接不受影响。
          </p>
        </div>
      )}

      {/* 密钥池 */}
      <motion.div
        initial={{ opacity: 0, y: 10 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ ...spring, delay: 0.11 }}
        className="mt-8 mb-4 flex items-center justify-between"
      >
        <h2 className="text-[15px] font-semibold tracking-[-0.01em]">密钥池</h2>
        {keys !== null && (
          <span className="text-[13px] text-muted-foreground">{keys.length} / 20 把</span>
        )}
      </motion.div>

      {keys === null ? (
        <div className="glass rounded-[20px] border border-white/40 shadow-[var(--card-shadow)] dark:border-white/10">
          {[0, 1].map((i) => (
            <div key={i} className="flex items-center gap-3 border-b border-border/60 px-5 py-4 last:border-b-0">
              <div className="size-9 animate-pulse rounded-[11px] bg-muted" />
              <div className="flex-1 space-y-2">
                <div className="h-3.5 w-32 animate-pulse rounded-full bg-muted" />
                <div className="h-3 w-48 animate-pulse rounded-full bg-muted/70" />
              </div>
            </div>
          ))}
        </div>
      ) : keys.length === 0 ? (
        <motion.div
          initial={{ opacity: 0, y: 12 }}
          animate={{ opacity: 1, y: 0 }}
          transition={spring}
          className="glass flex flex-col items-center rounded-[20px] border border-white/40 px-6 py-12 text-center shadow-[var(--card-shadow)] dark:border-white/10"
        >
          <span className="flex size-12 items-center justify-center rounded-[14px] bg-primary/10 text-primary">
            <KeyRound className="size-6" />
          </span>
          <p className="mt-4 text-[15px] font-medium">密钥池还是空的</p>
          <p className="mt-1 max-w-[380px] text-[13px] leading-relaxed text-muted-foreground">
            给每个 MCP 客户端（Claude Desktop、Cursor、自动化脚本…）各签发一把密钥，互不影响，随时吊销。
          </p>
          <Button className="mt-5 rounded-full px-4" onClick={() => setCreateOpen(true)}>
            <KeyRound className="size-4" />
            创建第一把密钥
          </Button>
        </motion.div>
      ) : (
        <div className="glass overflow-hidden rounded-[20px] border border-white/40 shadow-[var(--card-shadow)] dark:border-white/10">
          <AnimatePresence initial={false}>
            {keys.map((k) => (
              <motion.div
                key={k.id}
                layout
                initial={{ opacity: 0, y: 10 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, height: 0, scale: 0.98, filter: "blur(4px)" }}
                transition={spring}
                className="flex items-center gap-3 border-b border-border/60 px-5 py-4 last:border-b-0"
              >
                <span className="flex size-9 shrink-0 items-center justify-center rounded-[11px] bg-primary/10 text-primary">
                  <KeyRound className="size-4" />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex min-w-0 items-center gap-2">
                    <p className="truncate text-[14px] font-medium">{k.name}</p>
                    <span className="shrink-0 rounded-full bg-muted px-2 py-0.5 font-mono text-[11px] text-muted-foreground">
                      {k.prefix}…
                    </span>
                  </div>
                  <p className="mt-0.5 text-xs text-muted-foreground">
                    创建于 {relTime(k.created_at)}
                    {" · "}
                    最近使用 {k.last_used_at ? relTime(k.last_used_at) : "从未使用"}
                  </p>
                </div>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`删除密钥 ${k.name}`}
                  className="text-destructive hover:text-destructive"
                  onClick={() => setConfirmDelete(k)}
                >
                  <Trash2 className="size-4" />
                </Button>
              </motion.div>
            ))}
          </AnimatePresence>
        </div>
      )}

      <CreateKeyDialog
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => void load()}
        onError={(msg) => showToast({ status: "error", title: "签发失败", description: msg })}
      />

      {confirmDelete && (
        <Dialog open onOpenChange={(o) => !o && setConfirmDelete(null)}>
          <DialogContent className="max-w-sm rounded-[20px]">
            <DialogHeader>
              <span className="mb-1 flex size-10 items-center justify-center rounded-[12px] bg-destructive/10 text-destructive">
                <Trash2 className="size-5" />
              </span>
              <DialogTitle>删除密钥「{confirmDelete.name}」？</DialogTitle>
              <DialogDescription>
                持有 {confirmDelete.prefix}… 的客户端将立即失去 MCP 访问能力，池内其余密钥不受影响。
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="secondary" className="h-9 px-4" onClick={() => setConfirmDelete(null)}>取消</Button>
              <Button
                variant="destructive"
                className="h-9 px-4"
                onClick={() => {
                  const key = confirmDelete;
                  setConfirmDelete(null);
                  void deleteMCPKey(key.id)
                    .then(() => {
                      showToast({ status: "success", title: "已删除", description: `「${key.name}」已移出密钥池` });
                      return load();
                    })
                    .catch((e) => showToast({ status: "error", title: "删除失败", description: String((e as Error)?.message ?? e) }));
                }}
              >
                删除
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      <AnimatedToastStack toasts={toasts} onDismiss={dismissToast} fixed position="bottom-right" />
    </>
  );
}

/** 单行/多行复制组件：点击整块即复制，图标确认反馈。 */
function CopyRow({ value, className, mono = false, multiline = false }: {
  value: string;
  className?: string;
  mono?: boolean;
  multiline?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        });
      }}
      className={`group w-full rounded-[12px] border border-border/70 bg-background/60 px-3 text-left transition-colors hover:border-primary/40 hover:bg-background ${className ?? ""}`}
    >
      <span className="flex items-center gap-2">
        <span
          className={`min-w-0 flex-1 text-xs text-muted-foreground transition-colors group-hover:text-foreground ${
            mono ? "font-mono" : ""
          } ${multiline ? "whitespace-pre-wrap py-2.5 leading-relaxed" : "truncate py-2"}`}
        >
          {value}
        </span>
        {copied ? (
          <Check className="size-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
        ) : (
          <Copy className="size-3.5 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
        )}
      </span>
    </button>
  );
}

/** 签发对话框：表单 → 出现明文（仅此一次）两段式。 */
function CreateKeyDialog({
  open, onClose, onCreated, onError,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<{ token: string; prefix: string } | null>(null);
  const [copied, setCopied] = useState(false);

  const reset = () => {
    setName("");
    setIssued(null);
    setCopied(false);
  };
  const close = () => {
    onClose();
    reset();
  };

  const submit = () => {
    setBusy(true);
    createMCPKey(name.trim())
      .then((res) => {
        setIssued({ token: res.token, prefix: res.prefix });
        onCreated();
      })
      .catch((e) => onError(String((e as Error)?.message ?? e)))
      .finally(() => setBusy(false));
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      <DialogContent className="max-w-md rounded-[20px]">
        {issued === null ? (
          <>
            <DialogHeader>
              <DialogTitle>创建 MCP 密钥</DialogTitle>
              <DialogDescription>为这个客户端起个名字，方便日后辨认与吊销。</DialogDescription>
            </DialogHeader>
            <div className="grid gap-1.5 py-1">
              <Label htmlFor="ps-mcp-name">名称（可选）</Label>
              <Input
                id="ps-mcp-name"
                placeholder="如：Claude Desktop、Q3 自动化脚本"
                value={name}
                maxLength={40}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && !busy && submit()}
                autoFocus
              />
            </div>
            <DialogFooter>
              <Button variant="secondary" className="h-9 px-4" onClick={close} disabled={busy}>取消</Button>
              <Button className="h-9 px-4" disabled={busy} onClick={submit}>
                {busy ? "签发中…" : "签发密钥"}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>密钥已签发</DialogTitle>
              <DialogDescription>
                请立即保存——出于安全考虑，明文只在这一次展示，服务端只保留哈希。
              </DialogDescription>
            </DialogHeader>
            <div className="rounded-[14px] border border-amber-500/25 bg-amber-500/[0.06] px-3.5 py-3">
              <p className="flex items-start gap-2 text-[13px] leading-relaxed text-amber-700 dark:text-amber-400">
                <AlertTriangle className="mt-0.5 size-4 shrink-0" />
                关闭本对话框后将无法再次查看这把密钥。
              </p>
            </div>
            <button
              type="button"
              onClick={() => {
                void navigator.clipboard.writeText(issued.token).then(() => {
                  setCopied(true);
                  setTimeout(() => setCopied(false), 1500);
                });
              }}
              className="group flex w-full items-center gap-2 rounded-[12px] border border-border/70 bg-muted/40 px-3 py-2.5 text-left transition-colors hover:border-primary/40"
            >
              <span className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{issued.token}</span>
              {copied ? (
                <Check className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
              ) : (
                <Copy className="size-4 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
              )}
            </button>
            <DialogFooter>
              <Button className="h-9 px-4" onClick={close}>我已保存，完成</Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
