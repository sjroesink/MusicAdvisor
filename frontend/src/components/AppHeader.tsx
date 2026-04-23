import type { ReactNode } from "react";
import type { MusicAdvisor } from "../hooks/useMusicAdvisor";
import type { ThemeController } from "../hooks/useTheme";
import { HeaderStatus } from "./HeaderStatus";
import { ThemeToggle } from "./ThemeToggle";

interface AppHeaderProps {
  advisor: MusicAdvisor;
  theme: ThemeController;
  right?: ReactNode;
}

export function AppHeader({ advisor, theme, right }: AppHeaderProps) {
  return (
    <header
      style={{
        display: "flex",
        justifyContent: "space-between",
        alignItems: "center",
        padding: "22px 0 18px",
        borderBottom: "1px solid var(--rule-soft)",
      }}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 14 }}>
        <div
          style={{
            width: 22,
            height: 22,
            borderRadius: "50%",
            border: "1.25px solid var(--ink)",
            position: "relative",
          }}
          aria-hidden
        >
          <div
            style={{
              position: "absolute",
              inset: 4,
              borderRadius: "50%",
              background: "var(--ink)",
            }}
          />
        </div>
        <div className="display" style={{ fontSize: 17, lineHeight: 1 }}>
          Music advisor
        </div>
      </div>
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <HeaderStatus advisor={advisor} />
        {right}
        <ThemeToggle mode={theme.mode} onCycle={theme.cycle} />
      </div>
    </header>
  );
}
