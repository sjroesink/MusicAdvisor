package spotify

import "errors"

var (
	ErrNotConfigured  = errors.New("spotify: client id/secret not configured")
	ErrNoRedirectURI  = errors.New("spotify: redirect URI not configured")
	ErrUpstream       = errors.New("spotify: upstream error")
	ErrInvalidGrant   = errors.New("spotify: invalid grant")
)
