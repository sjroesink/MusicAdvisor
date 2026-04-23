// Typed HTTP client for the Music Advisor backend. Cookies ride the
// same-origin flow so we can rely on the ma_session cookie without
// managing tokens in the frontend.

export interface Integration {
  connected: boolean;
  needs_reconnect?: boolean;
  external_id?: string;
}

export interface MeSpotify {
  display_name?: string;
  image_url?: string;
  connected: boolean;
  needs_reconnect?: boolean;
}

export interface MeResponse {
  user_id: string;
  spotify?: MeSpotify;
  integrations: Record<string, Integration>;
}

export interface ApiError {
  error: string;
  message: string;
}

export class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
    this.name = "UnauthorizedError";
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    credentials: "include",
    headers: {
      Accept: "application/json",
      ...(init.headers ?? {}),
    },
    ...init,
  });
  if (res.status === 401) throw new UnauthorizedError();
  if (!res.ok) {
    const text = await res.text();
    let detail: string | undefined;
    try {
      const parsed = JSON.parse(text) as Partial<ApiError>;
      detail = parsed.message ?? parsed.error;
    } catch {
      detail = text || `${res.status} ${res.statusText}`;
    }
    throw new Error(detail ?? `HTTP ${res.status}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  me(): Promise<MeResponse> {
    return request<MeResponse>("/api/me");
  },
  logout(): Promise<void> {
    return request<void>("/api/auth/logout", { method: "POST" });
  },
  loginUrl(): string {
    return "/api/auth/spotify/login";
  },
};
