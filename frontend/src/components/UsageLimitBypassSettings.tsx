import { useTranslation } from "react-i18next";
import ChipInput from "./ChipInput";
import { Switch } from "./ui/switch";

interface Props {
  enabled: boolean;
  onEnabledChange: (enabled: boolean) => void;
  models: string[];
  onModelsChange: (models: string[]) => void;
  disabled?: boolean;
  options?: string[];
}

export default function UsageLimitBypassSettings({ enabled, onEnabledChange, models, onModelsChange, disabled, options }: Props) {
  const { t } = useTranslation();
  return <div className="space-y-3">
    <div className="flex items-center justify-between gap-3">
      <span className="text-sm font-semibold">{t("accounts.usageLimitBypassTitle")}</span>
      <Switch checked={enabled} onCheckedChange={onEnabledChange} disabled={disabled} aria-label={t("accounts.usageLimitBypassTitle")} />
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{t("accounts.usageLimitBypassHint")}</p>
    <ChipInput value={models} onChange={onModelsChange} options={options} disabled={disabled || !enabled} placeholder={t("accounts.usageLimitBypassPlaceholder")} />
    <p className="text-xs text-muted-foreground">{t("accounts.usageLimitBypassModelsHint")}</p>
  </div>;
}
