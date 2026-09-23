import { useTranslation } from "react-i18next";
import { SlidersHorizontal } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";

interface Props {
  nativeEnabled: boolean;
  bpsEnabled: boolean;
  supportsBPS: boolean;
  nativeModelCount: number;
  bpsModelCount: number;
  onNativeChange: (enabled: boolean) => void;
  onBPSChange: (enabled: boolean) => void;
  onConfigureModels: () => void;
}

export default function CodexRoutesCard(props: Props) {
  const { t } = useTranslation();
  const routes = [
    { name: "Codex", enabled: props.nativeEnabled, count: props.nativeModelCount, onChange: props.onNativeChange },
    ...(props.supportsBPS ? [{ name: "BPS", enabled: props.bpsEnabled, count: props.bpsModelCount, onChange: props.onBPSChange }] : []),
  ];
  return (
    <div className="rounded-xl border border-border/70 bg-card p-4.5 md:col-span-2">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="text-sm font-semibold">{t("accounts.routesTitle")}</div>
        <Button type="button" variant="outline" size="sm" onClick={props.onConfigureModels}>
          <SlidersHorizontal className="size-3.5" />
          {t("accounts.supportedModelsTitle")}
        </Button>
      </div>
      <div className="mt-3 grid gap-2 sm:grid-cols-2">
        {routes.map((route) => (
          <div key={route.name} className="flex items-center justify-between gap-4 rounded-lg border border-border/70 bg-muted/15 px-3.5 py-3">
            <div className="min-w-0">
              <div className="flex items-center gap-2 text-sm font-semibold">
                <span className={`size-1.5 rounded-full ${route.enabled ? "bg-emerald-500" : "bg-muted-foreground/40"}`} />
                {route.name}
              </div>
              <p className="mt-1 text-xs text-muted-foreground">
                {route.count ? t("accounts.routeModelCount", { count: route.count }) : t("accounts.routeModelsUnrestricted")}
              </p>
            </div>
            <Switch checked={route.enabled} onCheckedChange={route.onChange} aria-label={t("accounts.routeEnable", { route: route.name })} />
          </div>
        ))}
      </div>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("accounts.routesSummary")}</p>
    </div>
  );
}
