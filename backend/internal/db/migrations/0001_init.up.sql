-- 0001_init: initial schema for Music Advisor.
-- Five clusters: identity, catalog, user_data, signals, derived.

-- ── Identity ────────────────────────────────────────────────────────

CREATE TABLE users (
  id         TEXT PRIMARY KEY,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sessions (
  id               TEXT PRIMARY KEY,
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at       DATETIME NOT NULL,
  last_accessed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  user_agent       TEXT,
  created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE external_accounts (
  user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider           TEXT NOT NULL,
  external_id        TEXT NOT NULL,
  access_token_enc   BLOB,
  refresh_token_enc  BLOB,
  token_expires_at   DATETIME,
  scopes             TEXT,
  needs_reconnect    INTEGER NOT NULL DEFAULT 0,
  connected_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, provider)
);
CREATE UNIQUE INDEX idx_external_accounts_provider_id
  ON external_accounts(provider, external_id);

CREATE TABLE oauth_states (
  state         TEXT PRIMARY KEY,
  code_verifier TEXT NOT NULL,
  expires_at    DATETIME NOT NULL
);

-- ── Catalog (shared, deduped on mbid) ────────────────────────────────

CREATE TABLE artists (
  mbid       TEXT PRIMARY KEY,
  spotify_id TEXT UNIQUE,
  name       TEXT NOT NULL,
  sort_name  TEXT,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE albums (
  mbid                TEXT PRIMARY KEY,
  spotify_id          TEXT UNIQUE,
  primary_artist_mbid TEXT REFERENCES artists(mbid),
  title               TEXT NOT NULL,
  release_date        TEXT,
  type                TEXT NOT NULL,
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
  mbid TEXT PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE album_labels (
  album_mbid TEXT NOT NULL REFERENCES albums(mbid) ON DELETE CASCADE,
  label_mbid TEXT NOT NULL REFERENCES labels(mbid),
  PRIMARY KEY (album_mbid, label_mbid)
);

CREATE TABLE tags (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE
);

CREATE TABLE artist_tags (
  artist_mbid TEXT NOT NULL REFERENCES artists(mbid) ON DELETE CASCADE,
  tag_id      INTEGER NOT NULL REFERENCES tags(id),
  source      TEXT NOT NULL,
  score       REAL,
  PRIMARY KEY (artist_mbid, tag_id, source)
);

CREATE TABLE album_tags (
  album_mbid TEXT NOT NULL REFERENCES albums(mbid) ON DELETE CASCADE,
  tag_id     INTEGER NOT NULL REFERENCES tags(id),
  source     TEXT NOT NULL,
  score      REAL,
  PRIMARY KEY (album_mbid, tag_id, source)
);

CREATE TABLE resolver_cache (
  spotify_id   TEXT NOT NULL,
  subject_type TEXT NOT NULL,
  mbid         TEXT,
  confidence   REAL,
  resolved_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (spotify_id, subject_type)
);

-- ── User data ────────────────────────────────────────────────────────

CREATE TABLE saved_artists (
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  artist_mbid TEXT NOT NULL REFERENCES artists(mbid),
  saved_at    DATETIME NOT NULL,
  PRIMARY KEY (user_id, artist_mbid)
);

CREATE TABLE saved_albums (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  album_mbid TEXT NOT NULL REFERENCES albums(mbid),
  saved_at   DATETIME NOT NULL,
  PRIMARY KEY (user_id, album_mbid)
);

CREATE TABLE saved_tracks (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_mbid TEXT NOT NULL REFERENCES tracks(mbid),
  saved_at   DATETIME NOT NULL,
  PRIMARY KEY (user_id, track_mbid)
);

CREATE TABLE play_history (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_mbid       TEXT REFERENCES tracks(mbid),
  spotify_track_id TEXT NOT NULL,
  played_at        DATETIME NOT NULL,
  source           TEXT NOT NULL,
  context_uri      TEXT,
  progress_ms      INTEGER,
  UNIQUE (user_id, spotify_track_id, played_at)
);
CREATE INDEX idx_play_history_user_time ON play_history(user_id, played_at DESC);

CREATE TABLE top_snapshots (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,
  time_range   TEXT NOT NULL,
  rank         INTEGER NOT NULL,
  subject_mbid TEXT NOT NULL,
  snapshot_at  DATETIME NOT NULL
);
CREATE INDEX idx_top_user_kind_range
  ON top_snapshots(user_id, kind, time_range, snapshot_at DESC);

CREATE TABLE playlists (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  spotify_id  TEXT NOT NULL,
  name        TEXT NOT NULL,
  track_count INTEGER,
  fetched_at  DATETIME NOT NULL,
  UNIQUE (user_id, spotify_id)
);

CREATE TABLE playlist_tracks (
  playlist_id      INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  track_mbid       TEXT REFERENCES tracks(mbid),
  spotify_track_id TEXT NOT NULL,
  position         INTEGER NOT NULL,
  PRIMARY KEY (playlist_id, position)
);

-- ── Signals (append-only source of truth) ────────────────────────────

CREATE TABLE signals (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,
  subject_type TEXT NOT NULL,
  subject_id   TEXT NOT NULL,
  weight       REAL NOT NULL,
  source       TEXT NOT NULL,
  context      TEXT,
  ts           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_signals_user_subject ON signals(user_id, subject_type, subject_id);
CREATE INDEX idx_signals_user_ts      ON signals(user_id, ts DESC);
CREATE INDEX idx_signals_user_kind    ON signals(user_id, kind);

-- ── Derived ──────────────────────────────────────────────────────────

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

CREATE TABLE album_affinity (
  user_id        TEXT NOT NULL,
  album_mbid     TEXT NOT NULL,
  score          REAL NOT NULL,
  signal_count   INTEGER NOT NULL DEFAULT 0,
  last_signal_at DATETIME,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, album_mbid)
);
CREATE INDEX idx_album_affinity_score ON album_affinity(user_id, score DESC);

CREATE TABLE track_affinity (
  user_id        TEXT NOT NULL,
  track_mbid     TEXT NOT NULL,
  score          REAL NOT NULL,
  signal_count   INTEGER NOT NULL DEFAULT 0,
  last_signal_at DATETIME,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, track_mbid)
);
CREATE INDEX idx_track_affinity_score ON track_affinity(user_id, score DESC);

CREATE TABLE label_affinity (
  user_id        TEXT NOT NULL,
  label_mbid     TEXT NOT NULL,
  score          REAL NOT NULL,
  signal_count   INTEGER NOT NULL DEFAULT 0,
  last_signal_at DATETIME,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, label_mbid)
);
CREATE INDEX idx_label_affinity_score ON label_affinity(user_id, score DESC);

CREATE TABLE tag_affinity (
  user_id        TEXT NOT NULL,
  tag_id         INTEGER NOT NULL,
  score          REAL NOT NULL,
  signal_count   INTEGER NOT NULL DEFAULT 0,
  last_signal_at DATETIME,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, tag_id)
);
CREATE INDEX idx_tag_affinity_score ON tag_affinity(user_id, score DESC);

CREATE TABLE discover_candidates (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  subject_type  TEXT NOT NULL,
  subject_id    TEXT NOT NULL,
  source        TEXT NOT NULL,
  raw_score     REAL NOT NULL,
  reason_data   TEXT NOT NULL,
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
  rating       TEXT NOT NULL,
  rated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, subject_type, subject_id)
);

CREATE TABLE sync_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     TEXT REFERENCES users(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  started_at  DATETIME NOT NULL,
  finished_at DATETIME,
  status      TEXT NOT NULL,
  items_added INTEGER DEFAULT 0,
  error       TEXT
);
CREATE INDEX idx_sync_runs_user ON sync_runs(user_id, started_at DESC);
