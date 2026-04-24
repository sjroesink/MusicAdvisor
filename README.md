# Music Advisor

Self-hosted music recommendation webapp. Connects to a user's Spotify, pulls
library + top lists + recently-played signals, finds new releases via
MusicBrainz, and surfaces similar-artist discoveries via ListenBrainz +
Last.fm + MusicBrainz artist-rels + same-label catalogs — every feed row
carries explicit provenance so you can see *why* something showed up.

One Go binary + one Postgres (with pgvector) + one Docker image. The
pgvector column is reserved on `artists` for a future content-based
recommender.

## Running locally

You need a Postgres 16 with the `pgvector` extension. Easiest is the
docker-compose stack:

```bash
# Start Postgres only (the Go binary will run from your host).
docker compose up -d postgres

# Backend (Postgres at localhost:5432 — the compose service publishes
# 5432 only when you uncomment the ports stanza in docker-compose.yml).
cd backend
cp ../.env.example ../.env.local
# Edit MA_SESSION_SECRET, MA_SECRET_KEY, MA_SPOTIFY_CLIENT_ID / SECRET,
# MA_USER_AGENT_CONTACT and (optional) MA_LASTFM_API_KEY in .env.local.
# MA_DATABASE_URL defaults to postgres://musicadvisor:musicadvisor@localhost:5432/musicadvisor.
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
# Optional: pin a strong Postgres password
export MA_POSTGRES_PASSWORD=$(openssl rand -hex 16)

docker compose up -d --build
```

The Postgres data lives in the named volume `ma-pgdata`. The app container
bakes `frontend/dist` at `/app/frontend/dist` and serves it at `/`.

## Tests

```bash
cd backend
# go test ./... in parallel can race on the testcontainers Docker provider
# detection on Windows; -p 1 serializes the package binaries and is
# reliably green. Each package boots its own ephemeral pgvector container
# (~2s startup) and discards it on exit.
go test -p 1 ./...
```

Setting `MA_TEST_DATABASE_URL=postgres://...` skips the testcontainers
spin entirely and runs every test against the supplied Postgres (each
test still gets its own freshly created database).

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
    db/                pgx open + embedded migrations
    http/
      handlers/        HTTP handlers
      router.go        chi routing (short-lived group + SSE group + static)
    providers/         external API clients (spotify, musicbrainz,
                       listenbrainz, lastfm)
    scheduler/         in-process per-phase cron
    services/
      library/         saved albums + followed artists sync
      toplists/        top-{artists,tracks} × 3 ranges
      listening/       recently-played + skip detection
      releases/        MusicBrainz new releases scan
      lbsimilar/       ListenBrainz similar-artists → discover_candidates
      lfsimilar/       Last.fm similar-artists → discover_candidates
      mbrels/          MusicBrainz artist-rels → discover_candidates
      samelabel/       MusicBrainz same-label → discover_candidates
      discover/        candidate TTL + source-name constants
      signal/          event log + affinity propagation + side tables
      user/            multi-user + external-account storage
    sse/               in-memory pub/sub hub for /api/feed/stream
    testutil/          pgvector testcontainers harness

frontend/              Vite + React 18 + TypeScript
```

## Sync pipeline

`POST /api/sync/trigger` runs these phases sequentially in a detached
goroutine per user. Each phase publishes an SSE `phase` event on
start/done so `/api/feed/stream` clients can progressive-update:

```
library → toplists → listening → releases → mb-artist-rels →
mb-same-label → lb-similar → lastfm-similar → (done)
```

Each phase has its own rate-limit gate (12h, 6h, 30min, etc.) so
re-triggering during development doesn't pollute the signals log or
inflate affinity scores. The in-process `scheduler` package fires the
same phases on a time ticker for every connected user.
