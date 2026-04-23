import { Icon } from "./Icon";
import type { ThemeMode } from "../hooks/useTheme";

interface ThemeToggleProps {
  mode: ThemeMode;
  onCycle: () => void;
}

export function ThemeToggle({ mode, onCycle }: ThemeToggleProps) {
  const label = mode === "auto" ? "Auto" : mode === "light" ? "Light" : "Dark";
  const iconName = mode === "auto" ? "auto" : mode === "light" ? "sun" : "moon";
  return (
    <button
      onClick={onCycle}
      className="btn btn-tiny btn-ghost"
      style={{ gap: 6, fontSize: 11.5, letterSpacing: "0.04em" }}
      title={`Theme: ${label} (click to cycle)`}
      aria-label={`Theme: ${label}. Click to cycle through Auto, Light, Dark.`}
    >
      <Icon name={iconName} size={13} />
      {label}
    </button>
  );
}
