import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Clock, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// 出口时区与账号时区不一致的提示。指纹收敛把账号时区随请求下发，而真正的出站
// 落地在代理的 IP 时区上——两者不一致时上游看到的是"人在 A 区、IP 在 B 区"。
// 只在两边都有值且不同的时候出现，一键把账号时区改成出口时区。
export function ProxyTimezoneHint({
  proxyTimezone,
  accountTimezone,
  onSync,
  disabled = false,
  className,
}: {
  /** 代理池条目最近一次测试解析到的出口时区；空表示未测到，不提示。 */
  proxyTimezone?: string;
  /** 账号当前的指纹时区；空表示跟随全局默认，不提示。 */
  accountTimezone?: string;
  /** 省略则只提示、不给同步按钮（例如账号还没保存时）。 */
  onSync?: (timezone: string) => Promise<void>;
  disabled?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  const [syncing, setSyncing] = useState(false);

  const proxyTZ = (proxyTimezone ?? "").trim();
  const accountTZ = (accountTimezone ?? "").trim();
  if (!proxyTZ || !accountTZ || proxyTZ === accountTZ) return null;

  return (
    <div
      className={cn(
        "flex flex-wrap items-center gap-2 text-xs leading-relaxed text-amber-600 dark:text-amber-400",
        className,
      )}
    >
      <Clock className="size-3.5 shrink-0" />
      <span className="min-w-0">
        {t("accounts.proxyTimezoneMismatch", { proxy: proxyTZ, account: accountTZ })}
      </span>
      {onSync ? (
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="h-6 shrink-0 gap-1 px-2 text-xs"
          disabled={disabled || syncing}
          onClick={() => {
            if (syncing) return;
            setSyncing(true);
            void onSync(proxyTZ).finally(() => setSyncing(false));
          }}
        >
          {syncing ? <Loader2 className="size-3 animate-spin" /> : null}
          {t("accounts.proxyTimezoneSync", { tz: proxyTZ })}
        </Button>
      ) : null}
    </div>
  );
}

export default ProxyTimezoneHint;
