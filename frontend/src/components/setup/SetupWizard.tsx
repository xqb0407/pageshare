import { useCallback, useEffect, useRef, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import {
  ArrowLeft, ArrowRight, Check, Cloud, Copy, Database, Folder,
  Globe, HardDrive, KeyRound, RefreshCw, ShieldCheck, TriangleAlert,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Button as MotionButton } from "@/components/motion/button";
import { BouncyAccordion } from "@/components/motion/bouncy-accordion";
import { Loader } from "@/components/motion/loader";
import { RadioGroup, RadioGroupItem } from "@/components/motion/radio";
import { ThemeToggle } from "@/components/motion/theme-toggle";
import { Input as MotionInput } from "@/components/motion/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/motion/tabs";
import { ActionSwapButton } from "@/components/motion/action-swap";
import {
  ApiError, completeSetup, fetchSetupStatus,
  type SetupCompletePayload, type SetupStatus,
} from "@/lib/api";
import { cn } from "@/lib/utils";

// 与登录页一致的弹簧手感
const spring = { type: "spring" as const, stiffness: 320, damping: 28 };

/**
 * 卡片高度弹簧：ResizeObserver 追踪内容实际高度，外层 motion.div 用弹簧过渡。
 * 步骤切换、存储后端切换、token 模式切换等所有高度变化都由此平滑化。
 */
function useAutoHeight<T extends HTMLElement>() {
  const ref = useRef<T | null>(null);
  const [height, setHeight] = useState<number | "auto">("auto");
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(() => {
      const h = el.offsetHeight;
      setHeight((prev) => (prev === h ? prev : h));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return { ref, height };
}

const STEPS = [
  { title: "管理凭据", desc: "设置登录管理台与调用 MCP 的 token" },
  { title: "分享地址", desc: "决定访客拿到的分享链接长什么样" },
  { title: "存储", desc: "选择站点文件存放在哪里" },
] as const;

type Backend = "disk" | "s3" | "mem";

const BACKENDS: { value: Backend; icon: typeof Cloud; label: string; desc: string }[] = [
  { value: "disk", icon: HardDrive, label: "本地目录", desc: "站点文件存放在服务器磁盘，最简单可靠" },
  { value: "s3", icon: Cloud, label: "S3 / R2 / MinIO", desc: "兼容 S3 API 的对象存储，适合多机与备份" },
  { value: "mem", icon: Database, label: "内存", desc: "仅用于试跑，服务重启后站点全部丢失" },
];

/** 重启确认轮询：每 1s 一次，最多 20 次。 */
const POLL_MAX_TRIES = 20;

export default function SetupWizard() {
  const [status, setStatus] = useState<SetupStatus | null>(null);
  const [loadError, setLoadError] = useState("");
  const [step, setStep] = useState(0);
  const [phase, setPhase] = useState<"form" | "done" | "timeout">("form");
  const [stepError, setStepError] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  const [authToken, setAuthToken] = useState("");
  const [tokenMode, setTokenMode] = useState<"auto" | "custom">("auto");
  const [customToken, setCustomToken] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [wildcard, setWildcard] = useState("");
  const [backend, setBackend] = useState<Backend>("disk");
  const [root, setRoot] = useState("./data");
  const [s3, setS3] = useState({
    endpoint: "", region: "", bucket: "",
    access_key_id: "", secret_access_key: "", prefix: "",
  });
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const { ref: contentRef, height: contentHeight } = useAutoHeight<HTMLDivElement>();

  const setFieldError = (key: string, msg: string) =>
    setFieldErrors((prev) => ({ ...prev, [key]: msg }));

  useEffect(() => {
    let alive = true;
    fetchSetupStatus()
      .then((s) => {
        if (!alive) return;
        // 已配置：服务以正常模式运行，向导直接自毁
        if (!s.needs_setup) {
          window.location.replace("/admin/");
          return;
        }
        setStatus(s);
        setAuthToken(s.suggested_auth_token);
        setBaseUrl(s.suggested_base_url);
      })
      .catch((e) => {
        if (!alive) return;
        // setup API 已关闭（404）= 已配置的正常模式，直接回管理台
        if (e instanceof ApiError && e.status === 404) {
          window.location.replace("/admin/");
          return;
        }
        setLoadError(String((e as Error)?.message ?? e));
      });
    return () => {
      alive = false;
      if (pollRef.current) clearInterval(pollRef.current);
    };
  }, []);

  // 服务重启后 setup API 消失（404）或 needs_setup=false 都算成功；连接拒绝则继续等
  const startRestartPolling = useCallback(() => {
    let tries = 0;
    pollRef.current = setInterval(() => {
      tries += 1;
      if (tries > POLL_MAX_TRIES) {
        clearInterval(pollRef.current!);
        pollRef.current = null;
        setPhase("timeout");
        return;
      }
      fetchSetupStatus()
        .then((s) => {
          if (!s.needs_setup) {
            clearInterval(pollRef.current!);
            pollRef.current = null;
            window.location.href = "/admin/";
          }
        })
        .catch((e) => {
          if (e instanceof ApiError && e.status === 404) {
            clearInterval(pollRef.current!);
            pollRef.current = null;
            window.location.href = "/admin/";
          }
        });
    }, 1000);
  }, []);

  /** 实际提交用的 token：自动生成模式取预生成值，自定义模式取用户输入。 */
  const effectiveToken = (tokenMode === "auto" ? authToken : customToken).trim();

  /** 浏览器 crypto 生成 32 位 hex，供"重新生成"按钮使用。 */
  const randomToken = () =>
    Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
      b.toString(16).padStart(2, "0"),
    ).join("");

  const nextFrom = (current: number) => {
    setStepError("");
    setFieldErrors({});
    if (current === 0) {
      const t = effectiveToken;
      if (t === "") {
        setFieldError("token", tokenMode === "custom" ? "自定义 token 不能为空" : "管理 token 不能为空");
        return;
      }
      if (/\s/.test(t)) {
        setFieldError("token", "token 不能包含空格");
        return;
      }
      if (tokenMode === "custom" && t.length < 8) {
        setFieldError("token", "自定义 token 至少 8 个字符");
        return;
      }
    }
    if (current === 1 && !/^https?:\/\//.test(baseUrl.trim())) {
      setFieldError("url", "分享地址需以 http:// 或 https:// 开头");
      return;
    }
    if (current === 2) {
      if (backend === "disk" && root.trim() === "") {
        setFieldError("root", "本地目录不能为空");
        return;
      }
      if (backend === "s3" && s3.bucket.trim() === "") {
        setFieldError("bucket", "S3 存储需要填写桶名（bucket）");
        return;
      }
      void submit();
      return;
    }
    setStep(current + 1);
  };

  const submit = async () => {
    if (!status) return;
    setBusy(true);
    try {
      const payload: SetupCompletePayload = {
        auth_token: effectiveToken,
        cookie_secret: status.suggested_cookie_secret,
        public_base_url: baseUrl.trim(),
        site_wildcard_host: wildcard.trim(),
        listen: status.listen,
        db_path: "sites.json",
        max_upload_mb: 100,
        storage:
          backend === "disk"
            ? { backend, root: root.trim() }
            : backend === "s3"
              ? {
                  backend,
                  endpoint: s3.endpoint.trim(),
                  region: s3.region.trim(),
                  bucket: s3.bucket.trim(),
                  access_key_id: s3.access_key_id.trim(),
                  secret_access_key: s3.secret_access_key,
                  prefix: s3.prefix.trim(),
                }
              : { backend },
      };
      await completeSetup(payload);
      setPhase("done");
      startRestartPolling();
    } catch (e) {
      const msg = String((e as Error)?.message ?? e);
      // 校验失败等错误留在步骤级提示
      setStepError(msg);
    } finally {
      setBusy(false);
    }
  };

  const copyToken = async () => {
    await navigator.clipboard.writeText(effectiveToken);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  if (loadError) {
    return (
      <div className="relative z-10 flex min-h-dvh items-center justify-center p-6">
        <Card>
          <div className="flex flex-col items-center py-8 text-center">
            <span className="flex size-12 items-center justify-center rounded-2xl bg-destructive/10 text-destructive">
              <TriangleAlert className="size-5.5" />
            </span>
            <p className="mt-5 text-[16px] font-medium tracking-[-0.01em]">引导状态加载失败</p>
            <p className="mt-2 max-w-[320px] text-[13px] leading-relaxed text-destructive">{loadError}</p>
            <p className="mt-1.5 text-[13px] text-muted-foreground">请确认服务已启动后刷新此页。</p>
          </div>
        </Card>
      </div>
    );
  }

  return (
    <div className="relative z-10 grid min-h-dvh lg:grid-cols-[440px_1fr]">
      <SidePanel
        step={step}
        configPath={status?.config_path}
        visible={status !== null}
        onStepClick={(i) => {
          setStepError("");
          setFieldErrors({});
          setStep(i);
        }}
      />
      <div className="relative flex justify-center p-6 sm:p-10">
        <motion.div
          initial={{ opacity: 0, y: 24, scale: 0.96 }}
          animate={{ opacity: 1, y: 0, scale: 1 }}
          transition={{ type: "spring", stiffness: 260, damping: 22, delay: 0.05 }}
          className="m-auto w-full max-w-lg"
        >
        <Card>
          <motion.div
            animate={{ height: contentHeight }}
            transition={spring}
            style={{ overflow: "hidden" }}
          >
            <div ref={contentRef} className="flex flex-col gap-7">
          {phase === "form" && (
            <>
              {/* 头部：品牌 + 主题切换（仅窄屏；双栏时主题在左栏面板） */}
              <div className="flex items-center justify-between gap-2 lg:hidden">
                <div className="flex items-center gap-2.5">
                  <span className="flex size-6 items-center justify-center rounded-xl bg-primary text-[11px] font-bold text-primary-foreground shadow-sm">
                    p
                  </span>
                  <span className="text-[15px] font-medium tracking-[-0.02em]">pageshare</span>
                  <span className="text-[13px] text-muted-foreground">初始化向导</span>
                </div>
                <ThemeToggle
                  variant="circle-blur"
                  start="top-right"
                  aria-label="切换主题"
                  className="size-9 rounded-full hover:bg-accent hover:text-accent-foreground"
                  iconClassName="size-4"
                />
              </div>

              {status === null ? (
                <div className="flex flex-col items-center py-14 text-muted-foreground">
                  <Loader variant="spinner" size={24} label="正在加载引导状态" className="text-muted-foreground" />
                  <p className="mt-4 text-[13px]">正在加载引导状态…</p>
                </div>
              ) : (
                <>
                  {/* 进度：步骤名 + 细进度轨（双栏时进度在左栏，此处仅窄屏） */}
                  <div className="xl:hidden">
                    <div className="flex items-baseline justify-between">
                      <motion.span
                        key={step}
                        initial={{ opacity: 0, y: 6 }}
                        animate={{ opacity: 1, y: 0 }}
                        transition={spring}
                        className="text-[15px] font-medium tracking-[-0.01em]"
                      >
                        {STEPS[step].title}
                      </motion.span>
                      <span className="font-mono text-[11.5px] text-muted-foreground">
                        {String(step + 1).padStart(2, "0")} / {String(STEPS.length).padStart(2, "0")}
                      </span>
                    </div>
                    <div className="mt-2.5 flex gap-1.5">
                      {STEPS.map((s, i) => (
                        <span
                          key={s.title}
                          className="h-[3px] flex-1 overflow-hidden rounded-full bg-foreground/[0.07]"
                        >
                          <motion.span
                            className="block h-full rounded-full bg-foreground/70"
                            initial={false}
                            animate={{ width: i < step ? "100%" : i === step ? "50%" : "0%" }}
                            transition={spring}
                          />
                        </span>
                      ))}
                    </div>
                  </div>

                  <AnimatePresence mode="wait" initial={false}>
                    <motion.div
                      key={step}
                      initial={{ opacity: 0, x: 28 }}
                      animate={{ opacity: 1, x: 0 }}
                      exit={{ opacity: 0, x: -28 }}
                      transition={spring}
                      className="flex flex-col gap-4"
                    >
                      {step === 0 && (
                        <StepToken
                          mode={tokenMode}
                          token={authToken}
                          customToken={customToken}
                          copied={copied}
                          tokenError={fieldErrors.token}
                          onModeChange={(m) => {
                            setTokenMode(m);
                            setFieldErrors((p) => ({ ...p, token: "" }));
                          }}
                          onCustomTokenChange={(v) => {
                            setCustomToken(v);
                            setFieldErrors((p) => ({ ...p, token: "" }));
                          }}
                          onRegenerate={() => setAuthToken(randomToken())}
                          onCopy={() => void copyToken()}
                        />
                      )}
                      {step === 1 && (
                        <StepShareURL
                          baseUrl={baseUrl}
                          wildcard={wildcard}
                          urlError={fieldErrors.url}
                          onBaseURLChange={(v) => {
                            setBaseUrl(v);
                            setFieldErrors((p) => ({ ...p, url: "" }));
                          }}
                          onWildcardChange={setWildcard}
                        />
                      )}
                      {step === 2 && (
                        <StepStorage
                          backend={backend}
                          root={root}
                          s3={s3}
                          rootError={fieldErrors.root}
                          bucketError={fieldErrors.bucket}
                          onBackendChange={(b) => {
                            setBackend(b);
                            setFieldErrors({});
                          }}
                          onRootChange={(v) => {
                            setRoot(v);
                            setFieldErrors((p) => ({ ...p, root: "" }));
                          }}
                          onS3Change={(k, v) => {
                            setS3((s) => ({ ...s, [k]: v }));
                            if (k === "bucket") setFieldErrors((p) => ({ ...p, bucket: "" }));
                          }}
                        />
                      )}

                      <AnimatePresence mode="popLayout">
                        {stepError && (
                          <motion.div
                            key={stepError}
                            initial={{ opacity: 0, y: -6 }}
                            animate={{ opacity: 1, x: [0, -10, 10, -6, 6, 0] }}
                            exit={{ opacity: 0 }}
                            transition={{ duration: 0.45, ease: "easeOut" }}
                            className="flex items-start gap-2 rounded-[12px] border border-destructive/25 bg-destructive/[0.06] px-3.5 py-2.5"
                          >
                            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
                            <p className="text-[13px] leading-relaxed text-destructive">{stepError}</p>
                          </motion.div>
                        )}
                      </AnimatePresence>

                      <div className="mt-1 flex items-center gap-2.5">
                        {step > 0 && (
                          <Button
                            variant="ghost"
                            className="h-11 rounded-full px-4 text-muted-foreground hover:text-foreground"
                            onClick={() => {
                              setStepError("");
                              setFieldErrors({});
                              setStep(step - 1);
                            }}
                          >
                            <ArrowLeft className="size-4" />
                            上一步
                          </Button>
                        )}
                        <MotionButton
                          className="h-11 flex-1 rounded-full text-sm font-medium"
                          disabled={busy}
                          onClick={() => nextFrom(step)}
                        >
                          {step < 2 ? (
                            <>
                              继续
                              <ArrowRight className="size-4" />
                            </>
                          ) : busy ? (
                            "正在写入配置…"
                          ) : (
                            <>
                              <ShieldCheck className="size-4" />
                              完成配置
                            </>
                          )}
                        </MotionButton>
                      </div>
                    </motion.div>
                  </AnimatePresence>
                </>
              )}
            </>
          )}

          {phase === "done" && (
            <div className="flex flex-col items-center py-10 text-center">
              <motion.span
                initial={{ scale: 0.4, opacity: 0 }}
                animate={{ scale: 1, opacity: 1 }}
                transition={spring}
                className="flex size-14 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
              >
                <Check className="size-6.5" />
              </motion.span>
              <p className="mt-6 text-[19px] font-light tracking-[-0.02em]">一切就绪</p>
              <p className="mt-2 max-w-[320px] text-[13.5px] leading-relaxed text-muted-foreground">
                配置已写入{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono text-[12px]">{status?.config_path ?? "pageshare.json"}</code>
                ，服务正在重启，马上带你进入管理台。
              </p>
              <Loader
                variant="spinner"
                size={16}
                label="正在等待服务重启"
                className="mt-7 text-muted-foreground"
              />
            </div>
          )}

          {phase === "timeout" && (
            <div className="flex flex-col items-center py-10 text-center">
              <span className="flex size-14 items-center justify-center rounded-full bg-amber-500/15 text-amber-600 dark:text-amber-400">
                <RefreshCw className="size-6.5" />
              </span>
              <p className="mt-6 text-[19px] font-light tracking-[-0.02em]">只差手动重启</p>
              <p className="mt-2 max-w-[320px] text-[13.5px] leading-relaxed text-muted-foreground">
                配置已成功写入，但没有检测到服务自动重启。手动重启后即可进入管理台，也可以在此继续等待检测。
              </p>
              <Button
                className="mt-7 h-11 rounded-full px-6"
                onClick={() => {
                  setPhase("done");
                  startRestartPolling();
                }}
              >
                <RefreshCw className="size-4" />
                重新检测
              </Button>
            </div>
          )}
            </div>
          </motion.div>
        </Card>
        </motion.div>
      </div>
    </div>
  );
}

/* ── 左侧深色品牌面板（Clerk/Linear 式分割）：墨色底 + 白字叙事 + 可回退步骤 ── */

function SidePanel(props: {
  step: number;
  configPath?: string;
  visible: boolean;
  onStepClick: (i: number) => void;
}) {
  return (
    <motion.aside
      initial={{ opacity: 0 }}
      animate={{ opacity: props.visible ? 1 : 0 }}
      transition={{ duration: 0.5, ease: "easeOut" }}
      className="relative hidden flex-col justify-between overflow-hidden bg-[#161512] p-10 lg:flex dark:bg-[#121110]"
    >
      {/* 面板微光：右上极淡的白色光晕，避免死黑 */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 bg-[radial-gradient(ellipse_at_top_right,rgba(255,255,255,0.06),transparent_55%)]"
      />

      {/* 主题切换：面板右上角 */}
      <div className="absolute right-6 top-6 z-20">
        <ThemeToggle
          variant="circle-blur"
          start="top-right"
          aria-label="切换主题"
          className="size-9 rounded-full text-white hover:bg-white/10"
          iconClassName="size-4"
        />
      </div>

      <div className="relative flex items-center gap-3">
        <span className="flex size-10 items-center justify-center rounded-xl bg-white text-[17px] font-bold text-[#161512]">
          p
        </span>
        <div>
          <p className="text-[17px] font-medium tracking-[-0.02em] text-white">pageshare</p>
          <p className="text-[12.5px] text-white/50">自托管站点分享</p>
        </div>
      </div>

      <div className="relative">
        <h1 className="text-[38px] font-light leading-[1.14] tracking-[-0.03em] text-white">
          三分钟
          <br />
          完成部署<span className="text-white/40">.</span>
        </h1>
        <p className="mt-4 max-w-[320px] text-[13.5px] leading-relaxed text-white/60">
          配置一次，之后发布静态站点只需一条命令。向导会生成配置文件，服务随后自动重启进入正常模式。
        </p>
      </div>

      <ol className="relative flex flex-col">
        {STEPS.map((s, i) => {
          const state = i < props.step ? "done" : i === props.step ? "active" : "todo";
          const clickable = i < props.step;
          return (
            <li key={s.title} className="relative flex gap-3.5 pb-6 last:pb-0">
              {i < STEPS.length - 1 && (
                <span className="absolute left-[11px] top-7 h-[calc(100%-1.75rem)] w-px overflow-hidden bg-white/15">
                  <motion.span
                    className="block h-full w-full origin-top bg-white/70"
                    initial={false}
                    animate={{ scaleY: i < props.step ? 1 : 0 }}
                    transition={spring}
                  />
                </span>
              )}
              <span
                className={cn(
                  "relative z-10 flex size-[22px] shrink-0 items-center justify-center rounded-full border text-[11px] font-medium transition-colors duration-300",
                  state === "active" && "border-white bg-white text-[#161512]",
                  state === "done" && "border-white/25 bg-white/15 text-white",
                  state === "todo" && "border-white/20 text-white/50",
                )}
              >
                {state === "done" ? <Check className="size-3" /> : i + 1}
              </span>
              <button
                type="button"
                disabled={!clickable}
                onClick={() => props.onStepClick(i)}
                className="text-left"
              >
                <p
                  className={cn(
                    "text-[14px] leading-none transition-colors",
                    state === "todo" ? "text-white/50" : "text-white",
                    clickable && "hover:text-white/70",
                  )}
                >
                  {s.title}
                </p>
                <p className="mt-1.5 text-[12.5px] leading-snug text-white/40">{s.desc}</p>
              </button>
            </li>
          );
        })}
      </ol>

      <div className="relative flex items-center gap-2.5 text-[12px]">
        <span className="text-white/40">配置写入</span>
        <code className="rounded-md bg-white/[0.08] px-1.5 py-0.5 font-mono text-[11.5px] text-white/70">
          {props.configPath ?? "pageshare.json"}
        </code>
        <span className="text-white/40">· 全程约 1 分钟</span>
      </div>
    </motion.aside>
  );
}

/* ── 卡片容器：与登录页同款磨砂 ── */

function Card(props: { children: React.ReactNode }) {
  return (
    <div className="glass-strong rounded-[24px] border border-white/40 p-7 shadow-[var(--card-shadow)] dark:border-white/10 sm:p-8">
      {props.children}
    </div>
  );
}

/* ── 各步骤表单 ── */

function StepHeading(props: { title: string; desc: React.ReactNode }) {
  return (
    <div>
      <h1 className="text-[26px] font-light leading-tight tracking-[-0.03em]">{props.title}</h1>
      <p className="mt-2 text-[13.5px] leading-relaxed text-muted-foreground">{props.desc}</p>
    </div>
  );
}

function StepToken(props: {
  mode: "auto" | "custom";
  token: string;
  customToken: string;
  copied: boolean;
  tokenError?: string;
  onModeChange: (m: "auto" | "custom") => void;
  onCustomTokenChange: (v: string) => void;
  onRegenerate: () => void;
  onCopy: () => void;
}) {
  return (
    <>
      <StepHeading
        title="管理凭据"
        desc="这是登录管理台与调用 MCP 的唯一凭据。可以使用预生成的随机值，也可以设置为自己的 token。"
      />

      {/* 生成方式：motion 分段控件（layoutId 滑动指示器） */}
      <Tabs
        value={props.mode}
        onValueChange={(v) => props.onModeChange(v as "auto" | "custom")}
        variant="pill"
        className="w-fit"
      >
        <TabsList>
          <TabsTrigger value="auto">自动生成</TabsTrigger>
          <TabsTrigger value="custom">自定义</TabsTrigger>
        </TabsList>
      </Tabs>

      {props.mode === "auto" ? (
        <div className="grid gap-2">
          <div className="flex items-start gap-2">
            <MotionInput
              label="auth_token"
              readOnly
              value={props.token}
              className="flex-1"
              classNames={{ input: "font-mono text-[13px] text-muted-foreground" }}
            />
            <div className="flex shrink-0 gap-2 pt-[26px]">
              <Button
                type="button"
                variant="secondary"
                className="h-11 rounded-full border border-border px-4"
                onClick={props.onRegenerate}
                aria-label="重新生成 token"
              >
                <RefreshCw className="size-4" />
                重新生成
              </Button>
              <ActionSwapButton
                items={[
                  { id: "copy", label: "复制", icon: <Copy className="size-4" /> },
                  { id: "copied", label: "已复制", icon: <Check className="size-4" /> },
                ]}
                value={props.copied ? "copied" : "copy"}
                onClick={() => props.onCopy()}
                animation="roll"
                aria-label="复制 token"
                className="h-11 rounded-full px-4"
              />
            </div>
          </div>
          <p className="px-1 text-xs leading-relaxed text-muted-foreground">
            建议现在就复制保存到密码管理器；写入配置后也可在 pageshare.json 中修改。
          </p>
        </div>
      ) : (
        <MotionInput
          label="auth_token"
          leftIcon={<KeyRound />}
          value={props.customToken}
          onChange={props.onCustomTokenChange}
          error={props.tokenError}
          placeholder="至少 8 个字符，不能包含空格"
          classNames={{ input: "font-mono text-[13px]" }}
        />
      )}
      {props.mode === "custom" && (
        <p className="px-1 text-xs leading-relaxed text-muted-foreground">
          请使用足够长的随机字符串，避免使用生日、单词等易猜内容；复制保存好，登录管理台时要输入它。
        </p>
      )}
    </>
  );
}

function StepShareURL(props: {
  baseUrl: string;
  wildcard: string;
  urlError?: string;
  onBaseURLChange: (v: string) => void;
  onWildcardChange: (v: string) => void;
}) {
  return (
    <>
      <StepHeading
        title="分享地址"
        desc="访客拿到的分享链接都由这个基地址拼接而成，填对外可访问的域名即可。"
      />
      <MotionInput
        label="public_base_url"
        leftIcon={<Globe />}
        placeholder="https://share.example.com"
        value={props.baseUrl}
        onChange={props.onBaseURLChange}
        error={props.urlError}
        classNames={{ label: "text-[13px]", input: "font-mono text-[13px]" }}
      />
      <BouncyAccordion
        classNames={{
          item: "border border-border/70 bg-secondary/30",
          trigger: "min-h-0 px-4 py-3",
          title: "text-[13px]",
          description: "text-[13px]",
        }}
        items={[
          {
            id: "wildcard",
            title: "高级：泛域名整域托管",
            description: (
              <div className="grid gap-2.5">
                <MotionInput
                  label="site_wildcard_host（可选）"
                  placeholder="如 s.example.com，留空关闭"
                  value={props.wildcard}
                  onChange={props.onWildcardChange}
                  classNames={{ label: "text-[13px]", input: "font-mono text-[13px]" }}
                />
                <p className="text-xs leading-relaxed text-muted-foreground">
                  配置后站点按 {"{id}.主机名"} 整域托管，需要泛域名解析与证书配合。
                </p>
              </div>
            ),
          },
        ]}
      />
    </>
  );
}

const S3_FIELDS: { key: keyof SetupWizardS3; label: string; placeholder: string; optional?: boolean; full?: boolean }[] = [
  { key: "endpoint", label: "endpoint", placeholder: "留空用 AWS 默认，R2/MinIO 必填", full: true },
  { key: "region", label: "region", placeholder: "如 auto（R2）" },
  { key: "bucket", label: "bucket", placeholder: "桶名" },
  { key: "access_key_id", label: "access_key_id", placeholder: "Access Key ID" },
  { key: "secret_access_key", label: "secret_access_key", placeholder: "Secret Access Key" },
  { key: "prefix", label: "prefix", placeholder: "如 pageshare/", optional: true, full: true },
];

type SetupWizardS3 = {
  endpoint: string;
  region: string;
  bucket: string;
  access_key_id: string;
  secret_access_key: string;
  prefix: string;
};

function StepStorage(props: {
  backend: Backend;
  root: string;
  s3: SetupWizardS3;
  rootError?: string;
  bucketError?: string;
  onBackendChange: (b: Backend) => void;
  onRootChange: (v: string) => void;
  onS3Change: (key: keyof SetupWizardS3, v: string) => void;
}) {
  return (
    <>
      <StepHeading title="存储" desc="选择已发布站点的文件存放位置，之后也可以在配置文件中更换。" />
      <RadioGroup
        value={props.backend}
        onValueChange={(v) => props.onBackendChange(v as Backend)}
        className="grid gap-2"
      >
        {BACKENDS.map((b) => {
          const active = props.backend === b.value;
          return (
            <div
              key={b.value}
              onClick={(e) => {
                // 圆点按钮自己会走 RadioGroup 的 onValueChange，这里只兜整行点击
                if ((e.target as HTMLElement).closest('[role="radio"]')) return;
                props.onBackendChange(b.value);
              }}
              className={cn(
                "flex cursor-pointer items-center gap-3.5 rounded-[14px] border px-4 py-3.5 text-left transition-all",
                active
                  ? "border-primary/60 bg-primary/[0.06]"
                  : "border-border/70 bg-secondary/30 hover:border-primary/35 hover:bg-secondary/50",
              )}
            >
              <span
                className={cn(
                  "flex size-9 shrink-0 items-center justify-center rounded-[10px] transition-colors",
                  active ? "bg-primary text-primary-foreground" : "bg-muted text-muted-foreground",
                )}
              >
                <b.icon className="size-4.5" />
              </span>
              <span className="min-w-0 flex-1">
                <span className={cn("block text-[14px] font-medium", !active && "text-foreground")}>
                  {b.label}
                </span>
                <span className="mt-0.5 block text-[12px] leading-snug text-muted-foreground">{b.desc}</span>
              </span>
              {/* label 提供可访问名称，[&>span]:sr-only 隐藏行内重复的可见文字 */}
              <RadioGroupItem value={b.value} label={b.label} className="shrink-0 [&>span]:sr-only" />
            </div>
          );
        })}
      </RadioGroup>

      <AnimatePresence mode="wait" initial={false}>
        {props.backend === "mem" && (
          <motion.div
            key="mem-warn"
            initial={{ opacity: 0, y: -6 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -6 }}
            transition={spring}
            className="flex items-start gap-2 rounded-[12px] border border-amber-500/25 bg-amber-500/[0.08] px-3.5 py-2.5"
          >
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
            <p className="text-[12.5px] leading-relaxed text-amber-700 dark:text-amber-400">
              内存存储只适合本地试跑：重启后所有已发布站点都会丢失，正式部署请选择本地目录或对象存储。
            </p>
          </motion.div>
        )}
        {props.backend === "disk" && (
          <motion.div
            key="disk"
            initial={{ opacity: 0, y: 10 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -10 }}
            transition={spring}
          >
            <MotionInput
              label="本地目录（root）"
              leftIcon={<Folder />}
              placeholder="./data"
              value={props.root}
              onChange={props.onRootChange}
              error={props.rootError}
              classNames={{ label: "text-[13px]", input: "font-mono text-[13px]" }}
            />
            <p className="mt-1.5 px-1 text-xs leading-relaxed text-muted-foreground">
              相对路径基于服务运行目录；目录不存在时会自动创建。
            </p>
          </motion.div>
        )}
        {props.backend === "s3" && (
          <motion.div
            key="s3"
            initial={{ opacity: 0, y: 10 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -10 }}
            transition={spring}
            className="grid gap-x-3 gap-y-2.5 sm:grid-cols-2"
          >
            {S3_FIELDS.map((f) => (
              <MotionInput
                key={f.key}
                label={f.optional ? `${f.label}（可选）` : f.label}
                type={f.key === "secret_access_key" ? "password" : "text"}
                placeholder={f.placeholder}
                value={props.s3[f.key]}
                onChange={(v) => props.onS3Change(f.key, v)}
                error={f.key === "bucket" ? props.bucketError : undefined}
                className={f.full ? "sm:col-span-2" : undefined}
                classNames={{ label: "font-mono text-[12.5px]", input: "font-mono text-[13px]" }}
              />
            ))}
          </motion.div>
        )}
      </AnimatePresence>
    </>
  );
}
