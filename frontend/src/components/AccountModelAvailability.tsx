import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type PropsWithChildren,
} from "react";
import {
  CheckCircle2,
  CircleHelp,
  CircleX,
  Clock3,
  ListChecks,
  Loader2,
  ShieldAlert,
  Sparkles,
} from "lucide-react";
import { api } from "../api";
import type { AccountRow } from "../types";
import {
  accountModelAvailability,
  isCodexModelAccount,
  type ModelAvailabilityState,
} from "../lib/accountModelAvailability";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";

const storageKey = "codex-account-watched-model";
const WatchedModel = createContext({
  model: "",
  models: [] as string[],
  loading: true,
  error: "",
  setModel: (_value: string) => {},
  refresh: () => {},
});

export function AccountModelAvailabilityProvider({
  children,
}: PropsWithChildren) {
  const [model, setValue] = useState(() => {
    try {
      return localStorage.getItem(storageKey) || "gpt-6-sol";
    } catch {
      return "gpt-6-sol";
    }
  });
  const [models, setModels] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    api
      .getModels()
      .then((res) => {
        if (active)
          setModels(
            res.models.filter((m) => !m.includes("image") && !m.includes("(")),
          );
      })
      .catch((e) => {
        if (active) setError(getErrorMessage(e));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [nonce]);
  const value = useMemo(
    () => ({
      model,
      models,
      loading,
      error,
      setModel: (next: string) => {
        setValue(next);
        try {
          localStorage.setItem(storageKey, next);
        } catch {
          /* browser storage unavailable */
        }
      },
      refresh: () => setNonce((n) => n + 1),
    }),
    [model, models, loading, error],
  );
  return (
    <WatchedModel.Provider value={value}>{children}</WatchedModel.Provider>
  );
}

const labels: Record<ModelAvailabilityState, string> = {
  available: "实测可用",
  unsupported: "不支持",
  listed: "清单可见",
  unknown: "未检测",
  configured: "已配置 · 未验证",
  stale: "待复测",
  throttled: "限流 · 待复核",
  error: "异常 · 待复核",
};
const icons = {
  available: CheckCircle2,
  unsupported: CircleX,
  listed: ListChecks,
  unknown: CircleHelp,
  configured: CircleHelp,
  stale: Clock3,
  throttled: Clock3,
  error: ShieldAlert,
};
const tones: Record<ModelAvailabilityState, string> = {
  available: "text-emerald-700 bg-emerald-500/10 dark:text-emerald-300",
  unsupported: "text-red-700 bg-red-500/10 dark:text-red-300",
  listed: "text-blue-700 bg-blue-500/10 dark:text-blue-300",
  unknown: "text-muted-foreground bg-muted",
  configured: "text-muted-foreground bg-muted",
  stale: "text-amber-700 bg-amber-500/10 dark:text-amber-300",
  throttled: "text-amber-700 bg-amber-500/10 dark:text-amber-300",
  error: "text-amber-700 bg-amber-500/10 dark:text-amber-300",
};

export function AccountModelAvailabilityBadge({
  account,
  onClick,
}: {
  account: AccountRow;
  onClick: () => void;
}) {
  const { model } = useContext(WatchedModel);
  if (!isCodexModelAccount(account) || !model) return null;
  const result = accountModelAvailability(account, model);
  const Icon = icons[result.state];
  const label = `${model} · ${labels[result.state]}${result.blocked ? " · 白名单未放行" : ""}`;
  const title = [
    label,
    result.observation
      ? `记录于 ${new Date(result.observation.observed_at * 1000).toLocaleString()}`
      : "没有检测记录，未检测不代表不支持",
    "点击配置支持模型；清单可见不等于实测成功，记录超过 24 小时需复测。",
  ].join("\n");
  return (
    <button
      type="button"
      onClick={(e) => {
        e.stopPropagation();
        onClick();
      }}
      title={title}
      aria-label={label}
      className={`inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-medium ${tones[result.state]}`}
      data-model-state={result.state}
    >
      <Icon className="size-3 shrink-0" aria-hidden />
      {model} · {labels[result.state]}
      {result.blocked && (
        <ShieldAlert
          className="size-3 text-amber-600"
          aria-label="白名单未放行"
        />
      )}
    </button>
  );
}

export function AccountModelAvailabilityToolbar({
  accounts,
  onUpdated,
}: {
  accounts: AccountRow[];
  onUpdated: () => void;
}) {
  const { model, models, loading, error, setModel, refresh } =
    useContext(WatchedModel);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState("");
  const { showToast } = useToast();
  const eligible = accounts.filter(
    (a) => isCodexModelAccount(a) && a.enabled !== false,
  );
  const run = async (kind: "probe" | "manifest") => {
    if (busy) return;
    setBusy(true);
    setProgress("");
    let cursor = 0,
      done = 0,
      failed = 0;
    const worker = async () => {
      while (cursor < eligible.length) {
        const a = eligible[cursor++];
        try {
          if (kind === "probe") await api.probeAccountModels(a.id, model);
          else await api.syncAccountModelsUpstream(a.id);
        } catch {
          failed++;
        } finally {
          done++;
          setProgress(`${done}/${eligible.length}`);
        }
      }
    };
    try {
      await Promise.all([worker(), worker()]);
      onUpdated();
      refresh();
      showToast(
        `已完成 ${done} 个账户，${failed} 个请求失败。结果已记录，白名单需在“支持模型”中保存。`,
        failed ? "error" : "success",
      );
    } finally {
      setBusy(false);
    }
  };
  const options = Array.from(new Set([model, ...models]))
    .filter(Boolean)
    .map((value) => ({ label: value, value }));
  return (
    <div className="rounded-lg border border-border bg-card p-3 space-y-2 mb-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold">
          <Sparkles className="size-3.5" aria-hidden />
          关注模型
        </span>
        <Select
          aria-label="关注模型"
          value={model}
          options={options}
          onValueChange={setModel}
          disabled={busy || loading}
          className="w-44"
        />
        <Button
          size="sm"
          variant="outline"
          disabled={busy || !eligible.length}
          onClick={() => void run("manifest")}
        >
          同步本页清单
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={
            busy ||
            loading ||
            !model ||
            !models.includes(model) ||
            !eligible.length
          }
          onClick={() => void run("probe")}
        >
          {busy ? (
            <Loader2 className="size-3.5 animate-spin" />
          ) : (
            <CheckCircle2 className="size-3.5" />
          )}
          {busy ? `处理中 ${progress}` : "实测本页关注模型"}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          onClick={refresh}
          disabled={busy || loading}
        >
          刷新模型目录
        </Button>
      </div>
      <p className="text-[11px] text-muted-foreground">
        徽标展示各账户的检测结果；清单可见不代表调用成功。实测会消耗少量额度，不会自动修改白名单。记录超过 24 小时后提示复测。
      </p>
      {error && (
        <p role="alert" className="text-xs text-destructive">
          模型目录读取失败：{error}
        </p>
      )}
    </div>
  );
}

export function AccountModelAvailabilityPanel({
  account,
  draft,
  onAdd,
  onRefreshed,
}: {
  account: AccountRow;
  draft: string[];
  onAdd: (model: string) => void;
  onRefreshed: (account: AccountRow) => void;
}) {
  const { model, refresh } = useContext(WatchedModel);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  if (!isCodexModelAccount(account) || !model) return null;
  const result = accountModelAvailability(account, model);
  const allowed =
    !draft.length || draft.some((m) => m.toLowerCase() === model.toLowerCase());
  const probe = async () => {
    setBusy(true);
    setError("");
    try {
      await api.probeAccountModels(account.id, model);
      onRefreshed(await api.getAccount(account.id));
      refresh();
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="rounded-lg border border-border bg-muted/10 p-3 space-y-2">
      <div className="text-sm font-semibold">
        {model} · {labels[result.state]}
      </div>
      <p className="text-xs text-muted-foreground">
        {allowed
          ? "当前白名单已放行；这不代表上游确认支持。"
          : "当前白名单尚未放行此模型。"}
      </p>
      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void probe()}
        >
          {busy ? (
            <Loader2 className="size-3.5 animate-spin" />
          ) : (
            <CheckCircle2 className="size-3.5" />
          )}
          检测此模型
        </Button>
        {!allowed && (
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => onAdd(model)}
          >
            加入白名单（保存后生效）
          </Button>
        )}
      </div>
      {error && (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
