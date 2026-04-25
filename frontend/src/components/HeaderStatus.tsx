import type { MusicAdvisor } from "../hooks/useMusicAdvisor";

interface HeaderStatusProps {
  advisor: MusicAdvisor;
}

// Sits in the brand row on wide viewports and drops onto its own
// full-width row on narrow ones — see .app-header media queries in
// styles.css. Layout / responsive bits live in CSS so they can be
// overridden by media queries (inline styles would always win).
export function HeaderStatus({ advisor }: HeaderStatusProps) {
  if (advisor.stage === "idle") return null;
  const isLoading = advisor.stage === "connecting" || advisor.stage === "loading";

  return (
    <div className="header-status" aria-live="polite">
      {isLoading ? (
        <>
          <span className="dot dot-pulse" aria-hidden />
          <span className="status-text">
            {advisor.step || "Working…"}
            {advisor.libraryCount > 0 ? ` · ${advisor.libraryCount}` : ""}
          </span>
        </>
      ) : (
        <>
          <span className="dot" aria-hidden />
          <span className="status-text">
            {advisor.libraryCount} saved · synced just now
          </span>
        </>
      )}
    </div>
  );
}
