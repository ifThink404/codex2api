import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { api } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountRow,
  AccountListSummary,
  ClaudeImportTokenRequest,
} from "../types";

type ClaudeStatusFilter =
  | "all"
  | "normal"
  | "rate_limited"
  | "abnormal"
  | "error"
  | "disabled"
  | "locked";

// rowMatchesStatus 按筛选项判断账号是否命中(与后端 summary 计数口径对齐)。
function rowMatchesStatus(acc: AccountRow, filter: ClaudeStatusFilter): boolean {
  const s = (acc.status || "").toLowerCase();
  switch (filter) {
    case "all":
      return true;
    case "rate_limited":
      return s.includes("rate") || s === "cooldown";
    case "abnormal":
      return s === "unauthorized" || s === "error" || s === "banned";
    case "error":
      return s === "error";
    case "disabled":
      return acc.enabled === false;
    case "locked":
      return Boolean(acc.locked);
    case "normal":
      return (
        acc.enabled !== false &&
        !acc.locked &&
        (s === "active" || s === "ready" || s === "normal" || s === "")
      );
    default:
      return true;
  }
}

// claudeUsagePct 取用量百分比(0-100),无则 null。
function claudeUsagePct(v: unknown): number | null {
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) && n > 0 ? Math.min(100, Math.round(n)) : null;
}
import { ProxyPoolSelect } from "../components/ProxyPoolSelect";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StatusBadge from "../components/StatusBadge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";

// extractCode 从粘贴内容里取授权码：支持整条回调 URL、code#state、或纯 code。
// 与 cmd/claude_login 的解析保持一致（后端 exchange 端点只收 code）。
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

export default function ClaudeAccounts({
  headerSlot,
}: {
  headerSlot?: ReactNode;
} = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [summary, setSummary] = useState<AccountListSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [showAdd, setShowAdd] = useState(false);
  const [query, setQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<ClaudeStatusFilter>("all");

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      const res = await api.getAccountsPage({
        channel: "claude",
        page: 1,
        pageSize: 100,
        sort: "updated_at",
        order: "desc",
      });
      setAccounts(res.accounts ?? []);
      setSummary(res.summary ?? null);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setLoading(false);
    }
  }, [showToast]);

  useEffect(() => {
    void reload();
  }, [reload]);

  useEffect(() => {
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
  }, []);

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

  const filteredAccounts = useMemo(() => {
    const q = query.trim().toLowerCase();
    return accounts.filter((acc) => {
      if (!rowMatchesStatus(acc, statusFilter)) return false;
      if (!q) return true;
      return (
        (acc.email || "").toLowerCase().includes(q) ||
        (acc.name || "").toLowerCase().includes(q) ||
        (acc.models || []).some((m) => m.toLowerCase().includes(q))
      );
    });
  }, [accounts, query, statusFilter]);

  // 状态筛选项 + 计数(优先用后端 summary,回退到本地统计)。
  const statChips = useMemo(() => {
    const localCount = (f: ClaudeStatusFilter) =>
      accounts.filter((a) => rowMatchesStatus(a, f)).length;
    const s = summary;
    const chips: Array<{ id: ClaudeStatusFilter; label: string; count: number; tone?: string }> = [
      { id: "all", label: t("claude.statAll"), count: s?.total ?? accounts.length },
      { id: "normal", label: t("claude.statNormal"), count: s?.normal ?? localCount("normal"), tone: "text-emerald-600 dark:text-emerald-400" },
      { id: "rate_limited", label: t("claude.statRateLimited"), count: s?.rate_limited ?? localCount("rate_limited"), tone: "text-amber-600 dark:text-amber-400" },
      { id: "abnormal", label: t("claude.statAbnormal"), count: s?.abnormal ?? localCount("abnormal"), tone: "text-rose-600 dark:text-rose-400" },
      { id: "error", label: t("claude.statError"), count: s?.error ?? localCount("error"), tone: "text-rose-600 dark:text-rose-400" },
      { id: "disabled", label: t("claude.statDisabled"), count: s?.disabled ?? localCount("disabled") },
      { id: "locked", label: t("claude.statLocked"), count: s?.locked ?? localCount("locked") },
    ];
    return chips;
  }, [accounts, summary, t]);

  const healthChips = useMemo(() => {
    const s = summary;
    return [
      { label: t("claude.healthHealthy"), count: s?.healthy ?? 0, dot: "bg-emerald-500" },
      { label: t("claude.healthWarm"), count: s?.warm ?? 0, dot: "bg-amber-500" },
      { label: t("claude.healthRisky"), count: s?.risky ?? 0, dot: "bg-rose-500" },
    ];
  }, [summary, t]);

  return (
    <div>
      <PageHeader
        title={t("claude.title")}
        description={t("claude.subtitle")}
        titleAdornment={headerSlot}
        onRefresh={() => void reload()}
        actions={
          <Button onClick={() => setShowAdd(true)}>
            {t("claude.addAccount")}
          </Button>
        }
      />

      {/* 统计 + 调度视图 + 搜索 */}
      {accounts.length > 0 || summary ? (
        <div className="mb-4 space-y-3">
          <div className="flex flex-wrap items-center gap-1.5">
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
                  <span className="tabular-nums rounded-md bg-background/60 px-1 text-[10px] font-bold">
                    {chip.count}
                  </span>
                </button>
              );
            })}
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <span className="text-[11px] font-medium text-muted-foreground">
              {t("claude.schedulingView")}
            </span>
            {healthChips.map((h) => (
              <span key={h.label} className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                <span className={cn("size-1.5 rounded-full", h.dot)} />
                {h.label}
                <span className="tabular-nums font-semibold text-foreground">{h.count}</span>
              </span>
            ))}
          </div>
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t("claude.searchPlaceholder")}
            className="max-w-md"
          />
        </div>
      ) : null}

      {loading ? (
        <div className="py-16 text-center text-sm text-muted-foreground">
          {t("common.loading")}
        </div>
      ) : accounts.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border py-16 text-center text-sm text-muted-foreground">
          {t("claude.empty")}
        </div>
      ) : filteredAccounts.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
          {t("claude.emptyFiltered")}
        </div>
      ) : (
        <div className="space-y-2">
          {filteredAccounts.map((acc) => {
            const pct5h = claudeUsagePct(acc.usage_percent_5h);
            const pct7d = claudeUsagePct(acc.usage_percent_7d);
            const modelCount = (acc.models || []).length;
            const cooldownReason = (acc.status || "").toLowerCase().includes("rate")
              ? acc.error_message
              : "";
            return (
              <div
                key={acc.id}
                className="flex flex-col gap-2.5 rounded-lg border border-border bg-card p-3 sm:flex-row sm:items-center sm:justify-between"
              >
                <div className="flex min-w-0 items-center gap-2.5">
                  <ChannelLogo channel="claude" size={20} />
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium text-foreground">
                      {acc.email || acc.name || `#${acc.id}`}
                    </div>
                    <div className="truncate text-xs text-muted-foreground">
                      {acc.plan_type || "claude"}
                      {modelCount > 0 ? ` · ${t("claude.modelCount", { count: modelCount })}` : ""}
                      {acc.proxy_url ? ` · ${acc.proxy_url}` : ""}
                    </div>
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                  {/* 5h / 7d 用量 */}
                  {pct5h !== null || pct7d !== null ? (
                    <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
                      {pct5h !== null ? (
                        <span className="inline-flex items-center gap-1">
                          5h
                          <span className="h-1.5 w-14 overflow-hidden rounded-full bg-muted">
                            <span
                              className={cn("block h-full rounded-full", pct5h >= 90 ? "bg-rose-500" : pct5h >= 70 ? "bg-amber-500" : "bg-emerald-500")}
                              style={{ width: `${pct5h}%` }}
                            />
                          </span>
                          <span className="tabular-nums">{pct5h}%</span>
                        </span>
                      ) : null}
                      {pct7d !== null ? (
                        <span className="inline-flex items-center gap-1">
                          7d
                          <span className="h-1.5 w-14 overflow-hidden rounded-full bg-muted">
                            <span
                              className={cn("block h-full rounded-full", pct7d >= 90 ? "bg-rose-500" : pct7d >= 70 ? "bg-amber-500" : "bg-emerald-500")}
                              style={{ width: `${pct7d}%` }}
                            />
                          </span>
                          <span className="tabular-nums">{pct7d}%</span>
                        </span>
                      ) : null}
                    </div>
                  ) : null}
                  <StatusBadge status={acc.status} errorMessage={acc.error_message} detail={cooldownReason} />
                  <div className="flex items-center gap-1.5">
                    <Button variant="ghost" size="sm" onClick={() => void handleRefresh(acc)}>
                      {t("common.refresh")}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => void handleRefreshModels(acc)}>
                      {t("claude.refreshModels")}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => void handleDelete(acc)}>
                      {t("common.delete")}
                    </Button>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {showAdd ? (
        <ClaudeAddModal
          proxies={proxyPool}
          onClose={() => setShowAdd(false)}
          onAdded={() => {
            setShowAdd(false);
            void reload();
          }}
        />
      ) : null}
      {confirmDialog}
    </div>
  );
}

// ClaudeAddModal 提供两种添加方式：网页 OAuth 两步式 / 导入 token JSON。
function ClaudeAddModal({
  proxies,
  onClose,
  onAdded,
}: {
  proxies: ProxyRow[];
  onClose: () => void;
  onAdded: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [tab, setTab] = useState<"oauth" | "import">("oauth");

  // 公共：代理选择 + 时区
  const [proxyUrl, setProxyUrl] = useState("");
  const [useProxyPool, setUseProxyPool] = useState(false);
  const [name, setName] = useState("");
  const [timezone, setTimezone] = useState("");
  const [submitting, setSubmitting] = useState(false);

  // OAuth 两步
  const [authUrl, setAuthUrl] = useState("");
  const [state, setState] = useState("");
  const [callback, setCallback] = useState("");

  // Import
  const [tokenJson, setTokenJson] = useState("");

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
      await api.exchangeClaudeOAuthCode({
        state,
        code,
        name: name.trim() || undefined,
        proxy_url: useProxyPool ? undefined : proxyUrl.trim() || undefined,
        use_proxy_pool: useProxyPool || undefined,
        timezone: timezone.trim() || undefined,
      });
      showToast(t("claude.added"), "success");
      onAdded();
    } catch (error) {
      showToast(t("claude.exchangeFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [callback, name, onAdded, proxyUrl, showToast, state, t, timezone, useProxyPool]);

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
      await api.importClaudeToken({
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
      showToast(t("claude.added"), "success");
      onAdded();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [name, onAdded, proxyUrl, showToast, t, timezone, tokenJson, useProxyPool]);

  const proxyFields = (
    <div className="space-y-2">
      <span className="text-xs font-semibold text-muted-foreground">
        {t("claude.proxyLabel")}
      </span>
      <Input
        value={proxyUrl}
        onChange={(e) => setProxyUrl(e.target.value)}
        placeholder="http://127.0.0.1:7890"
        disabled={useProxyPool}
      />
      <ProxyPoolSelect
        proxies={proxies}
        onSelect={setProxyUrl}
        disabled={useProxyPool}
      />
      <label className="flex items-center gap-2 text-xs text-muted-foreground">
        <input
          type="checkbox"
          checked={useProxyPool}
          onChange={(e) => setUseProxyPool(e.target.checked)}
        />
        {t("claude.useProxyPool")}
      </label>
      <Input
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder={t("claude.namePlaceholder")}
      />
      <Input
        value={timezone}
        onChange={(e) => setTimezone(e.target.value)}
        placeholder={t("claude.timezonePlaceholder")}
      />
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
          <Button
            variant={tab === "oauth" ? "default" : "ghost"}
            size="sm"
            onClick={() => setTab("oauth")}
          >
            {t("claude.tabOAuth")}
          </Button>
          <Button
            variant={tab === "import" ? "default" : "ghost"}
            size="sm"
            onClick={() => setTab("import")}
          >
            {t("claude.tabImport")}
          </Button>
        </div>

        {tab === "oauth" ? (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("claude.step1")}</p>
            <div className="flex gap-2">
              <Button variant="secondary" size="sm" onClick={() => void genAuthUrl()}>
                {t("claude.genAuthUrl")}
              </Button>
              {authUrl ? (
                <a
                  href={authUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center text-xs text-primary underline"
                >
                  {t("claude.openAuth")}
                </a>
              ) : null}
            </div>
            <p className="text-xs text-muted-foreground">{t("claude.step2")}</p>
            <Input
              value={callback}
              onChange={(e) => setCallback(e.target.value)}
              placeholder={t("claude.callbackPlaceholder")}
            />
            {proxyFields}
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
            {proxyFields}
          </div>
        )}
      </div>
    </Modal>
  );
}
