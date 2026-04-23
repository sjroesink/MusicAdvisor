package spotify_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
)

func newTestClient(t *testing.T, authHandler, apiHandler http.Handler) *spotify.Client {
	t.Helper()
	auth := httptest.NewServer(authHandler)
	t.Cleanup(auth.Close)
	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)

	c, err := spotify.NewClient(spotify.Config{
		ClientID:     "test-id",
		ClientSecret: "test-secret",
		RedirectURI:  "http://localhost:8080/api/auth/spotify/callback",
		AuthBase:     auth.URL,
		APIBase:      api.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClient_MissingCredsIsTypedError(t *testing.T) {
	_, err := spotify.NewClient(spotify.Config{RedirectURI: "x"})
	if !errors.Is(err, spotify.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestNewPKCE_StructureAndMatch(t *testing.T) {
	v, ch, err := spotify.NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier length %d out of RFC 7636 bounds", len(v))
	}
	if len(ch) == 0 {
		t.Fatal("empty challenge")
	}
	v2, ch2, _ := spotify.NewPKCE()
	if v == v2 || ch == ch2 {
		t.Fatal("NewPKCE returned identical pairs on back-to-back calls")
	}
}

func TestAuthorizeURL_Composition(t *testing.T) {
	c := newTestClient(t, http.NotFoundHandler(), http.NotFoundHandler())
	got := c.AuthorizeURL("xyz-state", "chal-123", nil)
	for _, want := range []string{
		"response_type=code",
		"client_id=test-id",
		"redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fapi%2Fauth%2Fspotify%2Fcallback",
		"state=xyz-state",
		"code_challenge=chal-123",
		"code_challenge_method=S256",
		"scope=user-library-read",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("AuthorizeURL missing %q:\n  %s", want, got)
		}
	}
}

func TestExchangeCode_OK(t *testing.T) {
	authMux := http.NewServeMux()
	authMux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("code_verifier") != "my-verifier" {
			t.Errorf("code_verifier = %q", r.Form.Get("code_verifier"))
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "test-id" || p != "test-secret" {
			t.Errorf("basic auth missing/wrong: ok=%v u=%q", ok, u)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc",
			"refresh_token": "ref",
			"token_type":    "Bearer",
			"scope":         "user-library-read",
			"expires_in":    3600,
		})
	})

	c := newTestClient(t, authMux, http.NotFoundHandler())
	ts, err := c.ExchangeCode(context.Background(), "auth-code", "my-verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if ts.AccessToken != "acc" || ts.RefreshToken != "ref" {
		t.Fatalf("got %+v", ts)
	}
	if ts.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt zero")
	}
}

func TestExchangeCode_InvalidGrant(t *testing.T) {
	authMux := http.NewServeMux()
	authMux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	c := newTestClient(t, authMux, http.NotFoundHandler())
	_, err := c.ExchangeCode(context.Background(), "bad", "v")
	if !errors.Is(err, spotify.ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
}

func TestRefreshToken_PreservesRefreshWhenOmitted(t *testing.T) {
	authMux := http.NewServeMux()
	authMux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-acc",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	c := newTestClient(t, authMux, http.NotFoundHandler())
	ts, err := c.RefreshToken(context.Background(), "old-ref")
	if err != nil {
		t.Fatal(err)
	}
	if ts.RefreshToken != "old-ref" {
		t.Fatalf("RefreshToken = %q, want old-ref preserved", ts.RefreshToken)
	}
	if ts.AccessToken != "new-acc" {
		t.Fatal("AccessToken not updated")
	}
}

func TestGetMe_OK(t *testing.T) {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer acc" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":           "sander",
			"display_name": "Sander",
			"email":        "sander@example.com",
			"images": []map[string]any{
				{"url": "https://i.scdn.co/x", "width": 200, "height": 200},
			},
		})
	})
	c := newTestClient(t, http.NotFoundHandler(), apiMux)
	me, err := c.GetMe(context.Background(), "acc")
	if err != nil {
		t.Fatal(err)
	}
	if me.ID != "sander" || me.DisplayName != "Sander" {
		t.Fatalf("got %+v", me)
	}
	if me.ImageURL() != "https://i.scdn.co/x" {
		t.Fatalf("ImageURL = %q", me.ImageURL())
	}
}
