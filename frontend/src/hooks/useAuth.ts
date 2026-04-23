import { useCallback, useEffect, useState } from "react";
import { api, UnauthorizedError, type MeResponse } from "../api";

export type AuthState =
  | { state: "loading" }
  | { state: "unauthenticated" }
  | { state: "authenticated"; me: MeResponse }
  | { state: "error"; error: string };

export interface AuthController {
  auth: AuthState;
  login: () => void;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

export function useAuth(): AuthController {
  const [auth, setAuth] = useState<AuthState>({ state: "loading" });

  const refresh = useCallback(async () => {
    try {
      const me = await api.me();
      setAuth({ state: "authenticated", me });
    } catch (err) {
      if (err instanceof UnauthorizedError) {
        setAuth({ state: "unauthenticated" });
        return;
      }
      setAuth({
        state: "error",
        error: err instanceof Error ? err.message : String(err),
      });
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const login = useCallback(() => {
    window.location.href = api.loginUrl();
  }, []);

  const logout = useCallback(async () => {
    await api.logout();
    setAuth({ state: "unauthenticated" });
  }, []);

  return { auth, login, logout, refresh };
}
