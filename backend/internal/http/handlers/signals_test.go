package handlers_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// signalsClient authenticates via the Spotify mock round-trip so the
// tests exercise the real RequireAuth middleware.
func signalsClient(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t, true)

	resp, err := h.client.Get(h.server.URL + "/api/auth/spotify/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	state := loc.Query().Get("state")

	resp2, err := h.client.Get(h.server.URL + "/api/auth/spotify/callback?code=c&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	// The callback associated the login with user_id = "spotify:sander"
	// (external_accounts provider/external_id). We need that for asserting
	// affinity rows later.
	return h, "spotify:sander"
}

func postJSON(t *testing.T, h *harness, path string, body any) (*http.Response, []byte) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestSignals_RequiresAuth(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Post(h.server.URL+"/api/signals", "application/json",
		strings.NewReader(`{"kind":"heard_good","subject_type":"artist","subject_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSignals_RejectsServerSideKind(t *testing.T) {
	h, _ := signalsClient(t)
	resp, _ := postJSON(t, h, "/api/signals", map[string]string{
		"kind": "library_add", "subject_type": "album", "subject_id": "x",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSignals_RejectsInvalidSubjectType(t *testing.T) {
	h, _ := signalsClient(t)
	resp, _ := postJSON(t, h, "/api/signals", map[string]string{
		"kind": "heard_good", "subject_type": "banana", "subject_id": "x",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSignals_HeardGood_HappyPath(t *testing.T) {
	h, _ := signalsClient(t)
	resp, body := postJSON(t, h, "/api/signals", map[string]string{
		"kind": "heard_good", "subject_type": "artist", "subject_id": "ar-42",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(body))
	}
	var got struct {
		Status  string            `json:"status"`
		Kind    string            `json:"kind"`
		Subject map[string]string `json:"subject"`
		Score   float64           `json:"score"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.Kind != "heard_good" {
		t.Fatalf("bad response: %+v", got)
	}
	if got.Subject["type"] != "artist" || got.Subject["id"] != "ar-42" {
		t.Fatalf("subject = %+v", got.Subject)
	}
	if got.Score != 1.5 {
		t.Fatalf("score = %v, want 1.5", got.Score)
	}
}

func TestSignals_DismissThenHeardBad_AccumulatesScore(t *testing.T) {
	h, _ := signalsClient(t)

	postJSON(t, h, "/api/signals", map[string]string{
		"kind": "dismiss", "subject_type": "album", "subject_id": "al-9",
	})
	resp, body := postJSON(t, h, "/api/signals", map[string]string{
		"kind": "heard_bad", "subject_type": "album", "subject_id": "al-9",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(body))
	}
	var got struct {
		Score float64 `json:"score"`
	}
	json.Unmarshal(body, &got)
	// dismiss (-0.5) + heard_bad (-1.5) = -2.0
	if got.Score != -2.0 {
		t.Fatalf("score = %v, want -2.0", got.Score)
	}
}
