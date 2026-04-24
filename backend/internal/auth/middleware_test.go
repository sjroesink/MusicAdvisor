package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/testutil"
)

func TestRequireAuth_NoCookie401(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	defer conn.Close()

	store := auth.NewSessionStore(conn)
	h := auth.RequireAuth(store, auth.CookieConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestRequireAuth_ValidCookieForwardsWithUserID(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	defer conn.Close()

	userID := "u-123"
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}

	store := auth.NewSessionStore(conn)
	sess, err := store.Create(context.Background(), userID, "")
	if err != nil {
		t.Fatal(err)
	}

	var seenUserID string
	h := auth.RequireAuth(store, auth.CookieConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUserID = auth.UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if seenUserID != userID {
		t.Fatalf("context user id = %q, want %q", seenUserID, userID)
	}
}
