package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/sse"
)

type FeedStreamDeps struct {
	Logger *slog.Logger
	Hub    *sse.Hub
}

// FeedStream is a long-lived SSE endpoint. The contract is minimal: we
// publish `event: update` messages whenever something in the user's feed
// has changed. Clients are expected to refetch GET /api/feed on each
// event — payloads here intentionally carry only "what phase moved",
// which is cheap to serialize and keeps the stream noisy-but-small.
//
// A periodic keep-alive comment prevents intermediate proxies from
// tearing the connection for inactivity. Browsers ignore comment lines.
func FeedStream(d FeedStreamDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Initial handshake frame so the client can confirm the stream is
		// live before anything interesting happens.
		fmt.Fprint(w, "event: ready\ndata: {}\n\n")
		flusher.Flush()

		ch, cancel := d.Hub.Subscribe(userID)
		defer cancel()

		ka := time.NewTicker(25 * time.Second)
		defer ka.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ka.C:
				// Comment line: keeps the connection warm, no app payload.
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			case ev, ok := <-ch:
				if !ok {
					return
				}
				kind := ev.Kind
				if kind == "" {
					kind = "update"
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, ev.Data)
				flusher.Flush()
			}
		}
	}
}

