import { useCallback, useEffect, useState } from "react";

export type ThemeMode = "auto" | "light" | "dark";

const STORAGE_KEY = "ma:theme";

function readStored(): ThemeMode {
  if (typeof window === "undefined") return "auto";
  const raw = window.localStorage.getItem(STORAGE_KEY);
  return raw === "light" || raw === "dark" || raw === "auto" ? raw : "auto";
}

export interface ThemeController {
  mode: ThemeMode;
  cycle: () => void;
}

export function useTheme(): ThemeController {
  const [mode, setMode] = useState<ThemeMode>(() => readStored());

  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const apply = () => {
      const actual = mode === "auto" ? (mq.matches ? "dark" : "light") : mode;
      document.documentElement.setAttribute("data-theme", actual);
    };
    apply();
    if (mode === "auto") {
      mq.addEventListener("change", apply);
      return () => mq.removeEventListener("change", apply);
    }
  }, [mode]);

  useEffect(() => {
    window.localStorage.setItem(STORAGE_KEY, mode);
  }, [mode]);

  const cycle = useCallback(() => {
    setMode((m) => (m === "auto" ? "light" : m === "light" ? "dark" : "auto"));
  }, []);

  return { mode, cycle };
}
