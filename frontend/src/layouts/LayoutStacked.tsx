import { AppHeader } from "../components/AppHeader";
import { DiscoverBlock } from "../sections/DiscoverBlock";
import { ReleasesBlock } from "../sections/ReleasesBlock";
import type { MusicAdvisor } from "../hooks/useMusicAdvisor";
import type { ThemeController } from "../hooks/useTheme";
import type { Density } from "../types";

interface LayoutStackedProps {
  advisor: MusicAdvisor;
  theme: ThemeController;
  density: Density;
}

export function LayoutStacked({ advisor, theme, density }: LayoutStackedProps) {
  return (
    <div style={{ maxWidth: 880, margin: "0 auto", padding: "0 32px 120px" }}>
      <AppHeader advisor={advisor} theme={theme} />
      <div
        style={{
          marginTop: 48,
          display: "flex",
          flexDirection: "column",
          gap: 72,
        }}
      >
        <ReleasesBlock advisor={advisor} density={density} />
        <DiscoverBlock advisor={advisor} density={density} />
      </div>
    </div>
  );
}
