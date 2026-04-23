package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withWorkingDir changes the working directory for the duration of the test.
func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func writeEnvFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clearEnv snapshots and clears a list of env vars for the test; restores them on cleanup.
func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	saved := make(map[string]string, len(keys))
	for _, k := range keys {
		saved[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})
}

func TestLoadDotEnvFiles_LocalBeatsBase(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	clearEnv(t, "MA_TEST_FOO", "MA_TEST_BAR")

	writeEnvFile(t, filepath.Join(dir, ".env"), "MA_TEST_FOO=from-base\nMA_TEST_BAR=from-base\n")
	writeEnvFile(t, filepath.Join(dir, ".env.local"), "MA_TEST_FOO=from-local\n")

	loadDotEnvFiles()

	if got := os.Getenv("MA_TEST_FOO"); got != "from-local" {
		t.Fatalf("MA_TEST_FOO = %q, want from-local (local should win)", got)
	}
	if got := os.Getenv("MA_TEST_BAR"); got != "from-base" {
		t.Fatalf("MA_TEST_BAR = %q, want from-base (fallback to .env)", got)
	}
}

func TestLoadDotEnvFiles_RealEnvBeatsBothFiles(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	clearEnv(t, "MA_TEST_FOO")
	_ = os.Setenv("MA_TEST_FOO", "from-real-env")

	writeEnvFile(t, filepath.Join(dir, ".env"), "MA_TEST_FOO=from-base\n")
	writeEnvFile(t, filepath.Join(dir, ".env.local"), "MA_TEST_FOO=from-local\n")

	loadDotEnvFiles()

	if got := os.Getenv("MA_TEST_FOO"); got != "from-real-env" {
		t.Fatalf("MA_TEST_FOO = %q, want from-real-env (real env must win)", got)
	}
}

func TestLoadDotEnvFiles_NoFilesIsNoOp(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)

	// Must not panic, must not error, simply returns.
	loadDotEnvFiles()
}
