import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { api } from "../api";
import type { ProxyRow } from "../api";
import type { AccountRow, ClaudeImportTokenRequest } from "../types";
import { ProxyPoolSelect } from "../components/ProxyPoolSelect";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StatusBadge from "../components/StatusBadge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
  const [loading, setLoading] = useState(true);
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [showAdd, setShowAdd] = useState(false);

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

      {loading ? (
        <div className="py-16 text-center text-sm text-muted-foreground">
          {t("common.loading")}
        </div>
      ) : accounts.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border py-16 text-center text-sm text-muted-foreground">
          {t("claude.empty")}
        </div>
      ) : (
        <div className="space-y-2">
          {accounts.map((acc) => (
            <div
              key={acc.id}
              className="flex items-center justify-between gap-3 rounded-lg border border-border bg-card p-3"
            >
              <div className="flex min-w-0 items-center gap-2.5">
                <ChannelLogo channel="claude" size={20} />
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium text-foreground">
                    {acc.email || acc.name || `#${acc.id}`}
                  </div>
                  <div className="truncate text-xs text-muted-foreground">
                    {acc.plan_type || "claude"}
                    {acc.proxy_url ? ` · ${acc.proxy_url}` : ""}
                  </div>
                </div>
              </div>
              <div className="flex shrink-0 items-center gap-1.5">
                <StatusBadge
                  status={acc.status}
                  errorMessage={acc.error_message}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void handleRefresh(acc)}
                >
                  {t("common.refresh")}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void handleDelete(acc)}
                >
                  {t("common.delete")}
                </Button>
              </div>
            </div>
          ))}
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
