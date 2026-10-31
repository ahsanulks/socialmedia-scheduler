package domain

import (
	"net/url"
	"strings"
)

// Media is the optional visual attachment of a scheduled post. It is set at
// creation and never changes: platforms fetch the file from the URL at publish
// time, so the URL is part of what was scheduled.
type Media struct {
	url  string
	kind string
}

// The closed set of media kinds. Which kinds a platform accepts is its own
// Capabilities rule (MediaKinds), not a domain rule: the domain only rejects
// words outside this set.
const (
	MediaKindImage = "image"
	MediaKindVideo = "video"
)

// NewMedia validates an attachment. https only — the platform will fetch this
// URL, and both Meta publishing endpoints require a public server.
func NewMedia(rawURL, kind string) (Media, error) {
	u := strings.TrimSpace(rawURL)
	if len(u) > 2048 {
		return Media{}, invalid("media.url", "must be at most 2048 characters")
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Media{}, invalid("media.url", "must be a valid https URL")
	}
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case MediaKindImage, MediaKindVideo:
	default:
		return Media{}, invalid("media.kind", "must be image or video")
	}
	return Media{url: u, kind: k}, nil
}

func (m Media) URL() string  { return m.url }
func (m Media) Kind() string { return m.kind }
