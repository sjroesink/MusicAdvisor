package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/db"
	mahttp "github.com/sjroesink/music-advisor/backend/internal/http"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

type harness struct {
	server  *httptest.Server
	client  *http.Client
	spotify *httptest.Server // mocked Spotify auth+api
}

func newHarness(t *testing.T, withSpotify bool) *harness {
	t.Helper()

	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	key := make([]byte, 32)
	rand.Read(key)
	cipher, _ := auth.NewCipher(key)

	sessions := auth.NewSessionStore(conn)
	users := user.NewService(conn, cipher)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var spotifyClient *spotify.Client
	var spotifyMock *httptest.Server
	if withSpotify {
		// Mock Spotify server: handles /api/token and /v1/me.
		mux := http.NewServeMux()
		mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "acc",
				"refresh_token": "ref",
				"token_type":    "Bearer",
				"scope":         "user-library-read",
				"expires_in":    3600,
			})
		})
		mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":           "sander",
				"display_name": "Sander",
			})
		})
		spotifyMock = httptest.NewServer(mux)
		t.Cleanup(spotifyMock.Close)

		spotifyClient, err = spotify.NewClient(spotify.Config{
			ClientID:     "id",
			ClientSecret: "secret",
			RedirectURI:  "http://example.test/api/auth/spotify/callback",
			AuthBase:     spotifyMock.URL,
			APIBase:      spotifyMock.URL + "/v1",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	handler := mahttp.NewRouter(mahttp.Deps{
		DB:             conn,
		Logger:         logger,
		Sessions:       sessions,
		CookieCfg:      auth.CookieConfig{},
		Users:          users,
		Spotify:        spotifyClient,
		FrontendOKPath: "/",
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow; we inspect redirects
		},
	}
	return &harness{server: srv, client: client, spotify: spotifyMock}
}

func TestMe_Unauthenticated_Returns401(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSpotifyLogin_WithoutCreds_Returns503(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Get(h.server.URL + "/api/auth/spotify/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestSpotifyOAuth_FullRoundTrip(t *testing.T) {
	h := newHarness(t, true)

	// Step 1: hit /login, expect 302 to Spotify authorize URL.
	resp, err := h.client.Get(h.server.URL + "/api/auth/spotify/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loc.String(), h.spotify.URL) {
		t.Fatalf("redirect should go to mocked Spotify, got %s", loc.String())
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("state missing from authorize URL")
	}

	// Step 2: simulate the browser coming back to /callback with code=&state=.
	callbackURL := h.server.URL + "/api/auth/spotify/callback?code=the-code&state=" + state
	resp2, err := h.client.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("callback status = %d, want 302. body: %s", resp2.StatusCode, string(body))
	}
	// After successful callback, we should have a session cookie.
	urlForCookies, _ := url.Parse(h.server.URL)
	cookies := h.client.Jar.Cookies(urlForCookies)
	var hasSession bool
	for _, c := range cookies {
		if c.Name == auth.CookieName {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatalf("session cookie not set after callback")
	}

	// Step 3: /api/me now returns 200 with our spotify display name.
	resp3, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, want 200", resp3.StatusCode)
	}
	body, _ := io.ReadAll(resp3.Body)
	if !strings.Contains(string(body), `"connected":true`) {
		t.Fatalf("me body missing connected flag: %s", string(body))
	}
	if !strings.Contains(string(body), `"spotify"`) {
		t.Fatalf("me body missing spotify block: %s", string(body))
	}

	// Step 4: logout clears the session; /api/me is 401 again.
	logoutReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.server.URL+"/api/auth/logout", nil)
	resp4, err := h.client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp4.StatusCode)
	}

	resp5, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp5.Body.Close()
	if resp5.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout me status = %d, want 401", resp5.StatusCode)
	}
}

func TestSpotifyCallback_ReusedState_Rejected(t *testing.T) {
	h := newHarness(t, true)

	// Start flow, grab state.
	resp, _ := h.client.Get(h.server.URL + "/api/auth/spotify/login")
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")

	// First callback: 302 success.
	u := h.server.URL + "/api/auth/spotify/callback?code=c&state=" + state
	if r, _ := h.client.Get(u); r != nil {
		r.Body.Close()
	}
	// Second callback with the same state must fail.
	r2, err := h.client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused-state callback status = %d, want 400", r2.StatusCode)
	}
}
