import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import {
  Check, Copy, ExternalLink, Eye, EyeOff, Link2, Lock, LogOut, RefreshCw,
  Search, Timer, Trash2, UploadCloud, X,
} from "lucide-react";
import { useTheme } from "next-themes";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { AnimatedCharacters } from "@/components/AnimatedCharacters";
import { NotFoundGlitch } from "@/components/motion/not-found/glitch";
import { ThemeToggle } from "@/components/motion/theme-toggle";
import SetupWizard from "@/components/setup/SetupWizard";
import {
  AnimatedToastStack, useAnimatedToastStack,
} from "@/components/motion/animated-toast-stack";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/motion/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/motion/tabs";
import { Checkbox } from "@/components/motion/checkbox";
import McpKeysView from "@/components/McpKeysView";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  ApiError, clearToken, deleteSite, getToken, listSites, pickPaths, publish, setToken,
  zipFiles, type PickedFile, type PublishOptions, type SiteDTO,
} from "@/lib/api";
import { cn } from "@/lib/utils";

// 全站统一的弹簧手感：一点点回弹，不闹腾
const spring = { type: "spring" as const, stiffness: 320, damping: 28 };

type UploadTarget = { files: PickedFile[]; overwrite: SiteDTO | null };
type SiteFilter = "all" | "password" | "expiring";
type SiteSort = "updated" | "created" | "name";

/** 「即将过期」阈值：72 小时。 */
const EXPIRING_SOON_MS = 72 * 3600_000;

export default function App() {
  const [authed, setAuthed] = useState<boolean>(Boolean(getToken()));
  const { resolvedTheme: theme } = useTheme();
  const [sites, setSites] = useState<SiteDTO[] | null>(null);
  const [error, setError] = useState("");
  const [login, setLogin] = useState("");
  const [tokenFocused, setTokenFocused] = useState(false);
  const [showToken, setShowToken] = useState(false);
  const [upload, setUpload] = useState<UploadTarget | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState<string>("");
  const [confirmDelete, setConfirmDelete] = useState<SiteDTO | null>(null);
  const [dragging, setDragging] = useState(false);
  const [filter, setFilter] = useState<SiteFilter>("all");
  const [sort, setSort] = useState<SiteSort>("updated");
  const [query, setQuery] = useState("");
  const newFileRef = useRef<HTMLInputElement>(null);
  const { toasts, showToast, dismissToast } = useAnimatedToastStack({ limit: 4 });

  const notifyError = useCallback(
    (title: string, e: unknown) => {
      showToast({ status: "error", title, description: String((e as Error)?.message ?? e) });
    },
    [showToast],
  );

  const refresh = useCallback(async () => {
    try {
      setSites(await listSites());
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        clearToken();
        setAuthed(false);
      } else {
        notifyError("加载站点失败", e);
      }
    }
  }, [notifyError]);

  useEffect(() => {
    if (authed) void refresh();
  }, [authed, refresh]);

  const doLogin = async () => {
    setToken(login.trim());
    try {
      setSites(await listSites());
      setAuthed(true);
      setError("");
    } catch {
      clearToken();
      setError("token 校验失败，请重试");
    }
  };

  const doUpload = async (opts: PublishOptions) => {
    if (!upload) return;
    setBusy(true);
    try {
      const zip = await zipFiles(upload.files);
      const site = await publish(zip, opts, upload.overwrite ?? undefined);
      setUpload(null);
      await refresh();
      showToast({
        status: "success",
        title: upload.overwrite ? "覆盖发布成功" : "发布成功",
        description: `${site.name || site.id} · ${site.url}`,
        action: { label: "打开", onClick: () => window.open(site.url, "_blank") },
      });
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        clearToken();
        setAuthed(false);
      } else {
        notifyError("发布失败", e);
      }
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (site: SiteDTO) => {
    setBusy(true);
    try {
      await deleteSite(site.id);
      setConfirmDelete(null);
      await refresh();
      showToast({
        status: "success",
        title: "已删除",
        description: `「${site.name || site.id}」的分享链接已失效`,
      });
    } catch (e) {
      notifyError("删除失败", e);
    } finally {
      setBusy(false);
    }
  };

  const copyUrl = async (site: SiteDTO) => {
    await navigator.clipboard.writeText(site.url);
    setCopied(site.id);
    setTimeout(() => setCopied(""), 1500);
  };

  const pickFor = (site: SiteDTO) => {
    const input = document.createElement("input");
    input.type = "file";
    input.multiple = true;
    input.onchange = () => {
      const files = pickPaths(Array.from(input.files ?? []));
      if (files.length) setUpload({ files, overwrite: site });
    };
    input.click();
  };

  // /admin/mcp 为 MCP 服务页（无路由 SPA，同组件按路径分叉）
  const isMCPView =
    window.location.pathname === "/admin/mcp" || window.location.pathname === "/admin/mcp/";

  // 全页拖拽：enter/leave 计数避免子元素抖动；文件夹用 webkitGetAsEntry 递归展开
  useEffect(() => {
    if (!authed || isMCPView) return;
    let depth = 0;
    const hasFiles = (e: DragEvent) => Array.from(e.dataTransfer?.types ?? []).includes("Files");
    const onEnter = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      depth += 1;
      setDragging(true);
    };
    const onLeave = () => {
      depth = Math.max(0, depth - 1);
      if (depth === 0) setDragging(false);
    };
    const onOver = (e: DragEvent) => {
      if (hasFiles(e)) e.preventDefault();
    };
    const onDrop = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      depth = 0;
      setDragging(false);
      const dt = e.dataTransfer;
      if (!dt) return;
      // webkitGetAsEntry 必须在事件同步阶段调用，先收集 entries 再异步展开
      const entries = Array.from(dt.items ?? [])
        .map((it) => (typeof it.webkitGetAsEntry === "function" ? it.webkitGetAsEntry() : null))
        .filter((en): en is FileSystemEntry => Boolean(en));
      void (async () => {
        let picked: PickedFile[] = [];
        if (entries.some((en) => en.isDirectory)) {
          for (const en of entries) await readEntry(en, picked);
        }
        if (picked.length === 0) picked = pickPaths(Array.from(dt.files ?? []));
        if (picked.length) setUpload({ files: picked, overwrite: null });
      })();
    };
    document.addEventListener("dragenter", onEnter);
    document.addEventListener("dragleave", onLeave);
    document.addEventListener("dragover", onOver);
    document.addEventListener("drop", onDrop);
    return () => {
      document.removeEventListener("dragenter", onEnter);
      document.removeEventListener("dragleave", onLeave);
      document.removeEventListener("dragover", onOver);
      document.removeEventListener("drop", onDrop);
    };
  }, [authed, isMCPView]);

  const stats = useMemo(() => {
    const list = sites ?? [];
    return {
      all: list.length,
      password: list.filter((s) => s.has_password).length,
      expiring: list.filter(
        (s) => s.expires_at && new Date(s.expires_at).getTime() - Date.now() <= EXPIRING_SOON_MS,
      ).length,
    };
  }, [sites]);

  const visible = useMemo(() => {
    let list = sites ?? [];
    if (filter === "password") list = list.filter((s) => s.has_password);
    if (filter === "expiring") {
      list = list.filter(
        (s) => s.expires_at && new Date(s.expires_at).getTime() - Date.now() <= EXPIRING_SOON_MS,
      );
    }
    const q = query.trim().toLowerCase();
    if (q) list = list.filter((s) => `${s.name} ${s.id} ${s.url}`.toLowerCase().includes(q));
    const sorted = [...list];
    if (sort === "name") {
      sorted.sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id, "zh-CN"));
    } else if (sort === "created") {
      sorted.sort((a, b) => b.created_at.localeCompare(a.created_at));
    } else {
      sorted.sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    }
    return sorted;
  }, [sites, filter, query, sort]);

  const filtered = query.trim() !== "" || filter !== "all";

  // 首次使用向导：/admin/setup 独立整页渲染（引导模式专属，需在 hooks 全部执行后再分叉）
  if (window.location.pathname === "/admin/setup") {
    return <SetupWizard />;
  }

  // 管理台无客户端路由：已知入口之外的路径一律渲染动画 404（Go 侧同样回 404 状态码）
  const knownAdminPath =
    window.location.pathname === "/" ||
    window.location.pathname === "/index.html" ||
    window.location.pathname === "/admin" ||
    window.location.pathname === "/admin/" ||
    window.location.pathname === "/admin/setup" ||
    window.location.pathname === "/admin/mcp" ||
    window.location.pathname === "/admin/mcp/";
  if (!knownAdminPath) {
    return (
      <div className="relative z-10 flex min-h-dvh items-center justify-center">
        <NotFoundGlitch
          title="页面不存在"
          description="你访问的地址可能已过期回收，或从未存在。"
          homeHref="/admin/"
          homeLabel="返回管理台"
          browseHref="/"
          browseLabel="访问主页"
        />
      </div>
    );
  }

  if (!authed) {
    return (
      <div className="relative z-10 flex min-h-dvh items-center justify-center gap-20 p-6">
        {/* 品牌叙事：宽屏左侧，小屏隐藏 */}
        <motion.section
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ type: "spring", stiffness: 260, damping: 22 }}
          className="hidden max-w-[440px] lg:block"
        >
          <Brand />
          <h1 className="mt-10 text-[44px] font-semibold leading-[1.08] tracking-[-0.03em]">
            把一个文件夹
            <br />
            变成一条链接
          </h1>
          <p className="mt-5 text-[15px] leading-relaxed text-muted-foreground">
            AI 生成的静态页面，拖进来就有一个可分享的地址。
            覆盖更新链接不变，到期自动回收。
          </p>
          {/* 眼睛小人：输入 token 时紫色角色会捂眼，点“显示”后全体转头偷看 */}
          <motion.div
            initial={{ opacity: 0, y: 14 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ ...spring, delay: 0.14 }}
            className="mt-8"
          >
            <AnimatedCharacters
              scale={0.72}
              isTyping={tokenFocused}
              showPassword={showToken}
              passwordLength={login.length}
              darkColor={theme === "dark" ? "#4a4a4a" : "#2D2D2D"}
            />
          </motion.div>
          <ul className="mt-8 space-y-4">
            {[
              { icon: UploadCloud, title: "上传即发布", desc: "拖入目录自动打包，几秒后就是公网链接" },
              { icon: Link2, title: "链接永不换", desc: "覆盖更新同一个地址，收过链接的人永远打得开" },
              { icon: Timer, title: "过期自动回收", desc: "72 小时 / 7 天随手设，不给互联网留垃圾" },
            ].map((f, i) => (
              <motion.li
                key={f.title}
                initial={{ opacity: 0, y: 14 }}
                animate={{ opacity: 1, y: 0 }}
                transition={{ ...spring, delay: 0.18 + i * 0.09 }}
                className="flex items-start gap-3.5"
              >
                <span className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-[11px] bg-primary/10 text-primary">
                  <f.icon className="size-[18px]" />
                </span>
                <div>
                  <p className="text-[15px] font-medium">{f.title}</p>
                  <p className="mt-0.5 text-[13px] leading-relaxed text-muted-foreground">{f.desc}</p>
                </div>
              </motion.li>
            ))}
          </ul>
        </motion.section>

        <motion.div
          initial={{ opacity: 0, y: 24, scale: 0.94 }}
          animate={{ opacity: 1, y: 0, scale: 1 }}
          transition={{ type: "spring", stiffness: 260, damping: 22, delay: 0.08 }}
          className="glass-strong w-full max-w-sm rounded-[22px] border border-white/40 p-8 shadow-[var(--card-shadow)] dark:border-white/10"
        >
          <div className="mb-6 flex items-center justify-between">
            <span className="lg:hidden"><Brand /></span>
            <span className="hidden text-md text-muted-foreground lg:inline">管理员登录</span>
            <ThemeToggle
              variant="circle-blur"
              start="top-right"
              aria-label="切换主题"
              className="size-9 rounded-md hover:bg-accent hover:text-accent-foreground"
              iconClassName="size-4"
            />
          </div>
          <h1 className="mb-1 text-[22px] font-semibold tracking-[-0.02em] lg:hidden">登录管理页</h1>
          <p className="mb-6 text-md text-muted-foreground lg:hidden">输入服务端配置的管理 token</p>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void doLogin();
            }}
            className="flex flex-col gap-3"
          >
            <div className="relative">
              <Input
                type={showToken ? "text" : "password"}
                placeholder="管理 token"
                value={login}
                onChange={(e) => setLogin(e.target.value)}
                onFocus={() => setTokenFocused(true)}
                onBlur={() => setTokenFocused(false)}
                autoFocus
                className="h-10 pr-10 font-mono text-[13px]"
              />
              <button
                type="button"
                aria-label={showToken ? "隐藏 token" : "显示 token"}
                onClick={() => setShowToken((v) => !v)}
                className="absolute top-1/2 right-2.5 -translate-y-1/2 text-muted-foreground transition-colors hover:text-ink"
              >
                {showToken ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
              </button>
            </div>
            <AnimatePresence mode="popLayout">
              {error && (
                <motion.p
                  key={error}
                  initial={{ opacity: 0, y: -4 }}
                  animate={{ opacity: 1, x: [0, -10, 10, -6, 6, 0] }}
                  exit={{ opacity: 0 }}
                  transition={{ duration: 0.45, ease: "easeOut" }}
                  className="text-sm text-destructive"
                >
                  {error}
                </motion.p>
              )}
            </AnimatePresence>
            <Button type="submit" className="mt-1 h-10 text-sm font-medium">进入</Button>
          </form>
        </motion.div>
      </div>
    );
  }

  return (
    <div className="relative z-10 min-h-dvh">
      {/* 顶栏：全宽玻璃导航 */}
      <header className="glass sticky top-0 z-30 border-b border-border/60">
        <div className="mx-auto flex h-14 max-w-6xl items-center justify-between px-6">
          <div className="flex items-center gap-3">
            <Brand />
            <span className="hidden h-4 w-px bg-border sm:block" />
            <nav className="hidden items-center gap-1 sm:flex" aria-label="控制台导航">
              <a
                href="/admin/"
                aria-current={!isMCPView ? "page" : undefined}
                className={cn(
                  "rounded-full px-3 py-1.5 text-[13px] font-medium transition-colors",
                  !isMCPView ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground",
                )}
              >
                站点
              </a>
              <a
                href="/admin/mcp"
                aria-current={isMCPView ? "page" : undefined}
                className={cn(
                  "rounded-full px-3 py-1.5 text-[13px] font-medium transition-colors",
                  isMCPView ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground",
                )}
              >
                MCP 服务
              </a>
            </nav>
          </div>
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="icon" aria-label="刷新列表" onClick={() => void refresh()}>
              <RefreshCw className="size-4" />
            </Button>
            <ThemeToggle
              variant="circle-blur"
              start="top-right"
              aria-label="切换主题"
              className="size-9 rounded-md hover:bg-accent hover:text-accent-foreground"
              iconClassName="size-4"
            />
            <Button
              variant="ghost"
              size="icon"
              aria-label="退出登录"
              onClick={() => {
                clearToken();
                setAuthed(false);
                setSites(null);
                setFilter("all");
                setQuery("");
              }}
            >
              <LogOut className="size-4" />
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-6 pb-20">
        {isMCPView ? (
          <McpKeysView />
        ) : (
          <>
        {/* 页头：标题 + 主操作 */}
        <motion.div
          initial={{ opacity: 0, y: 14 }}
          animate={{ opacity: 1, y: 0 }}
          transition={spring}
          className="mt-9 mb-7 flex flex-wrap items-end justify-between gap-4"
        >
          <div>
            <h1 className="heading-display text-[32px] leading-[1.1] tracking-[-0.02em]">站点</h1>
            <p className="mt-1.5 text-[14px] text-muted-foreground">
              发布静态站点并分享链接，覆盖更新地址不变，到期自动回收。
            </p>
          </div>
          <Button className="h-9 rounded-full px-4" onClick={() => newFileRef.current?.click()}>
            <UploadCloud className="size-4" />
            上传站点
          </Button>
        </motion.div>

        {/* 上传条：紧凑横排，整条可点 */}
        <motion.div
          initial={{ opacity: 0, y: 14 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ ...spring, delay: 0.05 }}
        >
          <button
            type="button"
            onClick={() => newFileRef.current?.click()}
            className="glass group flex w-full items-center gap-4 rounded-[18px] border border-dashed border-border px-5 py-4 text-left shadow-[var(--card-shadow)] transition-all hover:border-primary/50"
          >
            <motion.span
              animate={{ y: [0, -4, 0] }}
              transition={{ duration: 2.6, repeat: Infinity, ease: "easeInOut" }}
              className="flex size-11 shrink-0 items-center justify-center rounded-[13px] bg-primary/10 text-primary"
            >
              <UploadCloud className="size-5" />
            </motion.span>
            <span className="min-w-0 flex-1">
              <span className="block text-[14.5px] font-medium">拖入文件或文件夹，即刻发布</span>
              <span className="mt-0.5 block truncate text-[12.5px] text-muted-foreground">
                html/css/js 静态站点，整目录打包上传后生成分享链接
              </span>
            </span>
            <span className="hidden shrink-0 items-center gap-1.5 rounded-full bg-secondary px-3.5 py-1.5 text-[13px] font-medium text-secondary-foreground transition-colors group-hover:bg-primary group-hover:text-primary-foreground sm:inline-flex">
              选择文件
            </span>
          </button>
        </motion.div>

        {/* 工具栏：筛选 + 搜索 + 排序 */}
        {sites !== null && (
          <motion.div
            initial={{ opacity: 0, y: 10 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ ...spring, delay: 0.08 }}
            className="mt-8 mb-5 flex flex-wrap items-center justify-between gap-3"
          >
            <Tabs value={filter} onValueChange={(v) => setFilter(v as SiteFilter)}>
              <TabsList className="border border-border/60 shadow-[var(--card-shadow)]">
                <TabsTrigger value="all">
                  全部<span className="ml-1.5 text-xs opacity-60 tabular-nums">{stats.all}</span>
                </TabsTrigger>
                <TabsTrigger value="password">
                  密码保护<span className="ml-1.5 text-xs opacity-60 tabular-nums">{stats.password}</span>
                </TabsTrigger>
                <TabsTrigger value="expiring">
                  即将过期<span className="ml-1.5 text-xs opacity-60 tabular-nums">{stats.expiring}</span>
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <div className="flex flex-1 flex-wrap items-center justify-end gap-2">
              <div className="relative w-full sm:w-56">
                <Search className="pointer-events-none absolute top-1/2 left-3.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="搜索名称或 ID"
                  className="h-9 rounded-full border-border/70 bg-card/70 pr-8 pl-9 text-[13px]"
                />
                {query && (
                  <button
                    type="button"
                    aria-label="清空搜索"
                    onClick={() => setQuery("")}
                    className="absolute top-1/2 right-3 -translate-y-1/2 text-muted-foreground transition-colors hover:text-foreground"
                  >
                    <X className="size-3.5" />
                  </button>
                )}
              </div>
              <Select value={sort} onValueChange={(v) => setSort(v as SiteSort)}>
                <SelectTrigger className="h-9 w-[124px] rounded-full border-border/70 bg-card/70 py-0 text-[13px]">
                  <SelectValue placeholder="排序" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="updated">最近更新</SelectItem>
                  <SelectItem value="created">创建时间</SelectItem>
                  <SelectItem value="name">名称</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </motion.div>
        )}

        <input
          ref={newFileRef}
          type="file"
          multiple
          className="hidden"
          onChange={(e) => {
            const files = pickPaths(Array.from(e.target.files ?? []));
            if (files.length) setUpload({ files, overwrite: null });
            e.currentTarget.value = "";
          }}
        />

        {/* 主体：骨架 / 空态 / 卡片栅格 */}
        {sites === null ? (
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {[0, 1, 2, 3].map((i) => <SiteSkeleton key={i} />)}
          </div>
        ) : visible.length === 0 ? (
          <motion.div
            initial={{ opacity: 0, y: 12 }}
            animate={{ opacity: 1, y: 0 }}
            transition={spring}
            className="glass mt-2 flex flex-col items-center rounded-[20px] border border-white/40 px-6 py-14 text-center shadow-[var(--card-shadow)] dark:border-white/10"
          >
            <span className="flex size-12 items-center justify-center rounded-[14px] bg-primary/10 text-primary">
              {filtered ? <Search className="size-5" /> : <UploadCloud className="size-6" />}
            </span>
            <p className="mt-4 text-[15px] font-medium">
              {filtered ? "没有匹配的站点" : "还没有站点"}
            </p>
            <p className="mt-1 max-w-[360px] text-[13px] leading-relaxed text-muted-foreground">
              {filtered
                ? "换个关键词，或清除筛选条件再试试。"
                : "拖入一个 html/css/js 文件夹，或点击下方按钮上传第一个站点。"}
            </p>
            {filtered ? (
              <Button
                variant="secondary"
                className="mt-5 rounded-full px-4"
                onClick={() => {
                  setQuery("");
                  setFilter("all");
                }}
              >
                清除筛选
              </Button>
            ) : (
              <Button className="mt-5 rounded-full px-4" onClick={() => newFileRef.current?.click()}>
                <UploadCloud className="size-4" />
                上传第一个站点
              </Button>
            )}
          </motion.div>
        ) : (
          <motion.ul key="grid" className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <AnimatePresence mode="popLayout">
              {visible.map((s, i) => (
                <SiteCard
                  key={s.id}
                  site={s}
                  index={i}
                  copied={copied === s.id}
                  onCopy={() => void copyUrl(s)}
                  onUpdate={() => pickFor(s)}
                  onDelete={() => setConfirmDelete(s)}
                />
              ))}
            </AnimatePresence>
          </motion.ul>
        )}
          </>
        )}
      </main>

      {/* 全页拖拽遮罩 */}
      <AnimatePresence>
        {dragging && !isMCPView && (
          <motion.div
            key="drag-overlay"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.15 }}
            className="pointer-events-none fixed inset-0 z-40"
          >
            <div className="absolute inset-0 bg-background/55 backdrop-blur-sm" />
            <div className="absolute inset-4 rounded-[28px] border-2 border-dashed border-primary/50 bg-primary/[0.03]" />
            <div className="absolute inset-0 flex flex-col items-center justify-center gap-3">
              <motion.span
                animate={{ y: [0, -10, 0] }}
                transition={{ duration: 1.6, repeat: Infinity, ease: "easeInOut" }}
                className="flex size-14 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-lg"
              >
                <UploadCloud className="size-7" />
              </motion.span>
              <p className="text-[15px] font-medium">松开以上传</p>
              <p className="text-[13px] text-muted-foreground">支持单个文件与整个文件夹</p>
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      {!isMCPView && upload && (
        <UploadDialog
          target={upload}
          busy={busy}
          onClose={() => setUpload(null)}
          onSubmit={(opts) => void doUpload(opts)}
        />
      )}
      {!isMCPView && confirmDelete && (
        <Dialog open onOpenChange={(o) => !o && setConfirmDelete(null)}>
          <DialogContent className="max-w-sm rounded-[20px]">
            <DialogHeader>
              <span className="mb-1 flex size-10 items-center justify-center rounded-[12px] bg-destructive/10 text-destructive">
                <Trash2 className="size-5" />
              </span>
              <DialogTitle>删除「{confirmDelete.name || confirmDelete.id}」？</DialogTitle>
              <DialogDescription>
                分享链接立即失效，对象存储中的内容也会被清除，不可恢复。
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="secondary" className="h-9 px-4" onClick={() => setConfirmDelete(null)}>取消</Button>
              <Button variant="destructive" className="h-9 px-4" disabled={busy} onClick={() => void doDelete(confirmDelete)}>
                删除
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      <AnimatedToastStack toasts={toasts} onDismiss={dismissToast} fixed position="bottom-right" />
    </div>
  );
}

/** 递归展开拖拽条目：保留目录结构（fullPath 去掉开头的 "/"）。 */
async function readEntry(entry: FileSystemEntry, out: PickedFile[]): Promise<void> {
  if (entry.isFile) {
    const file = await new Promise<File | null>((resolve) =>
      (entry as FileSystemFileEntry).file(resolve, () => resolve(null)),
    );
    if (file) out.push({ file, path: entry.fullPath.replace(/^\//, "") });
  } else if (entry.isDirectory) {
    const reader = (entry as FileSystemDirectoryEntry).createReader();
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve) =>
        reader.readEntries(resolve, () => resolve([])),
      );
      if (batch.length === 0) break;
      for (const child of batch) await readEntry(child, out);
    }
  }
}

const CHIP_TONES = {
  ok: "border-emerald-500/20 bg-emerald-500/[0.07] text-emerald-700 dark:text-emerald-400",
  warn: "border-amber-500/25 bg-amber-500/[0.08] text-amber-700 dark:text-amber-400",
  danger: "border-destructive/25 bg-destructive/10 text-destructive",
} as const;

function Chip({ tone, label }: { tone: keyof typeof CHIP_TONES; label: string }) {
  return (
    <span
      className={cn(
        "inline-flex h-6 shrink-0 items-center gap-1.5 rounded-full border px-2.5 text-[11px] font-medium",
        CHIP_TONES[tone],
      )}
    >
      <span className="size-1.5 rounded-full bg-current" />
      {label}
    </span>
  );
}

/** 到期状态徽章：永久 / 剩余时间，按紧急度着色。 */
function ExpiryChip({ expiresAt }: { expiresAt?: string }) {
  if (!expiresAt) return <Chip tone="ok" label="永久" />;
  const diff = new Date(expiresAt).getTime() - Date.now();
  if (diff <= 0) return <Chip tone="danger" label="已过期" />;
  const h = Math.floor(diff / 3600_000);
  const label = h >= 24 ? `${Math.floor(h / 24)} 天后过期` : `${h} 小时后过期`;
  if (h <= 24) return <Chip tone="danger" label={label} />;
  if (h <= 72) return <Chip tone="warn" label={label} />;
  return <Chip tone="ok" label={label} />;
}

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

function fmtSize(bytes: number): string {
  if (bytes >= 1048576) return `${(bytes / 1048576).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

function SiteCard({
  site, index, copied, onCopy, onUpdate, onDelete,
}: {
  site: SiteDTO;
  index: number;
  copied: boolean;
  onCopy: () => void;
  onUpdate: () => void;
  onDelete: () => void;
}) {
  return (
    <motion.li
      layout
      initial={{ opacity: 0, y: 18, scale: 0.96 }}
      animate={{ opacity: 1, y: 0, scale: 1 }}
      exit={{ opacity: 0, scale: 0.92, y: -10, filter: "blur(4px)" }}
      transition={{ ...spring, delay: Math.min(index * 0.06, 0.3) }}
      whileHover={{ y: -3 }}
      className="glass flex flex-col rounded-[20px] border border-white/40 p-5 shadow-[var(--card-shadow)] dark:border-white/10"
    >
      {/* 头部：头像 + 名称 + 属性徽章 + 到期状态 */}
      <div className="flex items-start gap-3">
        <span className="flex size-10 shrink-0 select-none items-center justify-center rounded-[12px] bg-primary/[0.06] text-[15px] font-semibold text-foreground ring-1 ring-foreground/[0.06] dark:bg-white/[0.07] dark:ring-white/10">
          {(site.name || site.id).trim().slice(0, 1).toUpperCase() || "P"}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-1.5">
            <h2 className="min-w-0 truncate text-[15px] font-semibold tracking-[-0.01em]">
              {site.name || site.id}
            </h2>
            {site.has_password && (
              <span className="inline-flex h-5 shrink-0 items-center gap-1 rounded-full bg-muted px-2 text-[11px] font-medium text-muted-foreground">
                <Lock className="size-2.5" />密码
              </span>
            )}
            {site.spa && (
              <span className="inline-flex h-5 shrink-0 items-center rounded-full bg-muted px-2 text-[11px] font-medium text-muted-foreground">
                SPA
              </span>
            )}
          </div>
          <p className="mt-1 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
            <span className="font-mono">{site.id}</span>
            <span className="opacity-40">·</span>
            <span>v{site.version}</span>
            <span className="opacity-40">·</span>
            <span className="truncate">更新于 {relTime(site.updated_at)}</span>
          </p>
        </div>
        <ExpiryChip expiresAt={site.expires_at} />
      </div>

      {/* 链接条：点击即复制 */}
      <button
        type="button"
        onClick={onCopy}
        title="点击复制链接"
        className="group/link mt-3.5 flex w-full items-center gap-2 rounded-[12px] border border-border/70 bg-background/60 px-3 py-2 text-left transition-colors hover:border-primary/40 hover:bg-background"
      >
        <Link2 className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground transition-colors group-hover/link:text-foreground">
          {site.url}
        </span>
        <AnimatePresence mode="wait" initial={false}>
          <motion.span
            key={copied ? "ok" : "copy"}
            initial={{ opacity: 0, scale: 0.6 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.6 }}
            transition={spring}
            className="shrink-0"
          >
            {copied ? (
              <Check className="size-3.5 text-emerald-600 dark:text-emerald-400" />
            ) : (
              <Copy className="size-3.5 text-muted-foreground opacity-0 transition-opacity group-hover/link:opacity-100" />
            )}
          </motion.span>
        </AnimatePresence>
      </button>

      {/* 底部操作区 */}
      <div className="mt-4 flex items-center gap-1.5 border-t border-border/60 pt-3.5">
        <Button size="sm" className="rounded-full px-3.5" onClick={onCopy}>
          <AnimatePresence mode="wait" initial={false}>
            <motion.span
              key={copied ? "check" : "copy"}
              initial={{ scale: 0.4, opacity: 0, rotate: -30 }}
              animate={{ scale: 1, opacity: 1, rotate: 0 }}
              exit={{ scale: 0.4, opacity: 0, rotate: 30 }}
              transition={spring}
              className="flex items-center"
            >
              {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            </motion.span>
          </AnimatePresence>
          {copied ? "已复制" : "复制链接"}
        </Button>
        <a href={site.url} target="_blank" rel="noreferrer">
          <Button size="sm" variant="secondary" className="rounded-full px-3.5">
            <ExternalLink className="size-3.5" />打开
          </Button>
        </a>
        <span className="flex-1" />
        <Button size="icon-sm" variant="ghost" aria-label="覆盖更新" onClick={onUpdate}>
          <UploadCloud className="size-4" />
        </Button>
        <Button
          size="icon-sm"
          variant="ghost"
          aria-label="删除"
          className="text-destructive hover:text-destructive"
          onClick={onDelete}
        >
          <Trash2 className="size-4" />
        </Button>
      </div>
    </motion.li>
  );
}

function SiteSkeleton() {
  return (
    <div className="glass rounded-[20px] border border-white/40 p-5 shadow-[var(--card-shadow)] dark:border-white/10">
      <div className="flex items-start gap-3">
        <div className="size-10 animate-pulse rounded-[12px] bg-muted" />
        <div className="flex-1 space-y-2 pt-1">
          <div className="h-3.5 w-1/3 animate-pulse rounded-full bg-muted" />
          <div className="h-3 w-1/2 animate-pulse rounded-full bg-muted/70" />
        </div>
        <div className="h-6 w-20 animate-pulse rounded-full bg-muted/70" />
      </div>
      <div className="mt-4 h-9 animate-pulse rounded-[12px] bg-muted/60" />
      <div className="mt-4 flex gap-2 border-t border-border/60 pt-3.5">
        <div className="h-8 w-20 animate-pulse rounded-full bg-muted" />
        <div className="h-8 w-16 animate-pulse rounded-full bg-muted/70" />
      </div>
    </div>
  );
}

function Brand() {
  return (
    <div className="flex items-center gap-2.5">
      <span className="flex size-6 items-center justify-center rounded-[7px] bg-primary text-[11px] font-bold text-primary-foreground shadow-sm">
        p
      </span>
      <span className="text-[15px] font-semibold tracking-[-0.02em]">pageshare</span>
      <span className="text-[13px] text-muted-foreground">静态页分享</span>
    </div>
  );
}

const TTL_PRESETS = [
  { value: "72h", label: "72 小时" },
  { value: "7d", label: "7 天" },
  { value: "30d", label: "30 天" },
] as const;

function UploadDialog({
  target,
  busy,
  onClose,
  onSubmit,
}: {
  target: UploadTarget;
  busy: boolean;
  onClose: () => void;
  onSubmit: (opts: PublishOptions) => void;
}) {
  const [name, setName] = useState(target.overwrite?.name ?? "");
  const [ttl, setTtl] = useState("");
  const [password, setPassword] = useState("");
  const [spa, setSpa] = useState(target.overwrite?.spa ?? false);
  const overwrite = target.overwrite;
  const total = target.files.reduce((acc, f) => acc + f.file.size, 0);
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-md rounded-[20px]">
        <DialogHeader>
          <DialogTitle>{overwrite ? `覆盖更新「${overwrite.name || overwrite.id}」` : "发布新站点"}</DialogTitle>
          <DialogDescription>
            已选择 {target.files.length} 个文件 · {fmtSize(total)}，将打包为 zip 上传。
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-1">
          <div className="grid gap-1.5">
            <Label htmlFor="ps-name">站点名称（可选）</Label>
            <Input id="ps-name" placeholder="如：Q3 路演页" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ps-ttl">有效期（可选）</Label>
            <Input
              id="ps-ttl"
              placeholder={overwrite ? "保持不变" : "永久，如 72h、7d"}
              value={ttl}
              onChange={(e) => setTtl(e.target.value)}
            />
            <div className="mt-0.5 flex flex-wrap gap-1.5">
              {TTL_PRESETS.map((p) => (
                <button
                  key={p.value}
                  type="button"
                  onClick={() => setTtl((v) => (v === p.value ? "" : p.value))}
                  className={cn(
                    "h-7 rounded-full border px-3 text-xs font-medium transition-colors",
                    ttl === p.value
                      ? "border-primary bg-primary text-primary-foreground"
                      : "border-border bg-secondary/60 text-secondary-foreground hover:bg-secondary",
                  )}
                >
                  {p.label}
                </button>
              ))}
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ps-pass">访问密码（可选）</Label>
            <Input
              id="ps-pass"
              type="password"
              placeholder={overwrite?.has_password ? "留空保持不变，输入空格清除" : "留空则无需密码"}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          <div className="rounded-[14px] border border-border/70 bg-secondary/30 px-3.5 py-3">
            <Checkbox
              checked={spa}
              onCheckedChange={setSpa}
              label="SPA 站点"
              aria-label="SPA 站点"
            />
            <p className="mt-1 pl-8 text-xs leading-relaxed text-muted-foreground">
              路由未命中时回退入口页，React Router 等客户端路由勾选。
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="secondary" className="h-9 px-4" onClick={onClose} disabled={busy}>取消</Button>
          <Button className="h-9 px-4" disabled={busy} onClick={() => onSubmit({ name, ttl, password: password.trim(), spa })}>
            {busy ? "上传中…" : overwrite ? "覆盖发布" : "发布"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
