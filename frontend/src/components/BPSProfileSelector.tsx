import { Switch } from "@/components/ui/switch";
import type { CodexBPSProfile } from "../types";

const profiles: { value: CodexBPSProfile; label: string }[] = [
  { value: "word", label: "Word" },
  { value: "excel", label: "Excel" },
  { value: "sheets", label: "Sheets" },
  { value: "powerpoint", label: "PowerPoint" },
];

export default function BPSProfileSelector({ value, onChange, disabled = false }: {
  value: CodexBPSProfile;
  onChange: (value: CodexBPSProfile) => void;
  disabled?: boolean;
}) {
  return (
    <div className="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-2" role="group" aria-label="BPS 类型（单选）">
      {profiles.map((profile) => (
        <label key={profile.value} className={`flex items-center justify-between gap-2 rounded-lg border px-3 py-2.5 text-sm ${value === profile.value ? "border-primary/40 bg-primary/5" : "border-border/70 bg-muted/15"} ${disabled ? "opacity-50" : "cursor-pointer"}`}>
          <span>{profile.label}</span>
          <Switch checked={value === profile.value} disabled={disabled} onCheckedChange={(checked) => { if (checked) onChange(profile.value); }} aria-label={`BPS ${profile.label}`} />
        </label>
      ))}
    </div>
  );
}
