package musicbrainz

import (
	"context"
	"net/url"
)

// ArtistRelation is one edge in the MB artist-relation graph.
//
//   - Type e.g. "member of band", "collaborator", "supporting musician",
//     "performer".
//   - Direction is "backward" or "forward" depending on which end of
//     the relation the query artist sat on. We keep it so callers can
//     distinguish "A is a member of B" from "B has member A".
//   - Target is always the OTHER artist, never the query.
type ArtistRelation struct {
	Type      string
	Direction string
	Target    Artist
}

// FetchArtistRelations issues `/artist/{mbid}?inc=artist-rels` and returns
// the artist-to-artist edges. Excludes self-referential rows MB sometimes
// emits. Returns an empty slice (not ErrNotFound) when the artist has no
// relations, so callers don't have to branch on error.
func (c *Client) FetchArtistRelations(ctx context.Context, mbid string) ([]ArtistRelation, error) {
	q := url.Values{
		"inc": {"artist-rels"},
		"fmt": {"json"},
	}
	var body struct {
		Relations []struct {
			Type      string `json:"type"`
			Direction string `json:"direction"`
			Artist    struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				SortName string `json:"sort-name"`
			} `json:"artist"`
		} `json:"relations"`
	}
	if err := c.get(ctx, "/artist/"+mbid, q, &body); err != nil {
		return nil, err
	}
	out := make([]ArtistRelation, 0, len(body.Relations))
	for _, r := range body.Relations {
		if r.Artist.ID == "" || r.Artist.ID == mbid {
			continue
		}
		out = append(out, ArtistRelation{
			Type:      r.Type,
			Direction: r.Direction,
			Target: Artist{
				MBID:     r.Artist.ID,
				Name:     r.Artist.Name,
				SortName: r.Artist.SortName,
			},
		})
	}
	return out, nil
}

// RelationStrength turns a MB relation-type string into a 0..1 weight.
// "member of band" and "collaborator" rank highest because they imply
// artistic proximity; "supporting musician" and generic "performer" are
// weaker signals. Unknown types get a small non-zero weight so we don't
// silently drop genuinely-related artists if MB adds a new type later.
func RelationStrength(relType string) float64 {
	switch relType {
	case "member of band", "has member":
		return 1.0
	case "collaboration", "collaborator":
		return 0.9
	case "supporting musician":
		return 0.7
	case "performer", "vocal", "instrument":
		return 0.5
	default:
		return 0.3
	}
}
