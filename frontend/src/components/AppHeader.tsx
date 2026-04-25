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
  const isLoading =
    advisor.stage === "connecting" || advisor.stage === "loading";
  return (
    <header className="app-header">
      <div className="ah-brand">
        <div
          style={{
            width: 22,
            height: 22,
            borderRadius: "50%",
            border: "1.25px solid var(--ink)",
            position: "relative",
            flexShrink: 0,
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
      <HeaderStatus advisor={advisor} />
      {right}
      <div className="ah-toggle">
        <ThemeToggle mode={theme.mode} onCycle={theme.cycle} />
      </div>
      {isLoading && <div className="header-sweep" aria-hidden />}
    </header>
  );
}
