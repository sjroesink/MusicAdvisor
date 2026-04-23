import { useCallback, useEffect, useRef, useState } from "react";
import { MA_DATA, type DiscoverItem, type NewRelease } from "../data";

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

// Stream-loading simulator — stands in for a real Spotify + MusicBrainz
// pipeline. Produces: library → analyze → new releases → discover.
// Cards stream into the feed as they "arrive", so the UI fills progressively.
export function useMusicAdvisor(): MusicAdvisor {
  const [stage, setStage] = useState<Stage>("idle");
  const [progress, setProgress] = useState(0);
  const [step, setStep] = useState("");
  const [newReleases, setNewReleases] = useState<NewRelease[]>([]);
  const [discover, setDiscover] = useState<DiscoverItem[]>([]);
  const [libraryCount, setLibraryCount] = useState(0);
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(() => new Set());
  const [ratings, setRatings] = useState<Record<string, Rating>>({});

  const timeoutsRef = useRef<ReturnType<typeof setTimeout>[]>([]);

  const clearTimers = useCallback(() => {
    timeoutsRef.current.forEach(clearTimeout);
    timeoutsRef.current = [];
  }, []);

  const at = useCallback((ms: number, fn: () => void) => {
    timeoutsRef.current.push(setTimeout(fn, ms));
  }, []);

  const start = useCallback(() => {
    clearTimers();
    setNewReleases([]);
    setDiscover([]);
    setLibraryCount(0);
    setStage("connecting");
    setProgress(0);
    setStep("Connecting to Spotify…");

    at(700, () => {
      setStage("loading");
      setStep("Reading your saved library…");
      setProgress(0.08);
    });

    const libSteps = [120, 260, 410, 537, 642, 728];
    libSteps.forEach((n, i) => {
      at(900 + i * 140, () => setLibraryCount(n));
    });

    at(1800, () => {
      setStep("Matching artists across MusicBrainz…");
      setProgress(0.25);
    });

    const releases = MA_DATA.newReleases;
    releases.forEach((r, i) => {
      at(2200 + i * 380, () => {
        setNewReleases((arr) => [...arr, r]);
        setProgress((p) => Math.min(p + 0.07, 0.65));
        if (i === 0) setStep("Found new releases from artists you follow…");
      });
    });

    at(2200 + releases.length * 380 + 300, () => {
      setStep("Looking for adjacent listens…");
      setProgress(0.72);
    });

    const disc = MA_DATA.discover;
    disc.forEach((d, i) => {
      at(2200 + releases.length * 380 + 600 + i * 320, () => {
        setDiscover((arr) => [...arr, d]);
        setProgress((p) => Math.min(p + 0.04, 0.98));
        if (i === 0) setStep("Threading recommendations…");
      });
    });

    at(2200 + releases.length * 380 + 600 + disc.length * 320 + 400, () => {
      setProgress(1);
      setStep("Done");
      setStage("ready");
    });
  }, [at, clearTimers]);

  const reset = useCallback(() => {
    clearTimers();
    setStage("idle");
    setProgress(0);
    setStep("");
    setNewReleases([]);
    setDiscover([]);
    setLibraryCount(0);
  }, [clearTimers]);

  useEffect(() => clearTimers, [clearTimers]);

  const onDismiss = useCallback((id: string) => {
    setDismissed((s) => {
      const n = new Set(s);
      n.add(id);
      return n;
    });
  }, []);

  const onRate = useCallback((id: string, value: Rating | null) => {
    setRatings((r) => {
      const n = { ...r };
      if (value == null) delete n[id];
      else n[id] = value;
      return n;
    });
  }, []);

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
