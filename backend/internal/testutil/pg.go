// Package testutil provides test-only helpers. The Postgres harness here
// boots a pgvector container once per test binary (via sync.Once), then
// hands each call to OpenTestDB a freshly created, migrated database on
// that shared instance. Cleanup drops the database so test packages don't
// leak state across runs.
//
// Reuse: testcontainers' Reuse flag keeps the container alive between
// `go test` invocations on the same machine, so repeat runs are fast.
// CI runs that prune containers between jobs simply eat the first-start
// cost each time.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// pgImage bundles the pgvector extension; using the "pg16" tag keeps
	// dev and prod on the same major version.
	pgImage = "pgvector/pgvector:pg16"

	adminUser = "postgres"
	adminPass = "postgres"
	adminDB   = "postgres"
)

var (
	initOnce sync.Once
	initErr  error
	hostDSN  string // DSN that points at the shared admin database
)

// OpenTestDB returns a fresh *sql.DB connected to a newly-created database
// on the shared test container. Migrations are applied automatically via
// db.Open. The database is dropped on test cleanup.
//
// Set MA_TEST_DATABASE_URL to point at an existing Postgres (e.g. a local
// docker compose) and the helper skips the testcontainers spin entirely.
// That's useful when Docker-in-Docker isn't available.
func OpenTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("MA_TEST_DATABASE_URL")
	if dsn == "" {
		initOnce.Do(startContainer)
		if initErr != nil {
			t.Fatalf("testutil: pg container: %v", initErr)
		}
		dsn = hostDSN
	}

	// Create a fresh database so tests don't trip over each other's
	// schema + data.
	adminConn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testutil: open admin: %v", err)
	}
	defer adminConn.Close()

	dbName := "t_" + randomSuffix(10)
	if _, err := adminConn.Exec("CREATE DATABASE " + quoteIdent(dbName)); err != nil {
		t.Fatalf("testutil: create db %s: %v", dbName, err)
	}

	// Build a DSN pointing at the new database.
	testDSN := rewriteDSN(dsn, dbName)
	conn, err := db.Open(testDSN)
	if err != nil {
		_, _ = adminConn.Exec("DROP DATABASE " + quoteIdent(dbName))
		t.Fatalf("testutil: open test db: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		a, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer a.Close()
		// Force-close any stray connections before DROP; a lingering
		// conn would otherwise fail the DROP.
		_, _ = a.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1",
			dbName,
		)
		_, _ = a.Exec("DROP DATABASE IF EXISTS " + quoteIdent(dbName))
	})

	return conn
}

func startContainer() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// One container per test binary. We deliberately don't share the
	// container across packages with WithReuseByName because parallel
	// `go test ./...` runs race on the reuse path on Windows.
	//
	// Docker Desktop on Windows can briefly report "rootless" while it's
	// initialising for a parallel caller; retry a few times with backoff
	// before giving up so a `go test ./...` sweep doesn't fail spuriously.
	var container *tcpostgres.PostgresContainer
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		container, err = tcpostgres.Run(ctx, pgImage,
			tcpostgres.WithDatabase(adminDB),
			tcpostgres.WithUsername(adminUser),
			tcpostgres.WithPassword(adminPass),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err == nil {
			break
		}
		// Only retry on the transient Docker-provider race; surface real
		// container errors immediately.
		if !strings.Contains(err.Error(), "rootless Docker") {
			break
		}
		time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		initErr = fmt.Errorf("run container: %w", err)
		return
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		initErr = fmt.Errorf("connstring: %w", err)
		return
	}
	hostDSN = dsn
}

func randomSuffix(n int) string {
	b := make([]byte, n/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// quoteIdent is the subset of pq.QuoteIdentifier we need — database
// names here are hex-only, so the simple doubling is safe.
func quoteIdent(id string) string {
	return `"` + id + `"`
}

// rewriteDSN swaps the target database portion of a postgres URL.
// Postgres URLs look like: postgres://user:pass@host:port/dbname?params.
// We replace the slice between the last '/' (after host:port) and the
// first '?'.
func rewriteDSN(dsn, dbName string) string {
	// Find "/" after "@host:port". Skip the scheme "//".
	schemeEnd := indexOf(dsn, "://")
	if schemeEnd < 0 {
		return dsn
	}
	rest := dsn[schemeEnd+3:]
	slash := indexByte(rest, '/')
	if slash < 0 {
		return dsn
	}
	after := rest[slash+1:]
	q := indexByte(after, '?')
	if q < 0 {
		return dsn[:schemeEnd+3] + rest[:slash+1] + dbName
	}
	return dsn[:schemeEnd+3] + rest[:slash+1] + dbName + after[q:]
}

func indexOf(s, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return i
		}
	}
	return -1
}
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
