package http

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/sjroesink/music-advisor/backend/internal/http/handlers"
)

type Deps struct {
	DB     *sql.DB
	Logger *slog.Logger
}

// NewRouter wires the chi router. Request-scoped middleware (auth, user
// context) is layered in on the /api subtree as phases land.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))

	r.Get("/healthz", handlers.Health(d.DB, d.Logger))

	return r
}
