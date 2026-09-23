import { useEffect, useState } from "react";
import { api } from "../api";
import type { AccountRow } from "../types";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { isBPSAccount } from "../lib/accountModelAvailability";
import { getErrorMessage } from "../utils/error";

export default function BPSAccountSettings() {
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [busy, setBusy] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api
      .getAccountsPage(
        { channel: "codex", page, pageSize: 20 },
        controller.signal,
      )
      .then((res) => {
        setAccounts(res.accounts);
        setTotal(res.total);
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(getErrorMessage(e));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, nonce]);
  const toggle = async (account: AccountRow, enabled: boolean) => {
    setBusy(account.id);
    setError("");
    try {
      await api.updateAccountScheduler(account.id, {
        codex_bps_enabled: enabled,
      });
      const saved = await api.getAccount(account.id);
      setAccounts((prev) => prev.map((a) => (a.id === saved.id ? saved : a)));
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        按账户开启，保存后立即生效。仅影响 HTTP Responses /
        compact；模型检测结果也按 BPS 与 Codex 分开记录。
      </p>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setNonce((n) => n + 1)}
          >
            重试
          </Button>
        </p>
      )}
      {loading ? (
        <p role="status" className="text-sm text-muted-foreground">
          正在读取账户…
        </p>
      ) : (
        <div className="divide-y divide-border">
          {accounts.filter(isBPSAccount).map((a) => (
            <div
              key={a.id}
              className="flex items-center justify-between gap-3 py-2"
            >
              <div className="min-w-0 text-sm">
                <span className="block truncate">
                  {a.name || a.email || `账户 ${a.id}`}
                </span>
                <span className="text-xs text-muted-foreground">
                  #{a.id} · {a.plan_type} ·{" "}
                  {a.codex_bps_enabled ? "BPS 已开启" : "普通 Codex"}
                </span>
              </div>
              <Switch
                aria-label={`账户 ${a.id} BPS 通道`}
                checked={a.codex_bps_enabled ?? false}
                disabled={busy !== null}
                onCheckedChange={(checked) => void toggle(a, checked)}
              />
            </div>
          ))}
          {!accounts.some(isBPSAccount) && (
            <p className="text-xs text-muted-foreground py-2">
              本页没有支持 BPS 的账户。
            </p>
          )}
        </div>
      )}
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span>
          第 {page} / {Math.max(1, Math.ceil(total / 20))} 页
        </span>
        <div className="flex gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={page <= 1 || loading || busy !== null}
            onClick={() => setPage((p) => p - 1)}
          >
            上一页
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={page * 20 >= total || loading || busy !== null}
            onClick={() => setPage((p) => p + 1)}
          >
            下一页
          </Button>
        </div>
      </div>
    </div>
  );
}
