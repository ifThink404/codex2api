import { Cable } from "lucide-react";
import { Switch } from "@/components/ui/switch";

export default function BPSTransportField({
  checked,
  onChange,
  disabled = false,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <div className="rounded-xl border border-border/70 bg-card p-4 space-y-2">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-2 text-sm font-semibold">
          <Cable className="size-4 text-primary" aria-hidden />
          BPS 通道
        </span>
        <Switch
          checked={checked}
          onCheckedChange={onChange}
          disabled={disabled}
          aria-label="BPS 通道"
        />
      </div>
      <p className="text-xs text-muted-foreground">
        仅此账户的 HTTP Responses / compact 请求使用 BPS；关闭后使用普通
        Codex。模型权限需要按通道单独检测。
      </p>
    </div>
  );
}
