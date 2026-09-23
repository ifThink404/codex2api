import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

export type AccountModelScope = "account" | "codex" | "bps";
export type AccountModelDrafts = Record<AccountModelScope, string[]>;

export default function AccountModelScopeTabs(props: {
  value: AccountModelScope;
  onChange: (scope: AccountModelScope) => void;
  drafts: AccountModelDrafts;
  supportsBPS: boolean;
  markedScopes?: Record<AccountModelScope, boolean>;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const scopes: AccountModelScope[] = props.supportsBPS
    ? ["account", "codex", "bps"]
    : ["account", "codex"];
  return (
    <div role="group" className="flex gap-1 rounded-lg bg-muted/60 p-1" aria-label={t("accounts.modelScope")}>
      {scopes.map((scope) => (
        <button
          type="button"
          key={scope}
          onClick={() => props.onChange(scope)}
          aria-pressed={props.value === scope}
          disabled={props.disabled}
          className={cn(
            "flex flex-1 items-center justify-center gap-1.5 rounded-md px-2 py-2 text-xs font-medium text-muted-foreground transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50",
            props.value === scope && "bg-background text-foreground shadow-sm",
          )}
        >
          {scope === "account" ? t("accounts.accountModelAllowlist") : scope === "codex" ? "Codex" : "BPS"}
          {props.markedScopes?.[scope] && <span className="size-1.5 shrink-0 rounded-full bg-primary" aria-hidden="true" />}
          {props.drafts[scope].length > 0 && (
            <span className="rounded bg-muted px-1.5 text-[10px] tabular-nums">{props.drafts[scope].length}</span>
          )}
        </button>
      ))}
    </div>
  );
}
