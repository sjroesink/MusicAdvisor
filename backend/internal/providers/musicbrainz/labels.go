package musicbrainz

import (
	"context"
	"fmt"
	"net/url"
	"sort"
)

// Label is a MusicBrainz label summary.
type Label struct {
	MBID string
	Name string
}

// ReleaseGroupLabels fetches the release-group along with its releases and
// the label-info for each release, then flattens to a deduplicated slice of
// labels. Empty names are dropped so callers don't need to guard against
// MB's occasional "[unknown]" entries.
func (c *Client) ReleaseGroupLabels(ctx context.Context, releaseGroupMBID string) ([]Label, error) {
	q := url.Values{
		"inc": {"releases+labels"},
		"fmt": {"json"},
	}
	var body struct {
		Releases []struct {
			LabelInfo []struct {
				Label struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"label"`
			} `json:"label-info"`
		} `json:"releases"`
	}
	if err := c.get(ctx, "/release-group/"+releaseGroupMBID, q, &body); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []Label{}
	for _, rel := range body.Releases {
		for _, li := range rel.LabelInfo {
			if li.Label.ID == "" || li.Label.Name == "" {
				continue
			}
			if seen[li.Label.ID] {
				continue
			}
			seen[li.Label.ID] = true
			out = append(out, Label{MBID: li.Label.ID, Name: li.Label.Name})
		}
	}
	return out, nil
}

// BrowseReleaseGroupsByLabel returns distinct release-groups published on
// the given label. MB's release-group browse endpoint does NOT support the
// `label` parameter, so we hit `/release?label=X&inc=release-groups`,
// collect release-groups, and dedupe by MBID. The result is ordered by
// FirstReleaseDate DESC (most recent first).
func (c *Client) BrowseReleaseGroupsByLabel(ctx context.Context, labelMBID string, limit int) ([]ReleaseGroup, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{
		"label":  {labelMBID},
		"type":   {"album|ep"},
		"status": {"official"},
		"inc":    {"release-groups"},
		"limit":  {fmt.Sprintf("%d", limit)},
		"fmt":    {"json"},
	}
	var body struct {
		Releases []struct {
			ReleaseGroup struct {
				ID               string `json:"id"`
				Title            string `json:"title"`
				PrimaryType      string `json:"primary-type"`
				FirstReleaseDate string `json:"first-release-date"`
				ArtistCredit     []struct {
					Artist struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"artist"`
				} `json:"artist-credit"`
			} `json:"release-group"`
		} `json:"releases"`
	}
	if err := c.get(ctx, "/release", q, &body); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]ReleaseGroup, 0, len(body.Releases))
	for _, rel := range body.Releases {
		rg := rel.ReleaseGroup
		if rg.ID == "" || seen[rg.ID] {
			continue
		}
		seen[rg.ID] = true
		r := ReleaseGroup{
			MBID:             rg.ID,
			Title:            rg.Title,
			PrimaryType:      rg.PrimaryType,
			FirstReleaseDate: rg.FirstReleaseDate,
		}
		if len(rg.ArtistCredit) > 0 {
			r.ArtistID = rg.ArtistCredit[0].Artist.ID
			r.ArtistName = rg.ArtistCredit[0].Artist.Name
		}
		out = append(out, r)
	}
	// Newest first. MB's `/release?label=` ordering is not stable.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].FirstReleaseDate > out[j].FirstReleaseDate
	})
	return out, nil
}
