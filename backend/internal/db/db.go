package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens the SQLite database at path, configures WAL and sensible
// pragmas, and runs any pending migrations embedded in the binary.
func Open(path string) (*sql.DB, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	// _pragma entries are applied per connection on open.
	dsn := "file:" + path + "?" +
		"_pragma=journal_mode(WAL)&" +
		"_pragma=synchronous(NORMAL)&" +
		"_pragma=foreign_keys(ON)&" +
		"_pragma=busy_timeout(5000)&" +
		"_time_format=sqlite"

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1) // serialize writes; reads go via separate connections added later
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return conn, nil
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return mkdirAll(dir)
}

func migrate(conn *sql.DB) error {
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, p.version); err != nil {
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
	// matches <digits>_<name>.up.sql
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
