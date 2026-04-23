import { Icon } from "./Icon";

interface ConnectScreenProps {
  onConnect: () => void;
}

export function ConnectScreen({ onConnect }: ConnectScreenProps) {
  return (
    <div
      style={{
        minHeight: "100vh",
        display: "grid",
        placeItems: "center",
        padding: "40px 24px",
      }}
    >
      <div style={{ maxWidth: 440, textAlign: "center" }}>
        <div className="eyebrow" style={{ marginBottom: 28 }}>
          Music advisor
        </div>
        <h1 className="display" style={{ fontSize: 44, margin: "0 0 20px" }}>
          Quiet recommendations,
          <br />
          rooted in what you already love.
        </h1>
        <p
          style={{
            color: "var(--ink-soft)",
            fontSize: 15.5,
            lineHeight: 1.55,
            margin: "0 0 36px",
            maxWidth: 380,
            marginInline: "auto",
          }}
        >
          Link your Spotify once. We'll look at the artists and albums you've
          saved, and tell you what's new — and what's worth finding next.
        </p>
        <button
          className="btn btn-primary"
          onClick={onConnect}
          style={{ height: 46, padding: "0 24px" }}
        >
          Connect Spotify
          <Icon name="arrow" size={15} />
        </button>
        <div
          style={{
            marginTop: 28,
            fontFamily: "var(--mono)",
            fontSize: 10.5,
            letterSpacing: "0.12em",
            textTransform: "uppercase",
            color: "var(--ink-faint)",
          }}
        >
          Read-only access · revoke anytime
        </div>
      </div>
    </div>
  );
}
