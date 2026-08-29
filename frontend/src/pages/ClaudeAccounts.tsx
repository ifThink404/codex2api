import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  X,
  Activity,
  Sparkles,
  Coins,
  BarChart3,
  Pencil,
  ExternalLink,
  RefreshCw,
  Lock,
  MoreHorizontal,
  Trash2,
  Columns3,
  Plus,
} from "lucide-react";

import { api } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountRow,
  AccountGroup,
  AccountListSummary,
  AccountEmailDomainFacet,
  AccountPageStatsItem,
  AccountHealthBucket,
  ClaudeImportTokenRequest,
} from "../types";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountHealthBar from "../components/AccountHealthBar";
import RequestCountPills from "../components/RequestCountPills";
import { CompactStat } from "../components/CompactStat";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountQuotaDistributionChart from "../components/AccountQuotaDistributionChart";
import AccountRateLimitRecoveryChart from "../components/AccountRateLimitRecoveryChart";
import type { AccountAnalysisResponse } from "../types";
import { ProxyField } from "../components/ProxyField";
import { AccountGroupManagerModal, ACCOUNT_GROUP_COLORS } from "../components/AccountGroupManagerModal";
import { Select } from "../components/ui/select";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StatusBadge from "../components/StatusBadge";
import Pagination from "../components/Pagination";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
} from "../components/AccountGroupFilterSelect";
import type { AccountGroupFilterValue } from "../components/AccountGroupFilterSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";

const FALLBACK_GROUP_COLOR = "#2563eb";
function normalizeGroupColor(color?: string): string {
  const v = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(v) ? v : FALLBACK_GROUP_COLOR;
}

// extractCode 从粘贴内容里取授权码：支持整条回调 URL、code#state、或纯 code。
function extractCode(input: string): string {
  const raw = input.trim();
  if (!raw) return "";
  if (raw.startsWith("http://") || raw.startsWith("https://")) {
    try {
      const u = new URL(raw);
      const code = u.searchParams.get("code");
      if (code) return code.trim();
    } catch {
      // fall through
    }
  }
  return raw;
}

// claudeUsagePct 取用量百分比(0-100)。后端解析 Anthropic 统一限流头后,
// usage_percent_5h/7d 为真实窗口利用率;null/undefined 表示尚无上游观测。
function claudeUsagePct(v: unknown): number | null {
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) && n >= 0 ? Math.min(100, n) : null;
}

function usageTone(pct: number): string {
  return pct >= 90 ? "bg-rose-500" : pct >= 70 ? "bg-amber-500" : "bg-emerald-500";
}

// formatCompactNum 紧凑数字:1234 → 1.2k。
function formatCompactNum(v: unknown): string {
  const n = typeof v === "number" ? v : Number(v);
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(Math.round(n));
}

// pad2 两位补零。
const pad2 = (n: number) => String(n).padStart(2, "0");

// formatShortDateTime "MM-DD HH:mm" 短格式(与 Codex 卡片的 ⏱ 重置时间一致口径)。
function formatShortDateTime(iso?: string): { label: string; title: string } | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return null;
  return {
    label: `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`,
    title: d.toLocaleString(),
  };
}

// formatRelativeShort 相对时间:刚刚 / Xm / Xh / Xd 前。
function formatRelativeShort(iso: string | undefined, t: (k: string) => string): string {
  if (!iso) return "-";
  const ts = new Date(iso).getTime();
  if (!Number.isFinite(ts)) return "-";
  const diff = Math.max(0, Date.now() - ts);
  const m = Math.floor(diff / 60000);
  if (m < 1) return t("claude.justNow");
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h${m % 60}m`;
  return `${Math.floor(h / 24)}d${h % 24}h`;
}

// maybeOfferSaveProxyToPool 手动输入(非代理池)的代理保存后,若该代理不在代理管理中,
// 询问是否存入代理池,方便后续复用与负载均衡。confirm 返回 true 才写入。
async function maybeOfferSaveProxyToPool(
  url: string,
  proxies: ProxyRow[],
  confirm: (opts: { title: string; description: string }) => Promise<boolean>,
  showToast: (msg: string, type?: "success" | "error") => void,
  t: (k: string, o?: Record<string, unknown>) => string,
): Promise<void> {
  const trimmed = url.trim();
  if (!trimmed) return;
  if (proxies.some((p) => p.url === trimmed)) return; // 已在池中
  const ok = await confirm({
    title: t("claude.saveProxyToPoolTitle"),
    description: trimmed,
  });
  if (!ok) return;
  try {
    await api.addProxies({ url: trimmed });
    showToast(t("claude.saveProxyToPoolDone"), "success");
  } catch (error) {
    showToast(getErrorMessage(error), "error");
  }
}

// avatarInitial 头像首字母。
function avatarInitial(acc: AccountRow): string {
  const s = (acc.email || acc.name || "").trim();
  return s ? s[0].toUpperCase() : "C";
}

// claudePlanBadge 按订阅档位配色(pro/max-5x/max-20x/team/enterprise/free)。
function claudePlanBadge(plan: string): { label: string; cls: string } {
  const p = plan.trim().toLowerCase();
  const base = "inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset";
  switch (p) {
    case "pro":
      return { label: "Pro", cls: `${base} bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20` };
    case "max-5x":
      return { label: "Max 5x", cls: `${base} bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20` };
    case "max-20x":
      return { label: "Max 20x", cls: `${base} bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20` };
    case "max":
      return { label: "Max", cls: `${base} bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20` };
    case "team":
      return { label: "Team", cls: `${base} bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20` };
    case "enterprise":
      return { label: "Enterprise", cls: `${base} bg-indigo-50 text-indigo-700 ring-indigo-600/20 dark:bg-indigo-950 dark:text-indigo-300 dark:ring-indigo-400/20` };
    case "free":
      return { label: "Free", cls: `${base} bg-zinc-100 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-400 dark:ring-zinc-500/20` };
    default:
      return { label: plan, cls: `${base} bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20` };
  }
}

// 状态过滤项 → 后端 status 参数。
type ClaudeStatusFilter =
  | "all"
  | "normal"
  | "scheduling"
  | "rate_limited"
  | "abnormal"
  | "banned"
  | "error"
  | "unsampled"
  | "disabled"
  | "locked";

type AuthFilter = "all" | "oauth" | "api_key";
type HealthTier = "healthy" | "warm" | "risky" | "banned";

type SortKey = "default" | "group" | "priority" | "usage" | "requests" | "today";
const SORT_MAP: Record<SortKey, { sort: NonNullable<Parameters<typeof api.getAccountsPage>[0]["sort"]>; order: "asc" | "desc" }> = {
  default: { sort: "updated_at", order: "desc" },
  group: { sort: "group", order: "asc" },
  priority: { sort: "scheduler_priority", order: "desc" },
  usage: { sort: "usage", order: "desc" },
  requests: { sort: "requests", order: "desc" },
  today: { sort: "today", order: "desc" },
};

// 可显隐列(序号/邮箱/操作为固定核心列,不参与切换)。持久化到 localStorage,与 Codex 一致。
const CLAUDE_TOGGLE_COLUMNS = [
  "groups",
  "priority",
  "plan",
  "status",
  "today",
  "requests",
  "usage",
  "cost",
  "importTime",
  "updatedAt",
] as const;
type ClaudeCol = (typeof CLAUDE_TOGGLE_COLUMNS)[number];
type ClaudeColVisibility = Record<ClaudeCol, boolean>;
const CLAUDE_COLS_KEY = "codex2api:claude-accounts:visible-columns";

function defaultClaudeCols(): ClaudeColVisibility {
  return Object.fromEntries(CLAUDE_TOGGLE_COLUMNS.map((c) => [c, true])) as ClaudeColVisibility;
}

function loadClaudeCols(): ClaudeColVisibility {
  const fallback = defaultClaudeCols();
  try {
    const raw = window.localStorage.getItem(CLAUDE_COLS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<ClaudeColVisibility>;
    return Object.fromEntries(
      CLAUDE_TOGGLE_COLUMNS.map((c) => [c, typeof parsed[c] === "boolean" ? parsed[c] : true]),
    ) as ClaudeColVisibility;
  } catch {
    return fallback;
  }
}

// LiveCountdown 显示限流/重置的剩余时间,每秒刷新。
// plain=true 为弱化文本样式(用量条下的 ⏱ 重置行);默认琥珀徽章(限流冷却)。
function LiveCountdown({ until, label, plain = false }: { until?: string; label: string; plain?: boolean }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!until) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [until]);
  if (!until) return null;
  const target = new Date(until).getTime();
  if (!Number.isFinite(target)) return null;
  const remain = Math.max(0, Math.floor((target - now) / 1000));
  if (remain <= 0) return null;
  const d = Math.floor(remain / 86400);
  const h = Math.floor((remain % 86400) / 3600);
  const m = Math.floor((remain % 3600) / 60);
  const s = remain % 60;
  const text = d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m${s}s` : `${s}s`;
  if (plain) {
    return (
      <span className="text-[11px] font-medium text-muted-foreground tabular-nums">
        {label} {text}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1 rounded-md bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 tabular-nums dark:text-amber-400">
      {label} {text}
    </span>
  );
}

// UsageWindow 单条用量窗口(5h / 7d)。视觉对齐 Codex 的 UsageBar/UsageWindowStat:
// - percent 有真实观测(Anthropic 统一限流头)→ 进度条 + 百分比 + ⏱重置倒计时;
// - 仅有网关侧明细(req/tok/$)→ 明细行;
// - 两者都无 → 不渲染(由父级统一显示 "-")。
function UsageWindow({
  label,
  pct,
  reset,
  resetLabel,
  detail,
}: {
  label: string;
  pct: number | null;
  reset?: string;
  resetLabel: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const hasDetail = !!detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0);
  const billed = typeof detail?.account_billed === "number" && detail.account_billed > 0 ? detail.account_billed : null;
  if (pct === null && !hasDetail) return null;
  const rt = formatShortDateTime(reset);
  // 明细(req/tok/$)进 tooltip,行内只留 标签+进度条+百分比+⏱重置,收窄整列。
  const detailTitle = [
    hasDetail ? `${formatCompactNum(detail?.requests)} req / ${formatCompactNum(detail?.tokens)} tok` : "",
    billed !== null ? `$${billed.toFixed(4)}` : "",
    rt ? `${resetLabel} ${rt.title}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="flex items-center gap-1.5 whitespace-nowrap" title={detailTitle || undefined}>
      <span className="w-[30px] shrink-0 text-[11px] font-medium text-muted-foreground">{label}</span>
      <span className="h-1.5 w-14 shrink-0 overflow-hidden rounded-full bg-muted">
        {pct !== null ? (
          <span className={cn("block h-full rounded-full transition-all", usageTone(pct))} style={{ width: `${Math.min(100, pct)}%` }} />
        ) : null}
      </span>
      <span className="w-[40px] shrink-0 text-right text-[12px] font-semibold tabular-nums">
        {pct !== null ? `${pct.toFixed(1)}%` : "—"}
      </span>
      {rt ? (
        <span className="shrink-0 text-[10px] tabular-nums text-muted-foreground/70">⏱{rt.label}</span>
      ) : null}
    </div>
  );
}

export default function ClaudeAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [summary, setSummary] = useState<AccountListSummary | null>(null);
  const [tags, setTags] = useState<string[]>([]);
  const [domains, setDomains] = useState<AccountEmailDomainFacet[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [groups, setGroups] = useState<AccountGroup[]>([]);

  const [showAdd, setShowAdd] = useState(false);
  const [showManageGroups, setShowManageGroups] = useState(false);
  const [assignTarget, setAssignTarget] = useState<AccountRow | null>(null);
  const [usageTarget, setUsageTarget] = useState<AccountRow | null>(null);
  const [editTarget, setEditTarget] = useState<AccountRow | null>(null);
  // page-stats 独立拉取:分页基础行不含 5h/7d/今日 的网关侧用量明细,单独补齐(与 Codex 页同构)。
  const [pageStats, setPageStats] = useState<Record<string, AccountPageStatsItem>>({});
  const [pageStatsToken, setPageStatsToken] = useState(0);
  // 健康状态条(近 200 分钟成败分桶,与 Codex 卡片同源接口)。
  const [healthBars, setHealthBars] = useState<Record<string, AccountHealthBucket[]>>({});
  // 额度分布 + 限流恢复分析(号池模式面板,与 Codex 同源接口/组件)。
  const [analysis, setAnalysis] = useState<AccountAnalysisResponse | null>(null);
  const [showAnalysis, setShowAnalysis] = useState(true);

  const loadAnalysis = useCallback(async () => {
    try {
      const res = await api.getAccountAnalysis("claude");
      setAnalysis(res);
    } catch {
      /* 分析面板失败不阻断列表 */
    }
  }, []);

  useEffect(() => {
    void loadAnalysis();
  }, [loadAnalysis]);

  // 过滤 / 排序 / 分页
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<ClaudeStatusFilter>("all");
  const [healthTier, setHealthTier] = useState<HealthTier | null>(null);
  const [planFilter, setPlanFilter] = useState<string>("all");
  const [authFilter, setAuthFilter] = useState<AuthFilter>("all");
  const [tagFilter, setTagFilter] = useState<string>("all");
  const [domainFilter, setDomainFilter] = useState<string>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(EMPTY_ACCOUNT_GROUP_FILTER);
  const [sortKey, setSortKey] = useState<SortKey>("default");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [hideDomainTags, setHideDomainTags] = useState(false);
  const [visibleCols, setVisibleCols] = useState<ClaudeColVisibility>(loadClaudeCols);
  useEffect(() => {
    try {
      window.localStorage.setItem(CLAUDE_COLS_KEY, JSON.stringify(visibleCols));
    } catch {
      /* localStorage 不可用时忽略 */
    }
  }, [visibleCols]);
  const [knownPlans, setKnownPlans] = useState<string[]>([]);
  const [selected, setSelected] = useState<Set<number>>(new Set());

  // 搜索防抖
  useEffect(() => {
    const id = window.setTimeout(() => setDebouncedSearch(search.trim()), 300);
    return () => window.clearTimeout(id);
  }, [search]);

  // 筛选变化时回到第一页
  useEffect(() => {
    setPage(1);
  }, [debouncedSearch, statusFilter, healthTier, planFilter, authFilter, tagFilter, domainFilter, groupFilter, sortKey, pageSize]);

  const claudeGroups = useMemo(() => groups.filter((g) => g.channel === "claude"), [groups]);
  const groupMap = useMemo(() => new Map(claudeGroups.map((g) => [g.id, g])), [claudeGroups]);

  const reloadGroups = useCallback(async () => {
    try {
      const res = await api.listAccountGroups();
      setGroups(res.groups ?? []);
    } catch {
      /* ignore */
    }
  }, []);

  const reload = useCallback(async () => {
    setLoading(true);
    const controller = new AbortController();
    try {
      const { sort, order } = SORT_MAP[sortKey];
      const res = await api.getAccountsPage(
        {
          channel: "claude",
          page,
          pageSize,
          search: debouncedSearch || undefined,
          status: statusFilter === "all" ? undefined : statusFilter,
          healthTier: healthTier ?? undefined,
          plan: planFilter === "all" ? undefined : planFilter,
          authKind: authFilter === "all" ? undefined : authFilter,
          tag: tagFilter === "all" ? undefined : tagFilter,
          emailDomain: domainFilter === "all" ? undefined : domainFilter,
          groupInclude: groupFilter.include,
          groupExclude: groupFilter.exclude,
          ungrouped: groupFilter.ungrouped,
          sort,
          order,
        },
        controller.signal,
      );
      if (controller.signal.aborted) return;
      const rows = res.accounts ?? [];
      setAccounts(rows);
      setSummary(res.summary ?? null);
      setTags(res.facets?.tags ?? []);
      setDomains(res.facets?.email_domains ?? []);
      setTotal(res.total ?? rows.length);
      if (res.page && res.page !== page) setPage(res.page);
      // 累积已知套餐,供套餐 Tab 使用。
      setKnownPlans((prev) => {
        const set = new Set(prev);
        for (const r of rows) if (r.plan_type) set.add(r.plan_type);
        return set.size === prev.length ? prev : Array.from(set);
      });
    } catch (error) {
      if (!controller.signal.aborted) showToast(getErrorMessage(error), "error");
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [
    page,
    pageSize,
    debouncedSearch,
    statusFilter,
    healthTier,
    planFilter,
    authFilter,
    tagFilter,
    domainFilter,
    groupFilter,
    sortKey,
    showToast,
  ]);

  useEffect(() => {
    void reload();
  }, [reload]);

  // 拉取当前页账号的网关侧用量明细(req/tok/$,5h/7d/今日窗口)。
  const accountIDsKey = useMemo(() => accounts.map((a) => a.id).join(","), [accounts]);
  useEffect(() => {
    if (!accountIDsKey) {
      setPageStats({});
      return;
    }
    const controller = new AbortController();
    void api
      .getAccountPageStats(accountIDsKey.split(",").map(Number), controller.signal)
      .then((res) => {
        if (!controller.signal.aborted) setPageStats(res.stats ?? {});
      })
      .catch(() => {
        /* stats 失败不阻断列表 */
      });
    return () => controller.abort();
  }, [accountIDsKey, pageStatsToken]);

  // 刷新单个账号用量:触发上游探针(有则)+ 重拉本页 page-stats 明细。
  const handleRefreshUsage = useCallback(
    async (acc: AccountRow) => {
      try {
        await api.refreshAccountUsage(acc.id);
      } catch {
        /* 探针失败照样重拉现有快照 */
      }
      setPageStatsToken((v) => v + 1);
    },
    [],
  );

  // 健康状态条数据。
  useEffect(() => {
    if (!accountIDsKey) {
      setHealthBars({});
      return;
    }
    let cancelled = false;
    void api
      .getAccountHealthBars(accountIDsKey.split(",").map(Number))
      .then((res) => {
        if (!cancelled) setHealthBars(res.buckets ?? {});
      })
      .catch(() => {
        /* 健康条失败不阻断列表 */
      });
    return () => {
      cancelled = true;
    };
  }, [accountIDsKey]);

  // 渲染行 = 基础行 + page-stats 补齐(只补缺失字段,基础行已有的以基础行为准)。
  const displayRows = useMemo(() => {
    return accounts.map((acc) => {
      const stats = pageStats[String(acc.id)];
      if (!stats) return acc;
      const merged = { ...acc };
      if (!merged.usage_5h_detail && stats.usage_5h_detail) merged.usage_5h_detail = stats.usage_5h_detail;
      if (!merged.usage_7d_detail && stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail;
      if (!merged.usage_today_detail && stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail;
      if (merged.official_usd == null && stats.official_usd != null) merged.official_usd = stats.official_usd;
      if (merged.official_usd_7d == null && stats.official_usd_7d != null) merged.official_usd_7d = stats.official_usd_7d;
      return merged;
    });
  }, [accounts, pageStats]);

  useEffect(() => {
    void reloadGroups();
    let cancelled = false;
    void api
      .listProxies()
      .then((res) => {
        if (!cancelled) setProxyPool(res.proxies ?? []);
      })
      .catch(() => {
        if (!cancelled) setProxyPool([]);
      });
    return () => {
      cancelled = true;
    };
  }, [reloadGroups]);

  // ── 账号操作 ──────────────────────────────────────────────
  const handleDelete = useCallback(
    async (acc: AccountRow) => {
      const ok = await confirm({
        title: t("claude.deleteConfirm"),
        description: acc.email || acc.name || `#${acc.id}`,
      });
      if (!ok) return;
      try {
        await api.deleteAccount(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [confirm, reload, showToast, t],
  );

  const handleRefresh = useCallback(
    async (acc: AccountRow) => {
      try {
        await api.refreshAccount(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [reload, showToast],
  );

  const handleRefreshModels = useCallback(
    async (acc: AccountRow) => {
      try {
        const res = await api.refreshClaudeModels(acc.id);
        showToast(t("claude.modelsRefreshed", { count: res.count }));
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [reload, showToast, t],
  );

  const handleRefreshAllModels = useCallback(async () => {
    try {
      await api.refreshAllClaudeModels();
      showToast(t("claude.allModelsRefreshed"), "success");
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [reload, showToast, t]);

  const handleToggleEnabled = useCallback(
    async (acc: AccountRow) => {
      const next = acc.enabled === false;
      try {
        await api.toggleAccountEnabled(acc.id, next);
        showToast(next ? t("claude.enabledToast") : t("claude.disabledToast"), "success");
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [reload, showToast, t],
  );

  const handleToggleLock = useCallback(
    async (acc: AccountRow) => {
      const next = !acc.locked;
      try {
        await api.toggleAccountLock(acc.id, next);
        showToast(next ? t("claude.lockedToast") : t("claude.unlockedToast"), "success");
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [reload, showToast, t],
  );

  const handleResetStatus = useCallback(
    async (acc: AccountRow) => {
      try {
        await api.resetAccountStatus(acc.id);
        showToast(t("claude.statusReset"), "success");
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [reload, showToast, t],
  );

  // ── 批量操作 ──────────────────────────────────────────────
  const selectedIds = useMemo(() => Array.from(selected), [selected]);
  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);
  const allSelected = accounts.length > 0 && accounts.every((a) => selected.has(a.id));
  const toggleSelectAll = useCallback(() => {
    setSelected((prev) => {
      if (accounts.every((a) => prev.has(a.id))) return new Set();
      return new Set(accounts.map((a) => a.id));
    });
  }, [accounts]);

  const runBatch = useCallback(
    async (patch: { enabled?: boolean; locked?: boolean }) => {
      if (selectedIds.length === 0) return;
      try {
        await api.batchUpdateAccounts({ ids: selectedIds, ...patch });
        setSelected(new Set());
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [selectedIds, reload, showToast],
  );

  // ── 派生 UI 数据 ──────────────────────────────────────────
  const statChips = useMemo(() => {
    const s = summary;
    const c: Array<{ id: ClaudeStatusFilter; label: string; count: number; tone?: string }> = [
      { id: "all", label: t("claude.statAll"), count: s?.total ?? total },
      { id: "normal", label: t("claude.statNormal"), count: s?.normal ?? 0, tone: "text-emerald-600 dark:text-emerald-400" },
      { id: "scheduling", label: t("claude.statScheduling"), count: s?.active ?? 0, tone: "text-sky-600 dark:text-sky-400" },
      { id: "rate_limited", label: t("claude.statRateLimited"), count: s?.rate_limited ?? 0, tone: "text-amber-600 dark:text-amber-400" },
      { id: "abnormal", label: t("claude.statAbnormal"), count: s?.abnormal ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "banned", label: t("claude.statBanned"), count: s?.banned ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "error", label: t("claude.statError"), count: s?.error ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "unsampled", label: t("claude.statUnsampled"), count: s?.unsampled ?? 0 },
      { id: "disabled", label: t("claude.statDisabled"), count: s?.disabled ?? 0 },
      { id: "locked", label: t("claude.statLocked"), count: s?.locked ?? 0 },
    ];
    return c;
  }, [summary, total, t]);

  const healthChips = useMemo(() => {
    const s = summary;
    return [
      { id: "healthy" as HealthTier, label: t("claude.healthHealthy"), count: s?.healthy ?? 0, dot: "bg-emerald-500" },
      { id: "warm" as HealthTier, label: t("claude.healthWarm"), count: s?.warm ?? 0, dot: "bg-amber-500" },
      { id: "risky" as HealthTier, label: t("claude.healthRisky"), count: s?.risky ?? 0, dot: "bg-rose-500" },
      { id: "banned" as HealthTier, label: t("claude.healthBanned"), count: s?.banned ?? 0, dot: "bg-zinc-500" },
    ];
  }, [summary, t]);

  const planTabs = useMemo(() => {
    const plans = knownPlans.filter(Boolean).sort();
    return ["all", ...plans];
  }, [knownPlans]);

  // Claude 账号本就全部走 OAuth;后端 oauth 计数为 grok 专用逻辑,这里按语义直接取 total。
  const authTabs: Array<{ id: AuthFilter; label: string; count?: number }> = [
    { id: "all", label: t("claude.authAll") },
    { id: "oauth", label: t("claude.authOAuth"), count: summary?.oauth || summary?.total || 0 },
    { id: "api_key", label: t("claude.authApiKey"), count: summary?.api_key ?? 0 },
  ];

  const filtersActive =
    statusFilter !== "all" ||
    healthTier !== null ||
    planFilter !== "all" ||
    authFilter !== "all" ||
    tagFilter !== "all" ||
    domainFilter !== "all" ||
    !isAccountGroupFilterEmpty(groupFilter) ||
    sortKey !== "default" ||
    debouncedSearch.length > 0;

  const clearFilters = useCallback(() => {
    setStatusFilter("all");
    setHealthTier(null);
    setPlanFilter("all");
    setAuthFilter("all");
    setTagFilter("all");
    setDomainFilter("all");
    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
    setSortKey("default");
    setSearch("");
  }, []);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  const selectFieldCls =
    "h-8 rounded-md border border-input bg-background px-2 text-xs text-foreground outline-none focus-visible:border-ring";

  return (
    <div>
      <PageHeader
        title={t("claude.title")}
        description={t("claude.subtitle")}
        titleAdornment={headerSlot}
        onRefresh={() => void reload()}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" onClick={() => setShowAnalysis((v) => !v)}>
              <BarChart3 className="size-3.5" />
              {showAnalysis ? t("usage.hideAnalysis") : t("usage.showAnalysis")}
            </Button>
            <Button variant="outline" size="sm" onClick={() => void handleRefreshAllModels()}>
              {t("claude.refreshAllModels")}
            </Button>
            <Button variant="outline" size="sm" onClick={() => setShowManageGroups(true)}>
              {t("claude.manageGroups")}
            </Button>
            <Button onClick={() => setShowAdd(true)}>{t("claude.addAccount")}</Button>
          </div>
        }
      />

      {/* 统计卡(复用共享 CompactStat,与 Codex 同款:状态药丸 + 5h/7d·封禁/错误 details) */}
      <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-5">
        <CompactStat
          label={t("accounts.totalAccounts")}
          chipLabel={t("claude.statAll")}
          value={summary?.total ?? total}
          tone="neutral"
          active={statusFilter === "all"}
          onClick={() => setStatusFilter("all")}
        />
        <CompactStat
          label={t("accounts.normalAccounts")}
          chipLabel={t("claude.statNormal")}
          value={summary?.normal ?? 0}
          tone="success"
          active={statusFilter === "normal"}
          onClick={() => setStatusFilter(statusFilter === "normal" ? "all" : "normal")}
        />
        <CompactStat
          label={t("accounts.schedulingAccounts")}
          chipLabel={t("claude.statScheduling")}
          value={summary?.active ?? 0}
          tone="warning"
          active={statusFilter === "scheduling"}
          onClick={() => setStatusFilter(statusFilter === "scheduling" ? "all" : "scheduling")}
        />
        <CompactStat
          label={t("accounts.rateLimited")}
          chipLabel={t("claude.statRateLimited")}
          value={summary?.rate_limited ?? 0}
          tone="warning"
          active={statusFilter === "rate_limited"}
          details={[
            { label: "5h", value: summary?.rate_limited_5h ?? 0 },
            { label: "7d", value: summary?.rate_limited_7d ?? 0 },
          ]}
          onClick={() => setStatusFilter(statusFilter === "rate_limited" ? "all" : "rate_limited")}
        />
        <CompactStat
          label={t("accounts.abnormalAccounts")}
          chipLabel={t("claude.statAbnormal")}
          value={summary?.abnormal ?? 0}
          tone="danger"
          active={statusFilter === "abnormal"}
          details={[
            { label: t("accounts.abnormalBannedShort"), value: summary?.banned ?? 0 },
            { label: t("accounts.abnormalErrorShort"), value: summary?.error ?? 0 },
          ]}
          onClick={() => setStatusFilter(statusFilter === "abnormal" ? "all" : "abnormal")}
        />
      </div>

      {/* 额度分布 + 限流恢复(号池模式分析面板,与 Codex 同款组件) */}
      {showAnalysis && analysis ? (
        <div className="mb-4 grid items-stretch gap-4 xl:grid-cols-2">
          <AccountQuotaDistributionChart
            analysis={analysis.quota}
            compact
            className="min-w-0"
            onRefreshAnalysis={() => void loadAnalysis()}
            onProbeError={(message) => showToast(message, "error")}
            descKey="claude.quotaDesc"
            emptyKey="claude.quotaEmpty"
            showProbe={false}
          />
          <AccountRateLimitRecoveryChart analysis={analysis} compact className="min-w-0" />
        </div>
      ) : null}

      {/* 统计芯片 */}
      <div className="mb-3 flex flex-wrap items-center gap-1.5">
        {statChips.map((chip) => {
          const active = statusFilter === chip.id;
          return (
            <button
              key={chip.id}
              type="button"
              onClick={() => setStatusFilter(chip.id)}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1 text-xs font-semibold transition-colors",
                active
                  ? "border-primary/40 bg-primary/10 text-primary"
                  : "border-border bg-muted/40 text-muted-foreground hover:text-foreground",
              )}
            >
              <span className={cn(!active && chip.tone)}>{chip.label}</span>
              <span className="rounded-md bg-background/60 px-1 text-[10px] font-bold tabular-nums">{chip.count}</span>
            </button>
          );
        })}
      </div>

      {/* 调度视图(点击按健康档过滤) */}
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <span className="text-[11px] font-medium text-muted-foreground">{t("claude.schedulingView")}</span>
        {healthChips.map((h) => {
          const active = healthTier === h.id;
          return (
            <button
              key={h.id}
              type="button"
              onClick={() => setHealthTier(active ? null : h.id)}
              className={cn(
                "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs transition-colors",
                active ? "bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground",
              )}
            >
              <span className={cn("size-1.5 rounded-full", h.dot)} />
              {h.label}
              <span className="font-semibold tabular-nums text-foreground">{h.count}</span>
            </button>
          );
        })}
      </div>

      {/* 套餐 Tab */}
      {planTabs.length > 1 ? (
        <div className="mb-2 flex flex-wrap items-center gap-1">
          {planTabs.map((p) => {
            const active = planFilter === p;
            return (
              <button
                key={p}
                type="button"
                onClick={() => setPlanFilter(p)}
                className={cn(
                  "rounded-md px-2 py-1 text-xs font-medium transition-colors",
                  active ? "bg-primary text-primary-foreground" : "bg-muted/40 text-muted-foreground hover:text-foreground",
                )}
              >
                {p === "all" ? t("claude.planAll") : p}
              </button>
            );
          })}
        </div>
      ) : null}

      {/* 过滤条:OAuth/API + 分组 + 标签 + 域名 + 排序 + 搜索 */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="inline-flex overflow-hidden rounded-md border border-border">
          {authTabs.map((a) => (
            <button
              key={a.id}
              type="button"
              onClick={() => setAuthFilter(a.id)}
              className={cn(
                "px-2.5 py-1 text-xs font-medium transition-colors",
                authFilter === a.id ? "bg-primary text-primary-foreground" : "bg-background text-muted-foreground hover:text-foreground",
              )}
            >
              {a.label}
              {typeof a.count === "number" ? <span className="ml-1 tabular-nums opacity-70">{a.count}</span> : null}
            </button>
          ))}
        </div>

        <AccountGroupFilterSelect
          groups={claudeGroups}
          value={groupFilter}
          onChange={setGroupFilter}
          className="w-40"
        />

        <Select
          compact
          className="w-32"
          value={tagFilter}
          onValueChange={setTagFilter}
          options={[{ value: "all", label: t("claude.allTags") }, ...tags.map((tag) => ({ value: tag, label: tag }))]}
        />

        <Select
          compact
          className="w-36"
          value={domainFilter}
          onValueChange={setDomainFilter}
          options={[
            { value: "all", label: t("claude.allDomains") },
            ...domains.map((d) => ({ value: d.domain, label: `${d.domain} (${d.total})` })),
          ]}
        />

        <Select
          compact
          className="w-32"
          value={sortKey}
          onValueChange={(v) => setSortKey(v as SortKey)}
          options={[
            { value: "default", label: t("claude.sortDefault") },
            { value: "group", label: t("claude.sortGroup") },
            { value: "priority", label: t("claude.sortPriority") },
            { value: "usage", label: t("claude.sortUsage") },
            { value: "requests", label: t("claude.sortRequests") },
            { value: "today", label: t("claude.sortToday") },
          ]}
        />

        <button
          type="button"
          onClick={() => setHideDomainTags((v) => !v)}
          className={cn(
            "rounded-md border border-border px-2 py-1 text-xs transition-colors",
            hideDomainTags ? "bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {hideDomainTags ? t("claude.showDomainTags") : t("claude.hideDomainTags")}
        </button>

        <ColumnsMenu visible={visibleCols} onChange={setVisibleCols} />

        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder={t("claude.searchPlaceholder")}
          className="h-8 max-w-xs flex-1"
        />

        {filtersActive ? (
          <Button variant="ghost" size="sm" onClick={clearFilters}>
            <X className="size-3.5" />
            {t("claude.clearFilters")}
          </Button>
        ) : null}
      </div>

      {/* 批量操作条 */}
      {selectedIds.length > 0 ? (
        <div className="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-xs">
          <span className="font-semibold text-primary">{t("claude.selectedCount", { count: selectedIds.length })}</span>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ enabled: true })}>
            {t("claude.batchEnable")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ enabled: false })}>
            {t("claude.batchDisable")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ locked: true })}>
            {t("claude.batchLock")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ locked: false })}>
            {t("claude.batchUnlock")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
            {t("claude.clearSelection")}
          </Button>
        </div>
      ) : null}

      {/* 账号列表 */}
      {loading ? (
        <div className="py-16 text-center text-sm text-muted-foreground">{t("common.loading")}</div>
      ) : total === 0 && !filtersActive ? (
        <div className="rounded-xl border border-dashed border-border py-16 text-center text-sm text-muted-foreground">
          {t("claude.empty")}
        </div>
      ) : accounts.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
          {t("claude.emptyFiltered")}
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-border bg-card shadow-sm">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left text-[11px] font-semibold uppercase tracking-wide text-muted-foreground [&>th]:whitespace-nowrap">
                <th className="w-10 px-3 py-2.5">
                  <input
                    type="checkbox"
                    className="size-3.5 cursor-pointer accent-primary"
                    checked={allSelected}
                    onChange={toggleSelectAll}
                    aria-label={t("accounts.selectAll")}
                  />
                </th>
                <th className="px-2 py-2.5 text-center">{t("accounts.sequence")}</th>
                <th className="px-2 py-2.5">{t("accounts.email")}</th>
                {visibleCols.groups ? <th className="px-2 py-2.5 text-center">{t("accounts.groupsLabel")}</th> : null}
                {visibleCols.priority ? <th className="px-2 py-2.5 text-center">{t("accounts.schedulerPriorityColumn")}</th> : null}
                {visibleCols.plan ? <th className="px-2 py-2.5 text-center">{t("accounts.plan")}</th> : null}
                {visibleCols.status ? <th className="px-2 py-2.5 text-center">{t("accounts.status")}</th> : null}
                {visibleCols.today ? <th className="px-2 py-2.5 text-center">{t("claude.todayLabel")}</th> : null}
                {visibleCols.requests ? <th className="px-2 py-2.5 text-center">{t("accounts.requests")}</th> : null}
                {visibleCols.usage ? <th className="px-2 py-2.5 text-center">{t("accounts.usage")}</th> : null}
                {visibleCols.cost ? <th className="px-2 py-2.5 text-center">{t("claude.costLabel")}</th> : null}
                {visibleCols.importTime ? <th className="px-2 py-2.5 text-center">{t("accounts.importTime")}</th> : null}
                {visibleCols.updatedAt ? <th className="px-2 py-2.5 text-center">{t("accounts.updatedAt")}</th> : null}
                <th className="px-2 py-2.5 text-right">{t("accounts.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {displayRows.map((acc, idx) => (
                <ClaudeAccountRow
                  key={acc.id}
                  acc={acc}
                  no={(page - 1) * pageSize + idx + 1}
                  selected={selected.has(acc.id)}
                  onToggleSelect={() => toggleSelect(acc.id)}
                  groupMap={groupMap}
                  healthBuckets={healthBars[String(acc.id)]}
                  hideDomainTags={hideDomainTags}
                  columns={visibleCols}
                  onRefresh={() => void handleRefresh(acc)}
                  onRefreshModels={() => void handleRefreshModels(acc)}
                  onToggleEnabled={() => void handleToggleEnabled(acc)}
                  onToggleLock={() => void handleToggleLock(acc)}
                  onResetStatus={() => void handleResetStatus(acc)}
                  onAssignGroups={() => setAssignTarget(acc)}
                  onUsage={() => setUsageTarget(acc)}
                  onUsageRefreshed={() => handleRefreshUsage(acc)}
                  onEdit={() => setEditTarget(acc)}
                  onDelete={() => void handleDelete(acc)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {total > 0 ? (
        <div className="mt-4">
          <Pagination
            page={page}
            totalPages={totalPages}
            onPageChange={setPage}
            totalItems={total}
            pageSize={pageSize}
            onPageSizeChange={(next) => {
              setPageSize(next);
              setPage(1);
            }}
            pageSizeOptions={[10, 20, 50, 100]}
          />
        </div>
      ) : null}

      {showAdd ? (
        <ClaudeAddModal
          proxies={proxyPool}
          groups={claudeGroups}
          onClose={() => setShowAdd(false)}
          onAdded={() => {
            setShowAdd(false);
            void reload();
          }}
        />
      ) : null}

      {showManageGroups ? (
        <AccountGroupManagerModal
          channel="claude"
          groups={claudeGroups}
          title={t("claude.manageGroups")}
          onClose={() => setShowManageGroups(false)}
          onChanged={() => {
            void reloadGroups();
            void reload();
          }}
        />
      ) : null}

      {assignTarget ? (
        <AssignGroupsModal
          account={assignTarget}
          groups={claudeGroups}
          onGroupsChanged={reloadGroups}
          onClose={() => setAssignTarget(null)}
          onSaved={() => {
            setAssignTarget(null);
            // 先刷新分组列表(内联新建的组要进 groupMap,否则芯片渲染不出),再刷新账号行。
            void reloadGroups();
            void reload();
          }}
        />
      ) : null}

      {usageTarget ? (
        <AccountUsageModal
          account={usageTarget}
          onClose={() => setUsageTarget(null)}
          showCreditSettings={false}
          officialUsage={false}
        />
      ) : null}

      {editTarget ? (
        <EditAccountModal
          account={editTarget}
          proxies={proxyPool}
          onClose={() => setEditTarget(null)}
          onSaved={() => {
            setEditTarget(null);
            void reload();
          }}
        />
      ) : null}

      {confirmDialog}
    </div>
  );
}

// ── 号池模式表格行(视觉对齐 Codex Pool Mode 表格;数据取 Claude 真实链路) ──
function ClaudeAccountRow({
  acc,
  no,
  selected,
  onToggleSelect,
  groupMap,
  healthBuckets,
  hideDomainTags,
  columns,
  onRefresh,
  onRefreshModels,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onAssignGroups,
  onUsage,
  onUsageRefreshed,
  onEdit,
  onDelete,
}: {
  acc: AccountRow;
  no: number;
  selected: boolean;
  onToggleSelect: () => void;
  groupMap: Map<number, AccountGroup>;
  healthBuckets?: AccountHealthBucket[];
  hideDomainTags: boolean;
  columns: ClaudeColVisibility;
  onRefresh: () => void;
  onRefreshModels: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onAssignGroups: () => void;
  onUsage: () => void;
  onUsageRefreshed: () => void | Promise<void>;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const pct5h = claudeUsagePct(acc.usage_percent_5h);
  const pct7d = claudeUsagePct(acc.usage_percent_7d);
  const disabled = acc.enabled === false;
  const cooldownReason = (acc.status || "").toLowerCase().includes("rate") ? acc.error_message : "";
  const accGroups = (acc.group_ids || []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[];
  const today = acc.usage_today_detail;
  const billed5h = typeof acc.usage_5h_detail?.account_billed === "number" ? acc.usage_5h_detail.account_billed : 0;
  const billed7d = typeof acc.usage_7d_detail?.account_billed === "number" ? acc.usage_7d_detail.account_billed : 0;
  const todayBilled = typeof today?.account_billed === "number" ? today.account_billed : 0;
  const created = formatShortDateTime(acc.created_at);

  const iconBtn =
    "inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground";

  return (
    <tr
      className={cn(
        "border-b border-border/60 align-middle transition-colors last:border-b-0 hover:bg-muted/30",
        selected && "bg-primary/5",
        disabled && "opacity-60",
      )}
    >
      {/* 勾选 */}
      <td className="px-3 py-3">
        <input
          type="checkbox"
          className="size-3.5 cursor-pointer accent-primary"
          checked={selected}
          onChange={onToggleSelect}
          aria-label={acc.email || acc.name}
        />
      </td>
      {/* 序号 */}
      <td className="px-2 py-3 text-center font-mono text-xs text-muted-foreground">{no}</td>
      {/* 邮箱 */}
      <td className="w-full min-w-[220px] px-2 py-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-orange-50 ring-1 ring-inset ring-orange-200 dark:bg-orange-950/70 dark:ring-orange-800">
            <ChannelLogo channel="claude" size={20} />
          </span>
          <div className="min-w-0">
            <div className="break-all text-[13px] font-medium leading-snug text-foreground" title={acc.email || acc.name}>
              {acc.email || acc.name || `#${acc.id}`}
            </div>
            <div className="mt-0.5 flex flex-wrap items-center gap-1">
              {!hideDomainTags && acc.email_domain ? (
                <span className="rounded bg-muted/70 px-1 py-0.5 text-[10px] text-muted-foreground">@{acc.email_domain}</span>
              ) : null}
              {acc.locked ? (
                <span className="inline-flex items-center rounded bg-blue-50 px-1 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                  <Lock className="mr-0.5 size-2.5" />
                  {t("claude.statLocked")}
                </span>
              ) : null}
            </div>
          </div>
        </div>
      </td>
      {columns.groups ? (
      <td className="min-w-[110px] px-2 py-3">
        <div className="flex flex-wrap items-center justify-center gap-1">
          {accGroups.map((g) => {
            const color = normalizeGroupColor(g.color);
            return (
              <button
                key={g.id}
                type="button"
                onClick={onAssignGroups}
                className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold transition-opacity hover:opacity-85"
                style={{ backgroundColor: `${color}14`, color, boxShadow: `inset 0 0 0 1px ${color}33` }}
                title={g.description || g.name}
              >
                <span className="size-1.5 rounded-full bg-current" />
                {g.name}
              </button>
            );
          })}
          <button
            type="button"
            onClick={onAssignGroups}
            className="inline-flex items-center gap-1 rounded-md border border-dashed border-border px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground transition-colors hover:border-primary/50 hover:text-primary"
          >
            <Plus className="size-2.5" />
            {t("claude.assignGroups")}
          </button>
        </div>
      </td>
      ) : null}
      {columns.priority ? (
      <td className="px-2 py-3 text-center">
        <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground" title={t("claude.priorityLabel")}>
          P {acc.scheduler_priority ?? 0}
        </span>
      </td>
      ) : null}
      {columns.plan ? (
      <td className="whitespace-nowrap px-2 py-3 text-center">
        {acc.plan_type ? (
          (() => {
            const b = claudePlanBadge(acc.plan_type);
            return <span className={b.cls}>{b.label}</span>;
          })()
        ) : (
          <span className="text-xs text-muted-foreground/50">-</span>
        )}
      </td>
      ) : null}
      {columns.status ? (
      <td className="min-w-[170px] px-2 py-3">
        <div className="flex flex-col items-center space-y-1.5">
          <div className="flex flex-wrap items-center justify-center gap-1">
            <StatusBadge status={acc.status} errorMessage={acc.error_message} detail={cooldownReason} />
            <LiveCountdown until={acc.cooldown_until} label={t("claude.resetIn")} />
            {acc.claude_api ? (
              <span
                className={cn(
                  "inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
                  acc.claude_usage_probe_error
                    ? "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300"
                    : acc.claude_usage_probe_at
                      ? "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-300"
                      : "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300",
                )}
                title={acc.claude_usage_probe_error || t("claude.samplingState.notSampled")}
              >
                {acc.claude_usage_probe_error
                  ? t("claude.samplingState.error")
                  : acc.claude_usage_probe_at
                    ? t("claude.samplingState.sampled")
                    : t("claude.samplingState.unsampled")}
              </span>
            ) : null}
          </div>
          {acc.claude_api ? (
            <div className="text-[10px] text-muted-foreground" title={acc.claude_usage_probe_error || undefined}>
              {t("claude.lastSample")}: {acc.claude_usage_probe_at ? formatRelativeShort(acc.claude_usage_probe_at, t) : t("claude.samplingState.notSampled")}
              {acc.claude_usage_probe_error ? ` · ${acc.claude_usage_probe_error}` : ""}
            </div>
          ) : null}
          <AccountHealthBar buckets={healthBuckets} />
        </div>
      </td>
      ) : null}
      {columns.today ? (
      <td className="px-2 py-3 text-center">
        {today ? (
          <div className="inline-flex flex-col items-center space-y-1 whitespace-nowrap text-[12px] tabular-nums">
            <div className="flex items-center gap-1.5">
              <span className={cn("inline-flex items-center gap-0.5", (today.requests ?? 0) > 0 ? "font-semibold text-foreground" : "text-muted-foreground/50")}>
                <Activity className={cn("size-3", (today.requests ?? 0) > 0 ? "text-sky-500" : "text-muted-foreground/40")} aria-hidden />
                {(today.requests ?? 0).toLocaleString()}
              </span>
              <span className={cn("inline-flex items-center gap-0.5", (today.tokens ?? 0) > 0 ? "font-semibold text-foreground" : "text-muted-foreground/50")}>
                <Sparkles className={cn("size-3", (today.tokens ?? 0) > 0 ? "text-purple-500 dark:text-purple-400" : "text-muted-foreground/40")} aria-hidden />
                {formatCompactNum(today.tokens)}
              </span>
            </div>
            <span
              className={cn(
                "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10px] ring-1 ring-inset",
                todayBilled > 0
                  ? "bg-emerald-500/10 font-medium text-emerald-700 ring-emerald-500/20 dark:text-emerald-400"
                  : "bg-slate-500/10 text-slate-500 ring-slate-500/20 dark:text-slate-400",
              )}
            >
              <Coins className={cn("size-2.5", todayBilled > 0 ? "text-emerald-500" : "opacity-50")} aria-hidden />
              ${todayBilled > 0 ? (todayBilled < 0.01 ? "<0.01" : todayBilled.toFixed(2)) : "0.00"}
            </span>
          </div>
        ) : (
          <span className="font-mono text-xs text-muted-foreground/40">-</span>
        )}
      </td>
      ) : null}
      {columns.requests ? (
      <td className="px-2 py-3 text-center">
        <div className="flex justify-center">
          <RequestCountPills account={acc} compact />
        </div>
      </td>
      ) : null}
      {columns.usage ? (
      <td className="px-2 py-3">
        <div className="flex items-center justify-center gap-1.5">
          <div className="min-w-0 space-y-1">
            {pct5h !== null || pct7d !== null || acc.usage_5h_detail || acc.usage_7d_detail ? (
              <>
                <UsageWindow label={t("claude.usage5h")} pct={pct5h} reset={acc.reset_5h_at} resetLabel={t("claude.resetIn")} detail={acc.usage_5h_detail} />
                <UsageWindow label={t("claude.usage7d")} pct={pct7d} reset={acc.reset_7d_at} resetLabel={t("claude.resetIn")} detail={acc.usage_7d_detail} />
              </>
            ) : (
              <span className="text-xs text-muted-foreground/50">-</span>
            )}
          </div>
          <UsageRefreshButton onRefresh={onUsageRefreshed} title={t("accounts.refreshUsage")} />
        </div>
      </td>
      ) : null}
      {columns.cost ? (
      <td className="px-2 py-3 text-center">
        <span className="inline-flex items-center whitespace-nowrap rounded-md bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
          5h: ${billed5h.toFixed(2)} / 7d: ${billed7d.toFixed(2)}
        </span>
      </td>
      ) : null}
      {columns.importTime ? (
      <td className="whitespace-nowrap px-2 py-3 text-center text-xs tabular-nums text-muted-foreground" title={created?.title}>
        {created?.label ?? "-"}
      </td>
      ) : null}
      {columns.updatedAt ? (
      <td className="whitespace-nowrap px-2 py-3 text-center text-xs text-muted-foreground">{formatRelativeShort(acc.updated_at, t)}</td>
      ) : null}
      {/* 操作 */}
      <td className="px-2 py-3">
        <div className="flex items-center justify-end gap-0.5">
          <button type="button" className={iconBtn} onClick={onEdit} title={t("claude.editTitle")} aria-label={t("claude.editTitle")}>
            <Pencil className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onUsage} title={t("accounts.actionUsageDetail")} aria-label={t("accounts.actionUsageDetail")}>
            <BarChart3 className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onRefreshModels} title={t("claude.refreshModels")} aria-label={t("claude.refreshModels")}>
            <RefreshCw className="size-3.5" />
          </button>
          <button
            type="button"
            className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-rose-500/10 hover:text-rose-600 dark:hover:text-rose-400"
            onClick={onDelete}
            title={t("common.delete")}
            aria-label={t("common.delete")}
          >
            <Trash2 className="size-3.5" />
          </button>
          <RowOverflowMenu
            items={[
              { key: "refresh", label: t("common.refresh"), onClick: onRefresh },
              { key: "reset", label: t("claude.resetStatus"), onClick: onResetStatus },
              { key: "lock", label: acc.locked ? t("claude.unlock") : t("claude.lock"), onClick: onToggleLock },
              { key: "toggle", label: disabled ? t("claude.enable") : t("claude.disable"), onClick: onToggleEnabled },
            ]}
          />
        </div>
      </td>
    </tr>
  );
}

// ColumnsMenu 列显隐下拉(与 Codex 的列控制一致):勾选切换,状态持久化到 localStorage。
function ColumnsMenu({
  visible,
  onChange,
}: {
  visible: ClaudeColVisibility;
  onChange: (next: ClaudeColVisibility) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  const labelFor: Record<ClaudeCol, string> = {
    groups: t("accounts.groupsLabel"),
    priority: t("accounts.schedulerPriorityColumn"),
    plan: t("accounts.plan"),
    status: t("accounts.status"),
    today: t("claude.todayLabel"),
    requests: t("accounts.requests"),
    usage: t("accounts.usage"),
    cost: t("claude.costLabel"),
    importTime: t("accounts.importTime"),
    updatedAt: t("accounts.updatedAt"),
  };
  const hiddenCount = CLAUDE_TOGGLE_COLUMNS.filter((c) => !visible[c]).length;

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
        aria-expanded={open}
      >
        <Columns3 className="size-3.5" />
        {t("claude.columns")}
        {hiddenCount > 0 ? <span className="tabular-nums opacity-70">({hiddenCount})</span> : null}
      </button>
      {open ? (
        <div className="absolute right-0 top-full z-40 mt-1 w-44 overflow-hidden rounded-lg border border-border bg-popover py-1 shadow-lg">
          {CLAUDE_TOGGLE_COLUMNS.map((c) => (
            <label key={c} className="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted">
              <input
                type="checkbox"
                checked={visible[c]}
                onChange={() => onChange({ ...visible, [c]: !visible[c] })}
              />
              {labelFor[c]}
            </label>
          ))}
        </div>
      ) : null}
    </div>
  );
}

// UsageRefreshButton 用量刷新按钮:点击时旋转动画,请求完成后停止(与全站刷新按钮一致)。
function UsageRefreshButton({ onRefresh, title }: { onRefresh: () => void | Promise<void>; title: string }) {
  const [spinning, setSpinning] = useState(false);
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      disabled={spinning}
      onClick={async () => {
        setSpinning(true);
        try {
          await onRefresh();
        } finally {
          setSpinning(false);
        }
      }}
      className="mt-0.5 shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-60"
    >
      <RefreshCw className={cn("size-3", spinning && "animate-spin")} />
    </button>
  );
}

// RowOverflowMenu "…" 溢出菜单:表格在 overflow 容器内,菜单用 fixed 定位避免被裁剪。
function RowOverflowMenu({
  items,
}: {
  items: Array<{ key: string; label: string; onClick: () => void; danger?: boolean }>;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ top: number; right: number } | null>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const close = () => setOpen(false);
    const onDown = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node) && !btnRef.current?.contains(e.target as Node)) close();
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") close();
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [open]);

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
        onClick={() => {
          const rect = btnRef.current?.getBoundingClientRect();
          if (rect) setPos({ top: rect.bottom + 4, right: Math.max(8, window.innerWidth - rect.right) });
          setOpen((v) => !v);
        }}
        aria-expanded={open}
        aria-label="more"
      >
        <MoreHorizontal className="size-3.5" />
      </button>
      {open && pos ? (
        <div
          ref={menuRef}
          className="fixed z-50 w-32 overflow-hidden rounded-lg border border-border bg-popover py-1 shadow-lg"
          style={{ top: pos.top, right: pos.right }}
        >
          {items.map((item) => (
            <button
              key={item.key}
              type="button"
              onClick={() => {
                setOpen(false);
                item.onClick();
              }}
              className={cn(
                "block w-full px-3 py-1.5 text-left text-xs transition-colors hover:bg-muted",
                item.danger ? "text-rose-600 dark:text-rose-400" : "text-foreground",
              )}
            >
              {item.label}
            </button>
          ))}
        </div>
      ) : null}
    </>
  );
}

// ── 账号分组指派弹窗 ──────────────────────────────────────
function AssignGroupsModal({
  account,
  groups,
  onClose,
  onSaved,
  onGroupsChanged,
}: {
  account: AccountRow;
  groups: AccountGroup[];
  onClose: () => void;
  onSaved: () => void;
  onGroupsChanged?: () => void | Promise<void>;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [selected, setSelected] = useState<number[]>(account.group_ids ?? []);
  const [busy, setBusy] = useState(false);

  // 内联建组:与其他页一致,复用 createAccountGroup(channel=claude),返回新 id 供自动勾选。
  const createGroupInline = useCallback(
    async (name: string): Promise<number | null> => {
      try {
        // 颜色按调色板循环取(与 Codex 内联建组一致),避免新组都是同一颜色。
        const color = ACCOUNT_GROUP_COLORS[groups.length % ACCOUNT_GROUP_COLORS.length];
        const res = await api.createAccountGroup({ name: name.trim(), channel: "claude", color });
        // 新组即时同步到父级 claudeGroups,保证保存后行内芯片能从 groupMap 取到它。
        await onGroupsChanged?.();
        return res.id ?? null;
      } catch (error) {
        showToast(getErrorMessage(error), "error");
        return null;
      }
    },
    [groups.length, onGroupsChanged, showToast],
  );

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await api.batchUpdateAccounts({ ids: [account.id], group_ids: selected });
      showToast(t("claude.groupsUpdated"), "success");
      onSaved();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [account.id, selected, onSaved, showToast, t]);

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.assignGroupsTitle")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void save()} disabled={busy}>
            {t("claude.save")}
          </Button>
        </div>
      }
    >
      <div className="space-y-2">
        <p className="text-xs text-muted-foreground">{account.email || account.name || `#${account.id}`}</p>
        <AccountGroupMultiSelect
          groups={groups}
          value={selected}
          onChange={setSelected}
          allLabel={t("accounts.groupsUnbound")}
          selectedLabel={t("accounts.groupsSelected", { count: selected.length })}
          placeholder={t("accounts.importGroupsPlaceholder")}
          emptyLabel={t("accounts.groupsNone")}
          emptyHint={t("accounts.groupsSelectHint")}
          onCreateGroup={createGroupInline}
          createLabel={t("accounts.groupCreate")}
          createPlaceholder={t("accounts.groupNamePlaceholder")}
          creatingLabel={t("accounts.groupCreating")}
          createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
        />
      </div>
    </Modal>
  );
}

// ── 账号编辑弹窗:仅 Claude 账号真实可调的字段 ─────────────
// 代理(影响出站 IP 一致性)、标签、调度优先级、5h/7d 自动暂停阈值
// (阈值对照 Anthropic 统一限流头回填的真实窗口利用率)。
function EditAccountModal({
  account,
  proxies,
  onClose,
  onSaved,
}: {
  account: AccountRow;
  proxies: ProxyRow[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [proxyUrl, setProxyUrl] = useState(account.proxy_url ?? "");
  const [tags, setTags] = useState((account.tags ?? []).join(", "));
  const [priority, setPriority] = useState(
    account.scheduler_priority != null ? String(account.scheduler_priority) : "",
  );
  const [scoreBias, setScoreBias] = useState(
    account.score_bias_override != null ? String(account.score_bias_override) : "",
  );
  const [concurrency, setConcurrency] = useState(
    account.base_concurrency_override != null ? String(account.base_concurrency_override) : "",
  );
  const [pause5h, setPause5h] = useState(
    account.auto_pause_5h_threshold != null ? String(account.auto_pause_5h_threshold) : "",
  );
  const [pause7d, setPause7d] = useState(
    account.auto_pause_7d_threshold != null ? String(account.auto_pause_7d_threshold) : "",
  );
  const [fpMode, setFpMode] = useState<"" | "preserve" | "force">(
    (account.claude_fingerprint_mode as "" | "preserve" | "force") ?? "",
  );
  const [timezone, setTimezone] = useState(account.timezone ?? "");
  const [busy, setBusy] = useState(false);

  const parseNum = (v: string): number | null => {
    const s = v.trim();
    if (!s) return null;
    const n = Number(s);
    return Number.isFinite(n) ? n : null;
  };

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await api.updateAccountScheduler(account.id, {
        proxy_url: proxyUrl.trim() || null,
        tags: tags
          .split(/[,，]/)
          .map((s) => s.trim())
          .filter(Boolean),
        scheduler_priority: parseNum(priority),
        score_bias_override: parseNum(scoreBias),
        base_concurrency_override: parseNum(concurrency),
        auto_pause_5h_threshold: parseNum(pause5h),
        auto_pause_7d_threshold: parseNum(pause7d),
        claude_fingerprint_mode: fpMode,
        timezone: timezone.trim(),
      });
      showToast(t("claude.saved"), "success");
      // 手动输入的代理若不在代理管理中,询问是否存入(需在关闭弹窗前完成)。
      await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onSaved();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [account.id, proxyUrl, proxies, confirm, tags, priority, scoreBias, concurrency, pause5h, pause7d, fpMode, timezone, onSaved, showToast, t]);

  const field = (label: string, node: ReactNode, hint?: string) => (
    <div className="space-y-1">
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      {node}
      {hint ? <p className="text-[10px] leading-tight text-muted-foreground/70">{hint}</p> : null}
    </div>
  );

  const selectCls =
    "h-9 w-full rounded-md border border-input bg-background px-2 text-sm text-foreground outline-none focus-visible:border-ring";

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.editTitle")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void save()} disabled={busy}>
            {t("claude.save")}
          </Button>
        </div>
      }
    >
      <div className="max-h-[70vh] space-y-4 overflow-y-auto pr-1">
        <p className="text-xs text-muted-foreground">{account.email || account.name || `#${account.id}`}</p>

        {/* 身份/网络 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionIdentity")}
          </div>
          {field(
            t("claude.proxyLabel"),
            <ProxyField value={proxyUrl} onChange={setProxyUrl} proxies={proxies} label="" />,
            t("claude.proxyHint"),
          )}
          {field(
            t("claude.fingerprintModeLabel"),
            <select className={selectCls} value={fpMode} onChange={(e) => setFpMode(e.target.value as "" | "preserve" | "force")}>
              <option value="">{t("claude.fpFollowGlobal")}</option>
              <option value="preserve">{t("claude.fpPreserve")}</option>
              <option value="force">{t("claude.fpForce")}</option>
            </select>,
            t("claude.fingerprintModeHint"),
          )}
          {field(
            t("claude.timezoneLabelEdit"),
            <Input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai" />,
            t("claude.timezoneHint"),
          )}
        </div>

        {/* 调度 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionScheduling")}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {field(
              t("claude.concurrencyLabel"),
              <Input value={concurrency} onChange={(e) => setConcurrency(e.target.value)} placeholder={t("claude.followGlobalPlaceholder")} inputMode="numeric" />,
              t("claude.concurrencyHint"),
            )}
            {field(
              t("claude.priorityLabel"),
              <Input value={priority} onChange={(e) => setPriority(e.target.value)} placeholder="0" inputMode="numeric" />,
            )}
            {field(
              t("claude.scoreBiasLabel"),
              <Input value={scoreBias} onChange={(e) => setScoreBias(e.target.value)} placeholder="0" inputMode="numeric" />,
              t("claude.scoreBiasHint"),
            )}
          </div>
        </div>

        {/* 自动暂停 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionAutoPause")}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {field(t("claude.autoPause5hLabel"), <Input value={pause5h} onChange={(e) => setPause5h(e.target.value)} placeholder="90" inputMode="numeric" />)}
            {field(t("claude.autoPause7dLabel"), <Input value={pause7d} onChange={(e) => setPause7d(e.target.value)} placeholder="90" inputMode="numeric" />)}
          </div>
        </div>

        {/* 标签 */}
        {field(
          t("claude.tagsLabel"),
          <Input value={tags} onChange={(e) => setTags(e.target.value)} placeholder={t("claude.tagsPlaceholder")} />,
        )}
      </div>
      {confirmDialog}
    </Modal>
  );
}

// ── 添加账号弹窗:网页 OAuth 两步式 / 导入 token JSON ──────
function ClaudeAddModal({
  proxies,
  groups,
  onClose,
  onAdded,
}: {
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onClose: () => void;
  onAdded: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [tab, setTab] = useState<"oauth" | "import">("oauth");

  const [proxyUrl, setProxyUrl] = useState("");
  const [useProxyPool, setUseProxyPool] = useState(false);
  const [name, setName] = useState("");
  const [timezone, setTimezone] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [groupIds, setGroupIds] = useState<Set<number>>(new Set());

  const [authUrl, setAuthUrl] = useState("");
  const [state, setState] = useState("");
  const [callback, setCallback] = useState("");
  const [tokenJson, setTokenJson] = useState("");

  const toggleGroup = useCallback((id: number) => {
    setGroupIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  // 添加成功后,如选择了分组则批量指派(用新账号返回的 id)。
  const applyGroups = useCallback(
    async (id?: number) => {
      if (groupIds.size === 0 || !id) return;
      try {
        await api.batchUpdateAccounts({ ids: [id], group_ids: Array.from(groupIds) });
      } catch {
        /* 分组指派失败不阻断添加流程 */
      }
    },
    [groupIds],
  );

  const genAuthUrl = useCallback(async () => {
    try {
      const res = await api.generateClaudeAuthURL();
      setAuthUrl(res.auth_url);
      setState(res.state);
      window.open(res.auth_url, "_blank", "noopener,noreferrer");
    } catch (error) {
      showToast(t("claude.authUrlFailed") + ": " + getErrorMessage(error), "error");
    }
  }, [showToast, t]);

  const submitOAuth = useCallback(async () => {
    const code = extractCode(callback);
    if (!state || !code) {
      showToast(t("claude.exchangeFailed"), "error");
      return;
    }
    setSubmitting(true);
    try {
      const res = await api.exchangeClaudeOAuthCode({
        state,
        code,
        name: name.trim() || undefined,
        proxy_url: useProxyPool ? undefined : proxyUrl.trim() || undefined,
        use_proxy_pool: useProxyPool || undefined,
        timezone: timezone.trim() || undefined,
      });
      await applyGroups(res?.id);
      showToast(t("claude.added"), "success");
      if (!useProxyPool) await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onAdded();
    } catch (error) {
      showToast(t("claude.exchangeFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [callback, name, onAdded, proxyUrl, proxies, confirm, showToast, state, t, timezone, useProxyPool, applyGroups]);

  const submitImport = useCallback(async () => {
    let parsed: Partial<ClaudeImportTokenRequest>;
    try {
      parsed = JSON.parse(tokenJson) as Partial<ClaudeImportTokenRequest>;
    } catch {
      showToast(t("claude.invalidJson"), "error");
      return;
    }
    if (!parsed.access_token || !parsed.refresh_token) {
      showToast(t("claude.invalidJson"), "error");
      return;
    }
    setSubmitting(true);
    try {
      const res = await api.importClaudeToken({
        access_token: parsed.access_token,
        refresh_token: parsed.refresh_token,
        email: parsed.email,
        account_id: parsed.account_id,
        expires_at: parsed.expires_at,
        name: name.trim() || undefined,
        proxy_url: useProxyPool ? undefined : proxyUrl.trim() || undefined,
        use_proxy_pool: useProxyPool || undefined,
        timezone: timezone.trim() || undefined,
      });
      await applyGroups(res?.id);
      showToast(t("claude.added"), "success");
      if (!useProxyPool) await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onAdded();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [name, onAdded, proxyUrl, proxies, confirm, showToast, t, timezone, tokenJson, useProxyPool, applyGroups]);

  const commonFields = (
    <div className="space-y-2">
      <ProxyField value={proxyUrl} onChange={setProxyUrl} proxies={proxies} label={t("claude.proxyLabel")} disabled={useProxyPool} />
      <label className="flex items-center gap-2 text-xs text-muted-foreground">
        <input type="checkbox" checked={useProxyPool} onChange={(e) => setUseProxyPool(e.target.checked)} />
        {t("claude.useProxyPool")}
      </label>
      <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={t("claude.namePlaceholder")} />
      <Input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder={t("claude.timezonePlaceholder")} />
      {groups.length > 0 ? (
        <div className="space-y-1">
          <span className="text-xs font-semibold text-muted-foreground">{t("claude.filterGroup")}</span>
          <div className="flex flex-wrap gap-1.5">
            {groups.map((g) => {
              const on = groupIds.has(g.id);
              return (
                <button
                  key={g.id}
                  type="button"
                  onClick={() => toggleGroup(g.id)}
                  className={cn(
                    "inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] transition-colors",
                    on ? "border-transparent text-white" : "border-border text-muted-foreground",
                  )}
                  style={on ? { backgroundColor: normalizeGroupColor(g.color) } : undefined}
                >
                  <span className="size-2 rounded-full" style={{ backgroundColor: normalizeGroupColor(g.color) }} />
                  {g.name}
                </button>
              );
            })}
          </div>
        </div>
      ) : null}
    </div>
  );

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.addAccount")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          {tab === "oauth" ? (
            <Button onClick={() => void submitOAuth()} disabled={submitting}>
              {t("claude.exchange")}
            </Button>
          ) : (
            <Button onClick={() => void submitImport()} disabled={submitting}>
              {t("claude.import")}
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4">
        <div className="flex gap-2">
          <Button variant={tab === "oauth" ? "default" : "ghost"} size="sm" onClick={() => setTab("oauth")}>
            {t("claude.tabOAuth")}
          </Button>
          <Button variant={tab === "import" ? "default" : "ghost"} size="sm" onClick={() => setTab("import")}>
            {t("claude.tabImport")}
          </Button>
        </div>

        {tab === "oauth" ? (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("claude.step1")}</p>
            <div className="flex flex-wrap items-center gap-2">
              <Button variant="secondary" size="sm" onClick={() => void genAuthUrl()}>
                {t("claude.genAuthUrl")}
              </Button>
              {authUrl ? (
                <>
                  <a href={authUrl} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 text-xs text-primary underline">
                    <ExternalLink className="size-3" />
                    {t("claude.openAuth")}
                  </a>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      void navigator.clipboard?.writeText(authUrl);
                      showToast(t("claude.authUrlCopied"), "success");
                    }}
                  >
                    {t("claude.copyLink")}
                  </Button>
                </>
              ) : null}
            </div>
            {/* 生成后展示完整授权 URL(可读、可手动复制),而不是只给一个跳转链接 */}
            {authUrl ? (
              <textarea
                readOnly
                value={authUrl}
                rows={2}
                onFocus={(e) => e.currentTarget.select()}
                className="w-full resize-none rounded-md border border-input bg-muted/40 p-2 font-mono text-[11px] leading-snug text-muted-foreground outline-none"
              />
            ) : null}
            <p className="text-xs text-muted-foreground">{t("claude.step2")}</p>
            <Input value={callback} onChange={(e) => setCallback(e.target.value)} placeholder={t("claude.callbackPlaceholder")} />
            {commonFields}
          </div>
        ) : (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("claude.importHint")}</p>
            <textarea
              value={tokenJson}
              onChange={(e) => setTokenJson(e.target.value)}
              placeholder={t("claude.importPlaceholder")}
              rows={6}
              className="w-full rounded-md border border-input bg-background p-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/20"
            />
            {commonFields}
          </div>
        )}
      </div>
      {confirmDialog}
    </Modal>
  );
}
