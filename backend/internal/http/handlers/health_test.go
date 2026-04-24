package handlers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/http/handlers"
)

func TestHealth_OK(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	defer conn.Close()

	h := handlers.Health(conn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("body = %q, want \"ok\"", rr.Body.String())
	}
}
