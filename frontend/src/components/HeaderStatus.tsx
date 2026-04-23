import type { MusicAdvisor } from "../hooks/useMusicAdvisor";

interface HeaderStatusProps {
  advisor: MusicAdvisor;
}

// Single slot that morphs between loading and ready states.
// Fixed min-width keeps the header from shifting when status changes.
export function HeaderStatus({ advisor }: HeaderStatusProps) {
  if (advisor.stage === "idle") return null;
  const isLoading = advisor.stage === "connecting" || advisor.stage === "loading";

  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 8,
        marginRight: 6,
        minWidth: 260,
        justifyContent: "flex-end",
      }}
      aria-live="polite"
    >
      {isLoading ? (
        <>
          <span className="dot dot-pulse" aria-hidden />
          <span
            style={{
              fontSize: 11.5,
              fontFamily: "var(--mono)",
              color: "var(--ink-faint)",
              letterSpacing: "0.06em",
              whiteSpace: "nowrap",
              overflow: "hidden",
              textOverflow: "ellipsis",
              maxWidth: 240,
            }}
          >
            {advisor.step || "Working…"}
            {advisor.libraryCount > 0 ? ` · ${advisor.libraryCount}` : ""}
          </span>
        </>
      ) : (
        <>
          <span className="dot" aria-hidden />
          <span
            style={{
              fontSize: 11.5,
              fontFamily: "var(--mono)",
              color: "var(--ink-faint)",
              letterSpacing: "0.06em",
              whiteSpace: "nowrap",
            }}
          >
            {advisor.libraryCount} saved · synced just now
          </span>
        </>
      )}
    </div>
  );
}
