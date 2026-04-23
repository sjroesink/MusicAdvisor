import { useEffect } from "react";
import { ConnectScreen } from "./components/ConnectScreen";
import { ThemeToggle } from "./components/ThemeToggle";
import { useAuth } from "./hooks/useAuth";
import { useMusicAdvisor } from "./hooks/useMusicAdvisor";
import { useTheme } from "./hooks/useTheme";
import { LayoutStacked } from "./layouts/LayoutStacked";

export function App() {
  const { auth, login } = useAuth();
  const theme = useTheme();
  const advisor = useMusicAdvisor();

  // Once authenticated, fire the (still-mocked) feed loader so the UI
  // keeps its streaming behavior while phases 3+ replace the simulator
  // with real backend data.
  useEffect(() => {
    if (auth.state === "authenticated" && advisor.stage === "idle") {
      advisor.start();
    }
  }, [auth.state, advisor]);

  if (auth.state === "loading") {
    return (
      <div
        style={{
          minHeight: "100vh",
          display: "grid",
          placeItems: "center",
          color: "var(--ink-faint)",
          fontFamily: "var(--mono)",
          fontSize: 11.5,
          letterSpacing: "0.12em",
          textTransform: "uppercase",
        }}
      >
        Loading…
      </div>
    );
  }

  if (auth.state === "error") {
    return (
      <div
        style={{
          minHeight: "100vh",
          display: "grid",
          placeItems: "center",
          padding: "0 24px",
          textAlign: "center",
          color: "var(--ink-soft)",
        }}
      >
        <div style={{ maxWidth: 420 }}>
          <div className="eyebrow" style={{ marginBottom: 12 }}>
            Something went wrong
          </div>
          <p style={{ fontSize: 14, lineHeight: 1.55 }}>{auth.error}</p>
        </div>
      </div>
    );
  }

  if (auth.state === "unauthenticated") {
    return (
      <>
        <div style={{ position: "absolute", top: 20, right: 24, zIndex: 10 }}>
          <ThemeToggle mode={theme.mode} onCycle={theme.cycle} />
        </div>
        <ConnectScreen onConnect={login} />
      </>
    );
  }

  return <LayoutStacked advisor={advisor} theme={theme} density="roomy" />;
}
