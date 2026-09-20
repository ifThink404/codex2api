import { useId } from "react";
import { useTranslation } from "react-i18next";
import type { AccountProbeMode } from "../types";
import type { AccountProbePolicy } from "../lib/accountProbePolicy";
import { Select } from "@/components/ui/select";
import { DraftNumberInput } from "@/components/ui/draft-number-input";
import { Switch } from "@/components/ui/switch";

interface AccountProbePolicyFieldsProps {
  value: AccountProbePolicy;
  onChange: (value: AccountProbePolicy) => void;
  disabled?: boolean;
  apiAccount?: boolean;
}

export default function AccountProbePolicyFields({
  value,
  onChange,
  disabled = false,
  apiAccount = false,
}: AccountProbePolicyFieldsProps) {
  const { t } = useTranslation();
  const id = useId();
  const modeHints: Record<AccountProbeMode, string> = {
    auto: t("accounts.probeAutoHint"),
    off: t("accounts.probeOffHint"),
    on: t("accounts.probeOnHint"),
  };

  return (
    <div className="space-y-3 rounded-lg border border-border/60 p-3">
      <div className="text-xs font-semibold text-foreground">
        {t("accounts.probePolicyTitle")}
      </div>
      <div className="space-y-1.5">
        <label htmlFor={`${id}-mode`} className="block text-xs font-semibold text-muted-foreground">
          {t("accounts.probeMode")}
        </label>
        <Select
          id={`${id}-mode`}
          value={value.probe_mode}
          onValueChange={(mode) => onChange({ ...value, probe_mode: mode as AccountProbeMode })}
          disabled={disabled}
          options={[
            { value: "auto", label: t("accounts.probeModeAuto") },
            { value: "off", label: t("accounts.probeModeOff") },
            { value: "on", label: t("accounts.probeModeOn") },
          ]}
        />
        <p className="text-xs leading-relaxed text-muted-foreground">{modeHints[value.probe_mode]}</p>
      </div>
      <div className="space-y-1.5">
        <label htmlFor={`${id}-interval`} className="block text-xs font-semibold text-muted-foreground">
          {t("accounts.probeInterval")}
        </label>
        <DraftNumberInput
          id={`${id}-interval`}
          value={value.probe_interval_minutes}
          onValueChange={(minutes) => onChange({ ...value, probe_interval_minutes: minutes })}
          min={0}
          max={1440}
          step={1}
          integer
          emptyValue={0}
          disabled={disabled || value.probe_mode === "off"}
          aria-describedby={`${id}-interval-hint`}
        />
        <p id={`${id}-interval-hint`} className="text-xs leading-relaxed text-muted-foreground">
          {t("accounts.probeIntervalHint")}
        </p>
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">{t("accounts.probeHealthHint")}</p>
      {apiAccount && (
        <div className="flex items-start justify-between gap-3 border-t border-border/60 pt-3">
          <div className="space-y-1.5">
            <label htmlFor={`${id}-recovery`} className="text-xs font-semibold text-foreground">
              {t("accounts.apiAutoRecovery")}
            </label>
            <p id={`${id}-recovery-hint`} className="text-xs leading-relaxed text-muted-foreground">
              {t("accounts.apiAutoRecoveryHint")}
            </p>
          </div>
          <Switch
            id={`${id}-recovery`}
            checked={value.api_auto_recovery_enabled}
            onCheckedChange={(checked) => onChange({ ...value, api_auto_recovery_enabled: checked })}
            disabled={disabled}
            aria-describedby={`${id}-recovery-hint`}
          />
        </div>
      )}
    </div>
  );
}
