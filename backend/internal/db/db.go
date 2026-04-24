// Package db opens a pooled Postgres connection via pgx (stdlib shim) and
// applies any pending embedded migrations at startup. The stdlib shim
// lets services keep using database/sql while benefiting from pgx's
// native protocol support.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open dials the Postgres server at dsn, waits briefly for it to accept
// connections (dev docker-compose starts pg and app side by side, so Ping
// is retried with a small budget), and applies migrations.
func Open(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// Sane pool defaults. Postgres handles concurrency well; no reason to
	// pin to 1 like we did with SQLite.
	conn.SetMaxOpenConns(16)
	conn.SetMaxIdleConns(4)
	conn.SetConnMaxLifetime(30 * time.Minute)

	// Short ping retry loop so a freshly started `docker compose up` doesn't
	// need an explicit wait-for-it in the app container.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := conn.Ping(); err == nil {
			break
		} else if time.Now().After(deadline) {
			conn.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return conn, nil
}

func migrate(conn *sql.DB) error {
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}
	applied, err := loadApplied(conn)
	if err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	type pending struct {
		version string
		path    string
	}
	var todo []pending
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !matchesUpMigration(name) {
			continue
		}
		version := versionFromFilename(name)
		if _, done := applied[version]; done {
			continue
		}
		todo = append(todo, pending{version: version, path: "migrations/" + name})
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].version < todo[j].version })
	for _, p := range todo {
		sqlBytes, err := migrationsFS.ReadFile(p.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", p.path, err)
		}
		tx, err := conn.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", p.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES($1)`, p.version); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func loadApplied(conn *sql.DB) (map[string]struct{}, error) {
	rows, err := conn.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

func matchesUpMigration(name string) bool {
	if len(name) < len("0_.up.sql") {
		return false
	}
	return filepath.Ext(name) == ".sql" && hasSuffix(name, ".up.sql")
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func versionFromFilename(name string) string {
	for i, r := range name {
		if r < '0' || r > '9' {
			return name[:i]
		}
	}
	return name
}
