package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

type createRequest struct {
	Content     string      `json:"content"`
	ScheduledAt string      `json:"scheduled_at"`
	Platform    string      `json:"platform"`
	Media       *mediaInput `json:"media,omitempty"`
}

// mediaInput is the optional attachment on create. Media is immutable after
// creation, so no update equivalent exists.
type mediaInput struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

func (r createRequest) MediaURL() string {
	if r.Media == nil {
		return ""
	}
	return r.Media.URL
}

func (r createRequest) MediaKind() string {
	if r.Media == nil {
		return ""
	}
	return r.Media.Kind
}

type updateRequest struct {
	Content         *string `json:"content"`
	ScheduledAt     *string `json:"scheduled_at"`
	ExpectedVersion *int    `json:"expected_version"`
}

type createResponse struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type scheduleResponse struct {
	ID             string  `json:"id"`
	Content        string  `json:"content"`
	MediaURL       *string `json:"media_url,omitempty"`
	MediaKind      *string `json:"media_kind,omitempty"`
	Status         string  `json:"status"`
	ScheduledAt    string  `json:"scheduled_at"`
	Platform       string  `json:"platform"`
	Attempts       int     `json:"attempts"`
	LastError      *string `json:"last_error"`
	ExternalPostID *string `json:"external_post_id"`
	Version        int     `json:"version"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	PublishedAt    *string `json:"published_at"`
}

type listResponse struct {
	Items      []scheduleResponse `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func viewToResponse(v app.ScheduleView) scheduleResponse {
	return scheduleResponse{
		ID:             v.ID,
		Content:        v.Content,
		MediaURL:       v.MediaURL,
		MediaKind:      v.MediaKind,
		Status:         v.Status,
		ScheduledAt:    v.ScheduledAt.UTC().Format(time.RFC3339),
		Platform:       v.Platform,
		Attempts:       v.Attempts,
		LastError:      v.LastError,
		ExternalPostID: v.ExternalPostID,
		Version:        v.Version,
		CreatedAt:      v.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      v.UpdatedAt.UTC().Format(time.RFC3339),
		PublishedAt:    formatOptional(v.PublishedAt),
	}
}

func formatOptional(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("body is empty")
		}
		return fmt.Errorf("body must be valid JSON: %w", err)
	}
	return nil
}

// parseUTCTime accepts RFC 3339 timestamps that end in Z. Offsets are rejected:
// the service stores UTC only.
func parseUTCTime(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, errors.New("is required")
	}
	if !strings.HasSuffix(trimmed, "Z") {
		return time.Time{}, errors.New("must be UTC (RFC 3339 ending in Z)")
	}
	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, errors.New("must be an RFC 3339 timestamp")
	}
	return t.UTC(), nil
}
