import { ConnectScreen } from "./components/ConnectScreen";
import { ThemeToggle } from "./components/ThemeToggle";
import { useMusicAdvisor } from "./hooks/useMusicAdvisor";
import { useTheme } from "./hooks/useTheme";
import { LayoutStacked } from "./layouts/LayoutStacked";

export function App() {
  const advisor = useMusicAdvisor();
  const theme = useTheme();

  if (advisor.stage === "idle") {
    return (
      <>
        <div style={{ position: "absolute", top: 20, right: 24, zIndex: 10 }}>
          <ThemeToggle mode={theme.mode} onCycle={theme.cycle} />
        </div>
        <ConnectScreen onConnect={advisor.start} />
      </>
    );
  }

  return <LayoutStacked advisor={advisor} theme={theme} density="roomy" />;
}
