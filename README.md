# Music Advisor

Self-hosted music recommendation webapp. Connects to a user's Spotify, pulls
library + top lists + recently-played signals, finds new releases via
MusicBrainz, and surfaces similar-artist discoveries via ListenBrainz — all
feed rows carry explicit provenance so you can see *why* something showed up.

One Go binary + one SQLite file + one Docker image. No external queue, no
hosted DB.

## Running locally

```bash
# Backend (SQLite at ./data/music-advisor.db)
cd backend
cp ../.env.example ../.env.local
# edit MA_SESSION_SECRET, MA_SECRET_KEY, MA_SPOTIFY_CLIENT_ID / SECRET,
# MA_USER_AGENT_CONTACT in .env.local — see .env.example comments
go run ./cmd/server

# Frontend (Vite dev server, proxies /api to :8080)
cd frontend
npm install
npm run dev
# → http://127.0.0.1:5173
```

Spotify requires `http://127.0.0.1` loopback for OAuth; set your redirect
URI in the Spotify dashboard to `http://127.0.0.1:5173/api/auth/spotify/callback`.

## Running in Docker

```bash
# Generate two 32-byte secrets (hex-encoded) once per environment.
openssl rand -hex 32   # MA_SESSION_SECRET
openssl rand -hex 32   # MA_SECRET_KEY

export MA_SESSION_SECRET=...
export MA_SECRET_KEY=...
export MA_SPOTIFY_CLIENT_ID=...
export MA_SPOTIFY_CLIENT_SECRET=...
export MA_USER_AGENT_CONTACT=you@example.com
export MA_BASE_URL=https://music.example.com  # whatever the public URL is

docker compose up -d --build
```

The container bakes `frontend/dist` at `/app/frontend/dist` and serves it
at `/`. SQLite lives under `/data` (named volume `ma-data`).

## Unraid / Traefik

`docker-compose.yml` has a commented-out `labels:` block with a Traefik
template. Uncomment, change the hostname, and make sure `MA_BASE_URL`
matches the public URL the Spotify app's redirect URI is registered
against. For Unraid specifically, add the container to the same Docker
network Traefik uses (`proxy` in the example).

## Maintenance

- `./server --healthcheck` — internal HTTP probe, also wired into the
  Docker healthcheck. Exit 0 when `GET /healthz` succeeds.
- `./server --rebuild-affinity <user-id>` — one-shot: recomputes all
  affinity rows for one user from the raw signals log. Useful after a
  signal-schema change.

## Layout

```
backend/
  cmd/server/          entry point + CLI flags
  internal/
    auth/              sessions, OAuth cookies, crypto
    config/            env loader (+ .env / .env.local fallback)
    db/                sqlite open + embedded migrations
    http/
      handlers/        HTTP handlers
      router.go        chi routing (short-lived group + SSE group + static)
    providers/         external API clients (spotify, musicbrainz, listenbrainz)
    services/
      library/         saved albums + followed artists sync
      toplists/        top-{artists,tracks} × 3 ranges
      listening/       recently-played + skip detection
      releases/        MusicBrainz new releases scan
      lbsimilar/       ListenBrainz similar-artists → discover_candidates
      signal/          event log + affinity propagation + side tables
      user/            multi-user + external-account storage
    sse/               in-memory pub/sub hub for /api/feed/stream

frontend/              Vite + React 18 + TypeScript
```

## Sync pipeline

`POST /api/sync/trigger` runs these phases sequentially in a detached
goroutine per user. Each phase publishes an SSE `phase` event on
start/done so `/api/feed/stream` clients can progressive-update:

```
library  →  toplists  →  listening  →  releases  →  lb-similar  →  (done)
```

Each phase has its own rate-limit gate (12h, 6h, 30min, etc.) so
re-triggering during development doesn't pollute the signals log or
inflate affinity scores.
