package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSet is the relevant subset of the /api/token response.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	ExpiresAt    time.Time
}

// ExchangeCode swaps an OAuth authorization code for a TokenSet using PKCE.
// The codeVerifier must match the challenge sent during AuthorizeURL.
func (c *Client) ExchangeCode(ctx context.Context, code, codeVerifier string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.redirectURI},
		"client_id":     {c.clientID},
		"code_verifier": {codeVerifier},
	}
	return c.postToken(ctx, form)
}

// RefreshToken exchanges a refresh_token for a fresh TokenSet. Spotify may or
// may not rotate the refresh_token; we keep the old one if the response omits
// it (common behavior).
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.clientID},
	}
	ts, err := c.postToken(ctx, form)
	if err != nil {
		return ts, err
	}
	if ts.RefreshToken == "" {
		ts.RefreshToken = refreshToken
	}
	return ts, nil
}

func (c *Client) postToken(ctx context.Context, form url.Values) (TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.authBase+"/api/token", strings.NewReader(form.Encode()),
	)
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.SetBasicAuth(c.clientID, c.clientSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusBadRequest {
		// Most commonly `{"error":"invalid_grant"}` — surface cleanly.
		return TokenSet{}, fmt.Errorf("%w: %s", ErrInvalidGrant, string(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenSet{}, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, string(body))
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenSet{}, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}
	return TokenSet{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}
