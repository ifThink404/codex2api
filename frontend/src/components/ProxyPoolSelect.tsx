import { useTranslation } from "react-i18next";

import type { ProxyRow } from "../api";
import { Select, type SelectOption } from "./ui/select";

interface ProxyPoolSelectProps {
  proxies: ProxyRow[];
  onSelect: (url: string) => void;
  disabled?: boolean;
  className?: string;
}

// ProxyPoolSelect 是账号表单里"从代理池选一条代理填入代理输入框"的下拉。
// 它不持有选中状态——选中后把该代理的 URL 交给 onSelect（由上层写进代理输入框，
// 仍可手动编辑），下拉本身回到占位符。代理池为空时不渲染（无可选项）。
//
// 每个选项展示该代理已绑定的账号数（bound_count），空闲代理（0 绑定）置顶并标注
// "空闲"，便于按负载均衡挑选，避免把新账号都堆到同一条代理上（IP 过载易风控）。
export function ProxyPoolSelect({
  proxies,
  onSelect,
  disabled = false,
  className,
}: ProxyPoolSelectProps) {
  const { t } = useTranslation();
  if (proxies.length === 0) {
    return null;
  }
  // 空闲（bound_count=0）优先，其余按绑定数升序，让负载最轻的代理排在前面。
  const sorted = [...proxies].sort(
    (a, b) => (a.bound_count ?? 0) - (b.bound_count ?? 0),
  );
  const options: SelectOption[] = sorted.map((proxy) => {
    const label = proxy.label?.trim();
    const base = label ? `${label} — ${proxy.url}` : proxy.url;
    const count = proxy.bound_count ?? 0;
    const bindTag = count === 0 ? t("proxies.idle") : t("proxies.boundCount", { count });
    return {
      value: proxy.url,
      // 绑定数/空闲放在最前，避免长 URL 被 truncate 截断后看不到负载信息。
      label: `[${bindTag}]  ${base}`,
      triggerLabel: label || proxy.url,
    };
  });
  return (
    <Select
      compact
      className={className}
      value=""
      placeholder={t("proxies.selectFromPool")}
      disabled={disabled}
      options={options}
      onValueChange={(url) => {
        if (url.trim()) onSelect(url);
      }}
    />
  );
}
