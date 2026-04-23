package db

import (
	"path/filepath"
	"testing"
)

func TestOpen_RunsMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	conn, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	var count int
	row := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if count == 0 {
		t.Fatalf("expected at least one applied migration, got 0")
	}

	expectTables := []string{
		"users", "sessions", "external_accounts", "artists", "albums",
		"tracks", "signals", "discover_candidates", "hides", "ratings",
	}
	for _, name := range expectTables {
		var found string
		err := conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
			name,
		).Scan(&found)
		if err != nil {
			t.Fatalf("expected table %s to exist: %v", name, err)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	conn1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	conn1.Close()

	conn2, err := Open(path)
	if err != nil {
		t.Fatalf("second open should not re-apply migrations: %v", err)
	}
	defer conn2.Close()
}
