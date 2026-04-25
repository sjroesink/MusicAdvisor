import type { AlbumType, DiscoverItem, NewRelease } from "../data";
import type { Rating } from "../hooks/useMusicAdvisor";
import type { CardKind, CardVariant, Density } from "../types";
import { Cover } from "./Cover";
import { Icon } from "./Icon";

type AlbumItem = NewRelease | DiscoverItem;

interface AlbumCardProps {
  item: AlbumItem;
  kind: CardKind;
  variant?: CardVariant;
  density?: Density;
  dismissed?: boolean;
  rating?: Rating | null;
  onDismiss: (id: string) => void;
  onRate: (id: string, value: Rating | null) => void;
  onFilterType?: (type: AlbumType) => void;
  activeTypes?: ReadonlySet<AlbumType>;
}

function dateLabel(item: AlbumItem, kind: CardKind) {
  if (kind === "release" && "date" in item) return item.date;
  return item.year;
}

// metaLine builds "N tracks · 42 min" but drops either half when the
// backend doesn't know it yet (new-release candidates lack track counts
// until the album is pulled into the library).
function metaLine(item: AlbumItem, form: "long" | "short") {
  const parts: string[] = [];
  if (item.tracks > 0) {
    if (form === "long") {
      parts.push(`${item.tracks} ${item.tracks === 1 ? "track" : "tracks"}`);
    } else {
      parts.push(`${item.tracks} ${item.tracks === 1 ? "trk" : "trks"}`);
    }
  }
  if (item.length) parts.push(item.length);
  return parts.join(" · ");
}

export function AlbumCard({
  item,
  kind,
  variant = "list",
  density = "roomy",
  dismissed = false,
  rating = null,
  onDismiss,
  onRate,
  onFilterType,
  activeTypes,
}: AlbumCardProps) {
  const compact = density === "compact";
  const typeActive = activeTypes?.has(item.type) ?? false;

  const TypeTag = () => {
    const clickable = Boolean(onFilterType);
    return (
      <button
        onClick={
          onFilterType
            ? (e) => {
                e.stopPropagation();
                onFilterType(item.type);
              }
            : undefined
        }
        disabled={!onFilterType}
        aria-pressed={typeActive}
        style={{
          fontFamily: "var(--mono)",
          fontSize: 9.5,
          letterSpacing: "0.14em",
          textTransform: "uppercase",
          color: typeActive ? "var(--paper)" : "var(--ink-faint)",
          background: typeActive ? "var(--ink)" : "transparent",
          border: `1px solid ${typeActive ? "var(--ink)" : "var(--rule)"}`,
          padding: "2px 7px",
          borderRadius: 999,
          cursor: clickable ? "pointer" : "default",
          transition: "background .15s, color .15s, border-color .15s",
        }}
        onMouseEnter={
          clickable
            ? (e) => {
                if (!typeActive) {
                  e.currentTarget.style.borderColor = "var(--ink-faint)";
                  e.currentTarget.style.color = "var(--ink-soft)";
                }
              }
            : undefined
        }
        onMouseLeave={
          clickable
            ? (e) => {
                if (!typeActive) {
                  e.currentTarget.style.borderColor = "var(--rule)";
                  e.currentTarget.style.color = "var(--ink-faint)";
                }
              }
            : undefined
        }
        title={clickable ? `Filter by ${item.type}` : undefined}
      >
        {item.type}
      </button>
    );
  };

  if (variant === "grid") {
    return (
      <div
        className="card-in"
        style={{
          display: "flex",
          flexDirection: "column",
          gap: compact ? 10 : 14,
          opacity: dismissed ? 0.35 : 1,
          transition: "opacity 0.2s",
        }}
      >
        <Cover label={item.cover} imageUrl={item.coverArtUrl} />
        <div>
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              alignItems: "baseline",
              gap: 8,
              marginBottom: 4,
            }}
          >
            <div
              style={{
                fontSize: 13.5,
                fontWeight: 500,
                color: "var(--ink)",
                lineHeight: 1.3,
              }}
            >
              {item.artist}
            </div>
            <div
              style={{
                fontFamily: "var(--mono)",
                fontSize: 10.5,
                color: "var(--ink-faint)",
                letterSpacing: "0.06em",
                whiteSpace: "nowrap",
              }}
            >
              {dateLabel(item, kind)}
            </div>
          </div>
          <div
            className="display"
            style={{
              fontSize: compact ? 17 : 20,
              lineHeight: 1.15,
              marginBottom: 8,
              color: "var(--ink)",
            }}
          >
            {item.title}
          </div>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 6,
              marginBottom: 10,
              flexWrap: "wrap",
            }}
          >
            <TypeTag />
            {metaLine(item, "long") && (
              <span
                style={{
                  fontSize: 11.5,
                  color: "var(--ink-faint)",
                  fontFamily: "var(--mono)",
                  letterSpacing: "0.04em",
                }}
              >
                {metaLine(item, "long")}
              </span>
            )}
          </div>
          {!compact && (
            <div
              style={{
                fontSize: 12.5,
                color: "var(--ink-soft)",
                lineHeight: 1.5,
                marginBottom: 14,
                fontStyle: "italic",
              }}
            >
              {item.reason}
            </div>
          )}
          <CardActions item={item} onDismiss={onDismiss} onRate={onRate} rating={rating} />
        </div>
      </div>
    );
  }

  return (
    <div
      className="card-in"
      style={{
        display: "grid",
        gridTemplateColumns: compact ? "64px 1fr auto" : "96px 1fr auto",
        gap: compact ? 16 : 22,
        alignItems: "start",
        padding: compact ? "14px 0" : "20px 0",
        borderTop: "1px solid var(--rule-soft)",
        opacity: dismissed ? 0.35 : 1,
        transition: "opacity 0.2s",
      }}
    >
      <Cover label={item.cover} imageUrl={item.coverArtUrl} />
      <div style={{ minWidth: 0 }}>
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: 10,
            marginBottom: 4,
            flexWrap: "wrap",
          }}
        >
          <div style={{ fontSize: 13.5, fontWeight: 500, color: "var(--ink)" }}>
            {item.artist}
          </div>
          <TypeTag />
          {metaLine(item, "short") && (
            <span className="eyebrow" style={{ fontSize: 9.5 }}>
              {metaLine(item, "short")}
            </span>
          )}
        </div>
        <div
          className="display"
          style={{
            fontSize: compact ? 19 : 24,
            marginBottom: compact ? 6 : 10,
            color: "var(--ink)",
          }}
        >
          {item.title}
        </div>
        {!compact && (
          <div
            style={{
              fontSize: 13,
              color: "var(--ink-soft)",
              lineHeight: 1.5,
              maxWidth: 520,
              fontStyle: "italic",
            }}
          >
            {item.reason}
          </div>
        )}
      </div>
      <div
        style={{
          display: "flex",
          flexDirection: "column",
          alignItems: "flex-end",
          gap: 8,
        }}
      >
        <div
          style={{
            fontFamily: "var(--mono)",
            fontSize: 10.5,
            color: "var(--ink-faint)",
            letterSpacing: "0.08em",
          }}
        >
          {dateLabel(item, kind)}
        </div>
        <CardActions item={item} onDismiss={onDismiss} onRate={onRate} rating={rating} />
      </div>
    </div>
  );
}

interface CardActionsProps {
  item: AlbumItem;
  rating: Rating | null;
  onDismiss: (id: string) => void;
  onRate: (id: string, value: Rating | null) => void;
}

// spotifyOpenURL prefers a deep link to the exact album when the backend
// has its Spotify ID cached (i.e. the album was ingested through library /
// top-lists / recently-played at some point). Items discovered purely via
// MB or Last.fm have no Spotify ID, so we fall back to Spotify's search
// page pre-populated with "artist title" — one click closer than the
// landing page, but not a true deep link.
function spotifyOpenURL(item: AlbumItem) {
  if (item.spotifyId) return `https://open.spotify.com/album/${item.spotifyId}`;
  const q = encodeURIComponent(`${item.artist} ${item.title}`.trim());
  return `https://open.spotify.com/search/${q}`;
}

function CardActions({ item, rating, onDismiss, onRate }: CardActionsProps) {
  const heard = rating != null;
  const openURL = spotifyOpenURL(item);
  const directLink = Boolean(item.spotifyId);

  return (
    <div style={{ display: "flex", gap: 4, alignItems: "center" }}>
      <a
        className="btn btn-tiny btn-ghost"
        style={{ gap: 5, textDecoration: "none" }}
        href={openURL}
        target="_blank"
        rel="noopener noreferrer"
        title={directLink ? "Open album in Spotify" : "Search in Spotify"}
      >
        <Icon name="ext" size={12} /> {directLink ? "Open" : "Search"}
      </a>

      {!heard && (
        <button
          className="btn btn-tiny btn-ghost"
          onClick={() => onRate(item.id, "pending")}
          style={{ gap: 5 }}
          title="Mark as heard"
        >
          <Icon name="check" size={12} /> Heard
        </button>
      )}

      {rating === "pending" && (
        <div
          className="card-in"
          role="group"
          aria-label="Rate this release"
          style={{
            display: "flex",
            alignItems: "center",
            gap: 2,
            padding: "0 2px 0 8px",
            border: "1px solid var(--accent)",
            borderRadius: 999,
            height: 28,
          }}
        >
          <span
            style={{
              fontSize: 11,
              color: "var(--ink-soft)",
              marginRight: 4,
              fontStyle: "italic",
            }}
          >
            How was it?
          </span>
          <button
            onClick={() => onRate(item.id, "good")}
            title="Good release"
            aria-label="Good release"
            style={{
              width: 24,
              height: 24,
              borderRadius: 999,
              display: "grid",
              placeItems: "center",
              color: "var(--ink-soft)",
            }}
            onMouseEnter={(e) => {
              e.currentTarget.style.color = "var(--accent-ink)";
              e.currentTarget.style.background = "var(--paper)";
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.color = "var(--ink-soft)";
              e.currentTarget.style.background = "transparent";
            }}
          >
            <Icon name="thumb-up" size={13} />
          </button>
          <button
            onClick={() => onRate(item.id, "bad")}
            title="Not for me"
            aria-label="Not for me"
            style={{
              width: 24,
              height: 24,
              borderRadius: 999,
              display: "grid",
              placeItems: "center",
              color: "var(--ink-soft)",
            }}
            onMouseEnter={(e) => {
              e.currentTarget.style.color = "var(--danger)";
              e.currentTarget.style.background = "var(--paper)";
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.color = "var(--ink-soft)";
              e.currentTarget.style.background = "transparent";
            }}
          >
            <Icon name="thumb-down" size={13} />
          </button>
        </div>
      )}

      {(rating === "good" || rating === "bad") && (
        <button
          className="btn btn-tiny btn-ghost"
          onClick={() => onRate(item.id, null)}
          style={{
            gap: 5,
            color: rating === "good" ? "var(--accent-ink)" : "var(--danger)",
            borderColor: rating === "good" ? "var(--accent)" : "var(--danger)",
          }}
          title={`Rated ${rating === "good" ? "good" : "not for me"} — click to clear`}
        >
          <Icon name={rating === "good" ? "thumb-up" : "thumb-down"} size={12} />
          {rating === "good" ? "Good" : "Not for me"}
        </button>
      )}

      <button
        className="btn btn-tiny btn-ghost"
        onClick={() => onDismiss(item.id)}
        style={{ padding: "0 8px" }}
        title="Dismiss"
        aria-label="Dismiss"
      >
        <Icon name="x" size={12} />
      </button>
    </div>
  );
}
