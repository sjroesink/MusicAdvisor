import { useCallback, useEffect, useRef, useState } from "react";
import type { DiscoverItem, NewRelease } from "../data";

export type Stage = "idle" | "connecting" | "loading" | "ready";
export type Rating = "pending" | "good" | "bad";

export interface MusicAdvisor {
  stage: Stage;
  progress: number;
  step: string;
  libraryCount: number;
  newReleases: NewRelease[];
  discover: DiscoverItem[];
  dismissed: ReadonlySet<string>;
  ratings: Readonly<Record<string, Rating>>;
  start: () => void;
  reset: () => void;
  onDismiss: (id: string) => void;
  onRate: (id: string, value: Rating | null) => void;
}

// FeedCard mirrors handlers.FeedCard on the backend. The two types exist
// independently because the frontend uses simpler NewRelease / DiscoverItem
// shapes everywhere else — we translate at the boundary below.
interface FeedCard {
  id: string;
  subject_type: string;
  artist: string;
  title: string;
  year?: number;
  date?: string;
  type: "Album" | "EP" | "Single";
  tracks?: number;
  length?: string;
  reason: string;
  cover?: string;
  score: number;
  source: string;
}

interface FeedHeader {
  library_count: number;
  last_sync_at?: string;
  status: "idle" | "syncing" | "ready" | "error";
}

interface FeedResponse {
  header: FeedHeader;
  new_releases: FeedCard[];
  discover: FeedCard[];
  ratings: { subject_type: string; subject_id: string; rating: string }[];
  hides: { subject_type: string; subject_id: string }[];
}

function mapNewRelease(c: FeedCard): NewRelease {
  return {
    id: c.id,
    artist: c.artist || "—",
    title: c.title || "Untitled",
    year: c.year ?? new Date().getFullYear(),
    date: c.date ?? "",
    type: c.type,
    tracks: c.tracks ?? 0,
    length: c.length ?? "",
    reason: c.reason,
    cover: c.cover ?? "",
  };
}

function mapDiscover(c: FeedCard): DiscoverItem {
  return {
    id: c.id,
    artist: c.artist || "—",
    title: c.title || "Untitled",
    year: c.year ?? new Date().getFullYear(),
    type: c.type,
    tracks: c.tracks ?? 0,
    length: c.length ?? "",
    reason: c.reason,
    cover: c.cover ?? "",
  };
}

// subjectTypeFor guesses subject_type from the feed source. Every current
// source surfaces album-level cards; if that changes, pass it through the
// FeedCard shape.
function subjectTypeFor(c: FeedCard): "artist" | "album" | "track" {
  switch (c.subject_type) {
    case "artist":
    case "album":
    case "track":
      return c.subject_type;
    default:
      return "album";
  }
}

// Map of id → subject_type, built from the latest feed payload. onRate /
// onDismiss need it to construct the signal POST body; the card itself
// doesn't carry subject_type down the component tree.
type SubjectIndex = Record<string, "artist" | "album" | "track">;

export interface UseMusicAdvisorOptions {
  // Whether the hook should actually hit the backend. Pass false before
  // the user has signed in; the hook stays in "idle" with empty state and
  // skips both the initial GET /api/feed and opening an EventSource.
  enabled: boolean;
}

export function useMusicAdvisor(options: UseMusicAdvisorOptions = { enabled: true }): MusicAdvisor {
  const [stage, setStage] = useState<Stage>("idle");
  const [progress, setProgress] = useState(0);
  const [step, setStep] = useState("");
  const [libraryCount, setLibraryCount] = useState(0);
  const [newReleases, setNewReleases] = useState<NewRelease[]>([]);
  const [discover, setDiscover] = useState<DiscoverItem[]>([]);
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(() => new Set());
  const [ratings, setRatings] = useState<Record<string, Rating>>({});
  const subjectRef = useRef<SubjectIndex>({});
  const esRef = useRef<EventSource | null>(null);

  const applyFeed = useCallback((f: FeedResponse) => {
    setLibraryCount(f.header.library_count);
    setNewReleases(f.new_releases.map(mapNewRelease));
    setDiscover(f.discover.map(mapDiscover));
    const idx: SubjectIndex = {};
    for (const c of f.new_releases) idx[c.id] = subjectTypeFor(c);
    for (const c of f.discover) idx[c.id] = subjectTypeFor(c);
    subjectRef.current = idx;

    const next: Record<string, Rating> = {};
    for (const r of f.ratings) {
      if (r.rating === "good") next[r.subject_id] = "good";
      else if (r.rating === "bad") next[r.subject_id] = "bad";
    }
    setRatings(next);
    setDismissed(new Set(f.hides.map((h) => h.subject_id)));

    switch (f.header.status) {
      case "syncing":
        setStage("loading");
        setStep("Syncing your library…");
        break;
      case "error":
        setStage("ready");
        setStep("Last sync failed — we’re showing what we have.");
        break;
      case "ready":
        setStage("ready");
        setStep("Done");
        setProgress(1);
        break;
      default:
        setStage("idle");
        setStep("");
    }
  }, []);

  const refetch = useCallback(async () => {
    try {
      const r = await fetch("/api/feed", { credentials: "include" });
      if (!r.ok) return;
      const data: FeedResponse = await r.json();
      applyFeed(data);
    } catch {
      /* network blips are fine — next SSE event retries */
    }
  }, [applyFeed]);

  // On mount (and when `enabled` flips true): one initial feed load, then
  // open the SSE stream and refetch on each phase event. If disabled,
  // the hook returns empty state and makes no network calls — useful
  // before the user has authenticated.
  useEffect(() => {
    if (!options.enabled) return;
    void refetch();
    const es = new EventSource("/api/feed/stream", { withCredentials: true });
    esRef.current = es;
    const bump = () => void refetch();
    es.addEventListener("phase", bump);
    es.addEventListener("update", bump);
    es.addEventListener("ready", () => {
      /* handshake only, do nothing */
    });
    es.onerror = () => {
      // Browser auto-reconnects; we just note the state.
    };
    return () => {
      es.close();
      esRef.current = null;
    };
  }, [refetch, options.enabled]);

  const start = useCallback(async () => {
    setStage("connecting");
    setStep("Queueing sync…");
    setProgress(0.05);
    try {
      const r = await fetch("/api/sync/trigger", {
        method: "POST",
        credentials: "include",
      });
      if (r.ok || r.status === 202) {
        setStage("loading");
        setStep("Sync queued — watching for updates…");
        setProgress(0.1);
      } else {
        setStage("ready");
        setStep("Sync not available right now.");
      }
    } catch {
      setStage("ready");
      setStep("Could not reach server.");
    }
  }, []);

  const reset = useCallback(() => {
    setStage("idle");
    setProgress(0);
    setStep("");
  }, []);

  const postSignal = useCallback(
    async (kind: string, id: string) => {
      const subjectType = subjectRef.current[id] ?? "album";
      try {
        await fetch("/api/signals", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            kind,
            subject_type: subjectType,
            subject_id: id,
          }),
        });
      } catch {
        /* optimistic UI keeps state even if POST fails */
      }
    },
    []
  );

  const onDismiss = useCallback(
    (id: string) => {
      setDismissed((s) => {
        const n = new Set(s);
        n.add(id);
        return n;
      });
      void postSignal("dismiss", id);
    },
    [postSignal]
  );

  const onRate = useCallback(
    (id: string, value: Rating | null) => {
      setRatings((r) => {
        const n = { ...r };
        if (value == null) delete n[id];
        else n[id] = value;
        return n;
      });
      if (value === "good") void postSignal("heard_good", id);
      if (value === "bad") void postSignal("heard_bad", id);
      // A null (toggle-off) is local-only; there's no "un-rate" signal on
      // the backend. The rating row lingers until overwritten.
    },
    [postSignal]
  );

  return {
    stage,
    progress,
    step,
    libraryCount,
    newReleases,
    discover,
    dismissed,
    ratings,
    start,
    reset,
    onDismiss,
    onRate,
  };
}
