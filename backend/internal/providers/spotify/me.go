package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Me is the relevant slice of the /me response.
type Me struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Country     string `json:"country"`
	Product     string `json:"product"`
	Images      []struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"images"`
}

// ImageURL returns the first image URL, or "" if none.
func (m Me) ImageURL() string {
	if len(m.Images) == 0 {
		return ""
	}
	return m.Images[0].URL
}

// GetMe calls /me with the given access token.
func (c *Client) GetMe(ctx context.Context, accessToken string) (Me, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/me", nil)
	if err != nil {
		return Me{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Me{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Me{}, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, string(body))
	}
	var me Me
	if err := json.Unmarshal(body, &me); err != nil {
		return Me{}, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}
	return me, nil
}
