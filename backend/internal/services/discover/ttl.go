// Package discover holds logic shared by every discover-source service
// (releases, lbsimilar, mbrels, mbsamelabel, lfsimilar, …). Individual
// source services own their fetch loop; this package owns the cross-cutting
// policies: candidate TTL per source, source-name constants, and (later)
// cross-source merge rules.
package discover

import "time"

// Source identifies which provider produced a discover_candidates row.
// Values are persisted in discover_candidates.source — keep them stable.
const (
	SourceMBReleases  = "mb_new_release" // legacy name from phase 6, kept for compat
	SourceLBSimilar   = "listenbrainz"   // legacy name from phase 7, kept for compat
	SourceMBArtistRel = "mb_artist_rels"
	SourceMBSameLabel = "mb_same_label"
	SourceLastfmSim   = "lastfm_similar"
)

// TTLForSource decides how long a discover candidate stays visible before
// the next sync can overwrite or drop it. Shorter TTLs suit time-sensitive
// signals (new releases, similar-artists that track the zeitgeist); longer
// TTLs suit catalog discovery where the underlying relationship is stable.
func TTLForSource(source string) time.Duration {
	switch source {
	case SourceMBReleases:
		return 7 * 24 * time.Hour
	case SourceLBSimilar, SourceLastfmSim:
		return 7 * 24 * time.Hour
	case SourceMBArtistRel, SourceMBSameLabel:
		// Artist-rels and same-label don't churn; a 14d window lets slow-
		// discovery candidates survive a week of user inactivity without
		// re-querying MB.
		return 14 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}
