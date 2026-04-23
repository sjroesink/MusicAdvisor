# Music Advisor — Backend Design

**Status:** draft for review
**Date:** 2026-04-22
**Author:** Claude (pairing with sjroesink@gmail.com)
**Target:** MVP v1 — Spotify-linked music recommendations, rich signal profile, multi-source discover with provenance.

---

## 1. Summary

Turn the existing React/Vite frontend prototype into a real, functional, multi-user web app. A Go backend provides Spotify-linked accounts, pulls signals from Spotify (library, top lists, recent plays, playlists), aggregates new releases via MusicBrainz, and produces discover recommendations from multiple sources (ListenBrainz now; MusicBrainz artist-rels, same-label, and Last.fm in later phases) with explicit per-item provenance.

Data lives in SQLite. Profile is a raw signal event log plus time-decayed affinity scores per artist / album / track / label / tag. Sync is hybrid: scheduled background workers plus an on-demand "kick" when the user opens the feed with stale data. Feed reads go against a pre-computed `discover_candidates` pool, not live external API calls.

One Go binary, one SQLite file, one Docker image. Served locally during development; deployed behind a reverse proxy on Unraid for self-hosted production.

---

## 2. Scope

**In MVP:**
- Spotify OAuth login, multi-user accounts
- Full Spotify signal collection: saved library + saved tracks + followed artists + top artists/tracks (short/medium/long term) + recently-played (high-freq poll) + playlists (owned and collaborative)
- Skip detection via consecutive recently-played snapshots
- MusicBrainz new-releases per followed artist
- ListenBrainz similar-artists → MB release lookup as first discover source
- Signal event log + derived affinity tables
- Hides (Dismiss) + good/bad rating signals
- Per-section type filter (already in UI, backend just respects it as a query param)
- SSE stream of sync progress and card arrivals
- Dockerized deployment

**Phase 2 (not in this spec's implementation plan, but schema/interfaces prepare for it):**
- MusicBrainz artist-rels discover source
- Same-label discover source
- Last.fm discover source and optional per-user Last.fm integration
- Manual "acquire these credentials" prereq steps are covered; code is stubbed

**Explicitly out:**
- Other music providers (Tidal, Apple Music, Deezer)
- Social / sharing features
- Mobile native app
- Admin panel UI (DB inspection suffices for MVP)

---

## 3. Architecture & topology

Single monorepo, single Go binary, SQLite on a persistent volume.

```
┌─────────────────────────────────────────────────────────┐
│  Go binary: music-advisor                               │
│                                                         │
│  HTTP (chi)   SSE hub   Worker pool (goroutines + cron) │
│       │          │              │                       │
│       └──────────┼──────────────┘                       │
│                  │                                      │
│             Services (auth, sync, feed, signal,         │
│                       affinity, discover)               │
│                  │                                      │
│        ┌─────────┴─────────┐                            │
│        │                   │                            │
│   Providers           sqlc repo layer                   │
│   (spotify, MB,             │                           │
│    LB, lastfm,              │                           │
│    resolver)         SQLite (WAL mode)                  │
└─────────────────────────────────────────────────────────┘
          ▲
          │  HTTPS via Traefik (prod) / http://localhost (dev)
          ▼
    React + Vite frontend (served as embedded static in prod;
                           Vite dev-server + proxy in dev)
```

**Key stack choices:**
- **Router:** `chi` — lightweight, idiomatic, composable middleware.
- **Database:** SQLite with WAL. Single-node is fine for MVP scale (hundreds of users).
- **Driver:** `modernc.org/sqlite` — pure Go, no cgo, cross-compiles to linux/arm64 for Unraid trivially.
- **Query layer:** `sqlc` — type-safe generated queries from SQL files; no ORM magic.
- **Migrations:** `golang-migrate` with numbered SQL files.
- **Rate limiting:** `golang.org/x/time/rate` — token-bucket per provider.
- **Logging:** `log/slog` structured logging.
- **Tests:** `testing`, `testify/require`, `go-vcr` for recorded provider fixtures.

**Topology rules:**
- Monorepo layout: `/frontend`, `/backend`, `/docs`. Frontend moves from current root into `/frontend/`.
- One process. Workers are goroutines with `time.Ticker`. No separate worker daemon (SQLite requires single-writer anyway).
- Frontend and backend build into one Docker image: multi-stage (node → go → distroless/static). Final image ~25–35 MB, non-root user, binary-only.
- Configuration via environment variables. Secrets never committed; `.env` for dev, file-based secrets for prod.

**Deployment model:**
- **Dev:** `go run ./cmd/server` on `:8080`, Vite dev-server on `:5173` with proxy config routing `/api` to `:8080`.
- **Prod (Unraid):** single container, volume mount for SQLite DB, exposed via Traefik with TLS. Healthcheck `GET /healthz`.

**Config env-vars:**
- `MA_DATABASE_PATH` (default `./data/music-advisor.db`)
- `MA_BASE_URL` (e.g. `http://localhost:8080` dev, `https://music.example.tld` prod)
- `MA_SESSION_SECRET` (64-byte random, for cookie signing)
- `MA_SECRET_KEY` (32-byte random, AES-GCM key for refresh-token encryption at rest)
- `MA_SPOTIFY_CLIENT_ID`, `MA_SPOTIFY_CLIENT_SECRET`
- `MA_LASTFM_API_KEY` (phase 2; nil = Last.fm disabled)
- `MA_LISTENBRAINZ_TOKEN` (optional)
- `MA_USER_AGENT_CONTACT` (e.g. `sjroesink@gmail.com`, required for MB user-agent)
- `MA_LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`)

**Prerequisites (manual steps, outside code):**
1. Register Spotify Developer App at `developer.spotify.com`. Redirect URI: `{MA_BASE_URL}/api/auth/spotify/callback`. Scopes needed listed in section 5.
2. Register Last.fm API account at `last.fm/api/account/create` (phase 2 only).
3. ListenBrainz token optional (`listenbrainz.org/settings/` → user token); MVP works without.

---

## 4. Data model

SQLite schema in 5 clusters. Full DDL lives in `/backend/migrations/0001_init.up.sql`; this section shows the structure and rationale.

### 4.1 Identity cluster

```sql
CREATE TABLE users (
  id          TEXT PRIMARY KEY,         -- UUID v4
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sessions (
  id               TEXT PRIMARY KEY,    -- 32-byte random, base64url
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at       DATETIME NOT NULL,
  last_accessed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  user_agent       TEXT,
  created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE external_accounts (
  user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider           TEXT NOT NULL,      -- 'spotify' | 'lastfm'
  external_id        TEXT NOT NULL,      -- spotify_user_id or lastfm username
  access_token_enc   BLOB,               -- AES-GCM, nonce prepended
  refresh_token_enc  BLOB,
  token_expires_at   DATETIME,
  scopes             TEXT,
  connected_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, provider)
);

CREATE TABLE oauth_states (
  state           TEXT PRIMARY KEY,
  code_verifier   TEXT NOT NULL,         -- PKCE
  expires_at      DATETIME NOT NULL
);
```

Refresh tokens are encrypted with AES-GCM using `MA_SECRET_KEY` before writing. Key rotation is a phase-3 concern.

### 4.2 Catalog cluster (shared across users, deduped on MBID)

```sql
CREATE TABLE artists (
  mbid         TEXT PRIMARY KEY,
  spotify_id   TEXT UNIQUE,
  name         TEXT NOT NULL,
  sort_name    TEXT,
  updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE albums (
  mbid                TEXT PRIMARY KEY,
  spotify_id          TEXT UNIQUE,
  primary_artist_mbid TEXT REFERENCES artists(mbid),
  title               TEXT NOT NULL,
  release_date        TEXT,                -- ISO 8601, may be YYYY or YYYY-MM
  type                TEXT NOT NULL,       -- 'Album' | 'EP' | 'Single'
  track_count         INTEGER,
  length_sec          INTEGER,
  updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_albums_artist ON albums(primary_artist_mbid);
CREATE INDEX idx_albums_release_date ON albums(release_date);

CREATE TABLE tracks (
  mbid         TEXT PRIMARY KEY,
  spotify_id   TEXT UNIQUE,
  album_mbid   TEXT REFERENCES albums(mbid),
  artist_mbid  TEXT REFERENCES artists(mbid),
  title        TEXT NOT NULL,
  duration_sec INTEGER,
  updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE labels (
  mbid   TEXT PRIMARY KEY,
  name   TEXT NOT NULL
);
CREATE TABLE album_labels (
  album_mbid   TEXT NOT NULL REFERENCES albums(mbid) ON DELETE CASCADE,
  label_mbid   TEXT NOT NULL REFERENCES labels(mbid),
  PRIMARY KEY (album_mbid, label_mbid)
);

CREATE TABLE tags (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  name   TEXT NOT NULL UNIQUE
);
CREATE TABLE artist_tags (
  artist_mbid TEXT NOT NULL REFERENCES artists(mbid) ON DELETE CASCADE,
  tag_id      INTEGER NOT NULL REFERENCES tags(id),
  source      TEXT NOT NULL,   -- 'musicbrainz' | 'listenbrainz' | 'spotify'
  score       REAL,
  PRIMARY KEY (artist_mbid, tag_id, source)
);
CREATE TABLE album_tags (
  album_mbid  TEXT NOT NULL REFERENCES albums(mbid) ON DELETE CASCADE,
  tag_id      INTEGER NOT NULL REFERENCES tags(id),
  source      TEXT NOT NULL,
  score       REAL,
  PRIMARY KEY (album_mbid, tag_id, source)
);

CREATE TABLE resolver_cache (
  spotify_id    TEXT NOT NULL,
  subject_type  TEXT NOT NULL,              -- 'artist' | 'album' | 'track'
  mbid          TEXT,                       -- NULL = tombstone (lookup failed)
  confidence    REAL,
  resolved_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (spotify_id, subject_type)
);
```

### 4.3 User-data cluster

```sql
CREATE TABLE saved_artists (
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  artist_mbid  TEXT NOT NULL REFERENCES artists(mbid),
  saved_at     DATETIME NOT NULL,
  PRIMARY KEY (user_id, artist_mbid)
);
CREATE TABLE saved_albums (
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  album_mbid  TEXT NOT NULL REFERENCES albums(mbid),
  saved_at    DATETIME NOT NULL,
  PRIMARY KEY (user_id, album_mbid)
);
CREATE TABLE saved_tracks (
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_mbid  TEXT NOT NULL REFERENCES tracks(mbid),
  saved_at    DATETIME NOT NULL,
  PRIMARY KEY (user_id, track_mbid)
);

CREATE TABLE play_history (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_mbid   TEXT REFERENCES tracks(mbid),      -- may be NULL if unresolved
  spotify_track_id TEXT NOT NULL,
  played_at    DATETIME NOT NULL,
  source       TEXT NOT NULL,                     -- 'recently-played'
  context_uri  TEXT,                              -- album/playlist context
  progress_ms  INTEGER,                           -- nullable; when derivable
  UNIQUE (user_id, spotify_track_id, played_at)
);
CREATE INDEX idx_play_history_user_time ON play_history(user_id, played_at DESC);

CREATE TABLE top_snapshots (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,     -- 'artist' | 'track'
  time_range   TEXT NOT NULL,     -- 'short' | 'medium' | 'long'
  rank         INTEGER NOT NULL,  -- 1..50
  subject_mbid TEXT NOT NULL,
  snapshot_at  DATETIME NOT NULL
);
CREATE INDEX idx_top_user_kind_range ON top_snapshots(user_id, kind, time_range, snapshot_at DESC);

CREATE TABLE playlists (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  spotify_id    TEXT NOT NULL,
  name          TEXT NOT NULL,
  track_count   INTEGER,
  fetched_at    DATETIME NOT NULL,
  UNIQUE (user_id, spotify_id)
);
CREATE TABLE playlist_tracks (
  playlist_id   INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  track_mbid    TEXT REFERENCES tracks(mbid),
  spotify_track_id TEXT NOT NULL,
  position      INTEGER NOT NULL,
  PRIMARY KEY (playlist_id, position)
);
```

### 4.4 Signals cluster (append-only, source of truth)

```sql
CREATE TABLE signals (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,
  subject_type  TEXT NOT NULL,     -- 'artist' | 'album' | 'track' | 'label' | 'tag' | 'type'
  subject_id    TEXT NOT NULL,     -- mbid (or for 'type', a string like 'EP')
  weight        REAL NOT NULL,
  source        TEXT NOT NULL,
  context       TEXT,              -- optional JSON
  ts            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_signals_user_subject ON signals(user_id, subject_type, subject_id);
CREATE INDEX idx_signals_user_ts ON signals(user_id, ts DESC);
CREATE INDEX idx_signals_user_kind ON signals(user_id, kind);
```

**Signal kinds and default weights:**

| kind | subject_type | source | default weight | notes |
|---|---|---|---|---|
| `library_add` | artist/album/track | spotify-library | +1.0 | on sync discovery |
| `follow_add` | artist | spotify-library | +1.2 | followed artist |
| `top_rank` | artist/track | spotify-top | +2.0 × (1 - rank/50) × range_mult | range_mult: short=1.5, medium=1.0, long=0.7 |
| `play_full` | track | spotify-recent-derived | +0.3 | inferred completion (next play gap ≥ duration − 30s) |
| `play_skip` | track | spotify-recent-derived | −0.3 | inferred skip, confidence in `context` JSON |
| `playlist_add` | track | spotify-library | +0.5 | in user-owned playlist |
| `heard_good` | album/track | ui | +1.5 | |
| `heard_bad` | album/track | ui | −1.5 | |
| `dismiss` | album/track | ui | −0.5 | also creates `hides` row |
| `filter_click` | type | ui | +0.05 | weak signal; implies type-preference |
| `open_click` | album/track | ui | +0.1 | opened in Spotify |

Signals are propagated to multiple subjects on write: e.g. a `heard_good` on an album creates signals for the album AND a derived `artist_boost` signal for the album's artist at 0.5× weight. Propagation rules live in `services/signal`.

### 4.5 Derived cluster

```sql
CREATE TABLE artist_affinity (
  user_id        TEXT NOT NULL,
  artist_mbid    TEXT NOT NULL,
  score          REAL NOT NULL,
  signal_count   INTEGER NOT NULL DEFAULT 0,
  last_signal_at DATETIME,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, artist_mbid)
);
CREATE INDEX idx_artist_affinity_score ON artist_affinity(user_id, score DESC);

-- identical shape for album_affinity, track_affinity, label_affinity, tag_affinity

CREATE TABLE discover_candidates (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  subject_type  TEXT NOT NULL,       -- 'album' | 'track'
  subject_id    TEXT NOT NULL,       -- mbid
  source        TEXT NOT NULL,       -- 'listenbrainz' | 'mb-artist-rels' | 'mb-same-label' | 'lastfm'
  raw_score     REAL NOT NULL,
  reason_data   TEXT NOT NULL,       -- JSON
  discovered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at    DATETIME,
  UNIQUE (user_id, subject_type, subject_id, source)
);
CREATE INDEX idx_dc_user_expires ON discover_candidates(user_id, expires_at);

CREATE TABLE hides (
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  subject_type TEXT NOT NULL,
  subject_id   TEXT NOT NULL,
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, subject_type, subject_id)
);

CREATE TABLE ratings (
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  subject_type TEXT NOT NULL,
  subject_id   TEXT NOT NULL,
  rating       TEXT NOT NULL,       -- 'pending' | 'good' | 'bad'
  rated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, subject_type, subject_id)
);

CREATE TABLE sync_runs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      TEXT REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,   -- see section 6
  started_at   DATETIME NOT NULL,
  finished_at  DATETIME,
  status       TEXT NOT NULL,   -- 'running' | 'ok' | 'partial' | 'failed'
  items_added  INTEGER DEFAULT 0,
  error        TEXT
);
CREATE INDEX idx_sync_runs_user ON sync_runs(user_id, started_at DESC);
```

**Affinity score formula:**
- On each new signal: `new_score = old_score + signal.weight × time_decay_factor(signal.ts)` with `time_decay_factor(ts) = exp(-ln(2) × age_days / 180)` (half-life 180 days).
- Score clamped to `[-10, +20]` to prevent runaway values from long-term heavy listeners.
- Weekly full-rebuild job: DELETE from affinity tables, replay all signals with current formula. Allows changing weights / half-life without schema migrations.

---

## 5. Provider integrations

Each provider lives in its own package under `/backend/internal/providers/`. No shared abstraction beyond `Name()` and `RateLimit()` — providers have fundamentally different concepts.

### 5.1 Spotify

**OAuth flow (Authorization Code + PKCE):**
1. `GET /api/auth/spotify/login`:
   - Generate `state` (32 bytes) + `code_verifier` (43 bytes).
   - Insert `oauth_states` row, 5-min TTL.
   - Redirect to `https://accounts.spotify.com/authorize?response_type=code&client_id=…&redirect_uri=…&state=…&code_challenge=…&code_challenge_method=S256&scope=<scopes>`.
2. `GET /api/auth/spotify/callback?code=&state=`:
   - Look up and delete `oauth_states` row; reject if not found or expired.
   - Exchange `code` for `access_token`, `refresh_token`, expiry.
   - `GET https://api.spotify.com/v1/me` → `spotify_user_id`, display name, image.
   - Upsert user: if `external_accounts` row exists for this `spotify_user_id`, use that user_id; else create new user.
   - Encrypt tokens, upsert `external_accounts`.
   - Create session row, set `ma_session` cookie.
   - Redirect to frontend `/`.

**Scopes:** `user-library-read user-follow-read user-top-read user-read-recently-played playlist-read-private playlist-read-collaborative user-read-email`.

**Endpoints consumed:**

| Endpoint | Cadence | Purpose |
|---|---|---|
| `/me` | on login | user id + display |
| `/me/albums` | 6h | saved albums (paginate, 50/page) |
| `/me/tracks` | 6h | saved tracks |
| `/me/following?type=artist` | 6h | followed artists |
| `/me/top/artists?time_range={short,medium,long}_term` | 24h | top artists (3 calls) |
| `/me/top/tracks?time_range={short,medium,long}_term` | 24h | top tracks (3 calls) |
| `/me/player/recently-played?limit=50` | 20m | recent plays, skip-detection input |
| `/me/playlists` | 24h | user's playlists (owned + collaborative) |
| `/playlists/{id}/tracks` | 24h | tracks per playlist |

**Rate limiting:** global token bucket, 10 req/s, burst 20. 429 → exponential backoff (250ms × 2^attempt, max 3 attempts).

**Token refresh:** lazy. On 401 response, call `POST /api/token` with `grant_type=refresh_token`, update `external_accounts`, retry original request once. If refresh also returns 4xx → mark session as degraded, force re-login on next frontend request.

**Skip detection:**
- On each `/me/player/recently-played` poll, fetch latest 50 plays.
- Compare with previous snapshot (deduped on `(user_id, played_at)`).
- Insert new `play_history` rows for plays not seen before.
- For each new play: if previous play's `played_at + duration_sec - 30s > new_play.played_at`, previous play is marked `play_skip` with `context={"confidence":0.8}`. Otherwise `play_full` (weight +0.3).
- Emit corresponding signals.

### 5.2 MusicBrainz

**Base URL:** `https://musicbrainz.org/ws/2/`. **Rate limit: strictly 1 req/s, global**, User-Agent required.

All MB calls pass through a single worker goroutine processing a FIFO queue. Parallel MB calls are forbidden (ban risk).

**Use cases:**
- **New releases:** `release-group/?artist={mbid}&type=album|ep|single&status=official&inc=ratings+tags` per followed artist, filtered to `first-release-date >= now-90d`. Queued on library-sync completion.
- **Artist relations (phase 2):** `artist/{mbid}?inc=artist-rels` finds members, collaborators, supporting musicians.
- **Same-label discover (phase 2):** `release-group/?label={mbid}&type=album&status=official`.
- **Tags:** `inc=tags` parameter on artist and release-group calls; written to `artist_tags` / `album_tags` with `source='musicbrainz'`.

**User-Agent:** `MusicAdvisor/0.1 (+{MA_USER_AGENT_CONTACT})`.

### 5.3 ListenBrainz

**Base URL:** `https://api.listenbrainz.org/1/`. **Rate limit:** 50 req/min anonymous (documented). We self-cap at 45 req/min to leave headroom. No token needed for reads used here.

**Endpoints:**
- `cf/similar/artists/{mbid}?algorithm=session_based_days_7500&count=50` — similar artists with scores.

**Discover strategy (first source):**
1. Pick top-20 artists from `artist_affinity` by score.
2. For each, call LB similar-artists.
3. Filter out artists the user already follows or has hidden.
4. For each similar artist with LB score ≥ 0.5: enqueue MB `release-group` lookup to find 1–2 well-rated recent albums.
5. Write `discover_candidates` rows: `source='listenbrainz'`, `raw_score=lb_score`, `reason_data={"via_artist_mbid":"…","via_artist_name":"Grouper","lb_score":0.87}`.
6. `expires_at = now + 7d`.

### 5.4 Last.fm (phase 2 stub)

Package exists, interface is wired, scheduler skips it if `MA_LASTFM_API_KEY` is empty. Endpoints planned: `artist.getSimilar`, `user.getLovedTracks` (if user connects their Last.fm), `user.getRecentTracks`.

### 5.5 Resolver — Spotify ID ↔ MBID

Spotify does not expose MBIDs. Mapping is best-effort:

- **Tracks:** Spotify `/tracks/{id}` returns `external_ids.isrc`. MB lookup: `recording/?query=isrc:{isrc}&fmt=json`. Hit rate > 90%.
- **Albums:** Spotify `/albums/{id}` returns `external_ids.upc`. MB lookup: `release/?query=barcode:{upc}&fmt=json`, then `release-group` parent. Lower hit rate; fallback to string match `artist_name + title + release_year`.
- **Artists:** MB query on name, cross-validated with a known album MBID from that artist. Confidence recorded.

Every lookup writes `resolver_cache` row (including failed lookups as tombstones, `mbid=NULL`). Background job retries tombstones after 30 days.

---

## 6. Sync & worker design

**Scheduler:** single `workers/scheduler.go` with multiple `time.Ticker`s, all context-cancellable for graceful shutdown.

**Tickers:**

| Tick | Cadence | Job | Scope |
|---|---|---|---|
| `library` | 6h | Pull Spotify library (albums, tracks, followed artists) | per active user |
| `top_lists` | 24h | Pull Spotify top artists/tracks × 3 ranges | per active user |
| `recent_plays` | 20min | Pull `/me/player/recently-played`, run skip detection, emit signals | per active user |
| `playlists` | 24h | Pull Spotify playlists + tracks | per active user |
| `mb_releases` | 6h (piggybacks `library`) | For each followed artist, query MB for new releases | per user |
| `discover_listenbrainz` | every 30min | Refresh candidate pool for stale users | per user with stale candidates |
| `affinity_rebuild` | every Sunday 03:00 UTC | Full replay of signals into affinity tables | global |
| `resolver_retry` | daily 04:00 UTC | Retry tombstoned resolver rows older than 30d | global |

**Active user** = any user with `sessions.last_accessed_at` within the last 30 days. The auth middleware updates `last_accessed_at` on every authenticated request (throttled to at most once per minute per session to avoid write amplification). Inactive users are skipped to reduce provider load.

**On-demand trigger:**
`POST /api/sync/trigger` enqueues an immediate synchronous run of `library` + `top_lists` + `recent_plays` + `mb_releases` + `discover_listenbrainz` for the calling user, streamed via their SSE connection.

**Feed-open stale-check:**
On `GET /api/feed`, if `last_sync_at` for this user is older than 30 minutes, the handler:
1. Returns current feed from DB immediately (fast response).
2. Asynchronously enqueues a light sync run (recent_plays + discover refresh only).
3. Client's `/api/feed/stream` SSE connection receives new items as they're discovered.

**Worker runner:**
Every job runs under `workers/runner.go`:
1. Insert `sync_runs` row with `status='running'`.
2. Execute job with timeout (default 5 min, skip-detection 1 min, MB new-releases 30 min due to rate limit).
3. On success: update `finished_at`, `status='ok'`, `items_added`.
4. On error: `status='failed'`, `error=…`. No panic propagation; error is logged and stored.
5. Partial success allowed: `status='partial'` with error field populated.

**SSE hub (`sse/hub.go`):**
- `map[userID][]chan Event`, guarded by RWMutex.
- `Connect(userID) (<-chan Event, closeFn)` registers a channel.
- `Broadcast(userID, event)` fans out to all user channels, drops if channel full (buffer size 32).
- Handler writes `data: {json}\n\n` per event, flushes, heartbeats every 30s to keep proxy connections alive.

**Graceful shutdown:**
1. On SIGTERM/SIGINT, cancel root context.
2. HTTP server `Shutdown()` with 10s timeout.
3. Scheduler tickers stop; in-flight worker jobs run to completion with 30s grace.
4. SSE hub closes all channels.
5. Close DB.

---

## 7. Frontend integration

The current frontend already has the UI surface. The work is to swap mock data for real API calls and add the auth/SSE plumbing.

**Changes:**
1. **Move into `/frontend/`** — relocate from repo root. Update `Dockerfile` accordingly.
2. **Remove `data.ts` mock module.** Replace with an `api.ts` client using `fetch` with `credentials: 'include'`.
3. **Rewrite `useMusicAdvisor`:**
   - On mount: `GET /api/me`. 401 → show `ConnectScreen`; success → call `GET /api/feed` for initial state, open `GET /api/feed/stream` (SSE).
   - SSE event handlers: `status` updates step/library_count, `release` pushes to `newReleases`, `discover` pushes to `discover`, `done` sets stage=`ready`.
   - Drop the fake timer-based streaming simulator.
4. **ConnectScreen** "Connect Spotify" button → `window.location.href = '/api/auth/spotify/login'`.
5. **Signal POSTs** — replace the local `onDismiss` / `onRate` / `onFilterType` state mutations with `POST /api/signals`, then optimistically update local state, reconcile on response.
6. **Multi-reason discover display** — component change: when `reasons.length > 1`, show primary reason with subtle "(+{n-1} more)" affordance; on hover/focus, show the rest inline. Grid variant shows all; list variant shows primary only.
7. **Error handling:** on 401 anywhere, drop session state and re-render `ConnectScreen`. Show a small warning banner if SSE disconnects + reconnect with exponential backoff (built into `EventSource` semantics but needs app-level awareness).
8. **Theme toggle, filter UI, card interactions** — unchanged. They already produce the right events; they just emit them to the backend now.

**Dev-time proxy:** `vite.config.ts` gains:
```ts
server: {
  port: 5173,
  proxy: {
    '/api': 'http://localhost:8080',
    '/healthz': 'http://localhost:8080',
  },
},
```

---

## 8. Error handling & rate limiting

**Per-provider rate limiters** (`golang.org/x/time/rate`, global buckets):

| Provider | Rate | Burst |
|---|---|---|
| Spotify | 10 req/s | 20 |
| MusicBrainz | 1 req/s | 1 (strict) |
| ListenBrainz | 45 req/min (anon) | 10 |
| Last.fm | 5 req/s | 10 |

**Retry policy:** exponential backoff on 429 and 5xx: `250ms × 2^attempt + jitter`, max 3 attempts. On final failure: log + record in `sync_runs.error`, move on.

**Circuit breaker:** after 5 consecutive failures to a given provider (across all users), skip all calls to that provider for 30 min. Recorded per-provider in-memory; reset on successful call.

**Token refresh failure cascade:**
- 401 on Spotify call → refresh token → retry.
- 401 on refresh → mark `external_accounts` row `needs_reconnect=true`, log, abandon job.
- Next frontend `/api/me` call surfaces `{reconnect_required: true}` → frontend routes back to `ConnectScreen`.

**Graceful degradation:**
- If LB is down, feed still serves existing cached candidates. User sees slightly stale discover.
- If MB is down, new-releases sync fails; feed serves previously-synced releases. Header status shows "sync_error" state.
- If Spotify is down entirely → `/api/feed` still works (serves cached data), but recent_plays/library sync will fail and be retried.

**Logging:** structured `slog` everywhere. Context: request_id, user_id, provider, endpoint, duration_ms, outcome. Log level from `MA_LOG_LEVEL`.

**No alerting in MVP.** `sync_runs` table + `/api/sync/runs` endpoint is enough for debugging by inspection.

---

## 9. Testing strategy

**Unit tests:** one `_test.go` per package. Services mock their provider + db dependencies via interfaces. Affinity formula, skip-detection heuristic, propagation rules, feed-merging logic all unit-tested.

**Integration tests (`/backend/test/`):**
- Spin up a temporary SQLite (`t.TempDir()`), run migrations, exercise full service flows.
- Cover: OAuth callback round-trip, signal write + affinity update + feed read, sync_runs error paths.

**Provider contract tests:**
- `go-vcr` records real HTTP fixtures into `/backend/internal/providers/*/testdata/`. CI replays them. Fixtures refreshed manually when a provider changes API shape.
- No live-API calls in CI (secrets + flake risk).

**Frontend E2E:**
- Existing Playwright test (in MCP) extended: mock `/api/me`, `/api/feed`, `/api/feed/stream` via Playwright's `route.fulfill()`. Verify: connect flow, SSE-driven streaming, heard/rating flow, type filter.

**CI:**
- GitHub Actions: `go test ./...`, `sqlc diff` (schema drift check), frontend `npm run typecheck` + `npm run build`, Playwright smoke.
- No live-API; all mocked.

**Local dev quality-of-life:**
- `make seed` — load a "demo user" with canned Spotify data from fixtures.
- `make reset-db` — wipe SQLite and re-run migrations.
- `make rebuild-affinity` — re-run weekly affinity rebuild job on demand.

---

## 10. Phasing / delivery order

1. **Phase 0 — prerequisites (manual):** register Spotify Dev App.
2. **Phase 1 — skeleton:** Go module, chi router, SQLite + migrations (0001_init), `GET /healthz`, Docker build.
3. **Phase 2 — auth:** Spotify OAuth flow, sessions, `/api/me`, `ConnectScreen` wiring.
4. **Phase 3 — catalog & library sync:** Spotify library + resolver + catalog upsert; `saved_*` tables populated. Signals written for `library_add`, `follow_add`.
5. **Phase 4 — signals API + affinity:** `/api/signals`, event propagation, incremental affinity updates, `ratings` + `hides` tables wired. Frontend wires Heard/Dismiss/filter.
6. **Phase 5 — top + recent-plays + skip detection:** 24h and 20min tickers, `top_rank` and `play_*` signals.
7. **Phase 6 — MB new releases:** MB worker, 6h tick, `mb_releases` job, feed shows new_releases.
8. **Phase 7 — ListenBrainz discover:** LB client, discover_listenbrainz job, candidate pool. Feed shows discover with provenance.
9. **Phase 8 — SSE live streaming:** SSE hub, `/api/feed/stream`, frontend `EventSource`.
10. **Phase 9 — polish & Dockerize for Unraid:** multi-stage Dockerfile, compose for Unraid, volume mount docs, Traefik labels example.

Each phase is independently shippable (backend green build + frontend works in degraded mode without later phases).

---

## 11. Open questions for phase-2 planning

These are NOT blockers for MVP, but worth noting:
- Discover expiry policy per source — phase 2 will need different `expires_at` windows for MB artist-rels vs LB vs Last.fm.
- Affinity propagation rules — currently a simple 0.5× album→artist; worth revisiting after first week of real signals.
- Skip-detection false-positive rate — needs empirical measurement on the author's own account before trusting the −0.3 weight.
- Cross-provider deduplication of candidates — same release found via LB and MB-same-label should collapse into one card. MVP uses `UNIQUE (user_id, subject_type, subject_id, source)` + feed-time merge; phase 2 may need smarter.

---

## 12. Appendix: frontend data-shape compatibility

Current frontend types (from `/src/data.ts`):

```ts
interface NewRelease { id; artist; title; year; date; type; tracks; length; reason; cover; }
interface DiscoverItem { id; artist; title; year; type; tracks; length; reason; cover; }
```

Backend response shapes match these exactly, with minor additions:
- `id` = `"album:${mbid}"` string.
- `cover` = placeholder string (e.g. `"NF · PARAPHRASES"`) until real cover URLs are added in phase 2.
- `reason` (singular) kept for backward compat; `reasons[]` array added on `DiscoverItem` only.
- `rating: null | "pending" | "good" | "bad"` added to both (reflects `ratings` table).
- `links.spotify: string` added for "Open" button (currently opens `#`).
