// Package graphapi holds what the Meta Graph adapters (threads, facebook, later
// instagram) share: the error envelope shape and its mapping onto the
// retryable/permanent split.
package graphapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// APIError is the Graph error envelope shared across Meta's APIs (Threads,
// Facebook, Instagram):
//
//	{"error": {"message", "type", "code", "error_subcode", "fbtrace_id"}}
type APIError struct {
	Error struct {
		Message      string `json:"message"`
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
	} `json:"error"`
}

// Classify maps a Graph API failure onto the adapter port's
// retryable/permanent split. Same rules for every Meta surface:
//
//	retryable: 429, 5xx, and the "not ready"/"not processed" style transient 400
//	          answers a polling flow can produce before content is finished;
//	permanent: everything else, including 401/403 and the expired-token code 190.
//
// platform is only for the error text (e.g. "threads", "facebook").
func Classify(platform string, status int, body []byte) error {
	var parsed APIError
	_ = json.Unmarshal(body, &parsed)
	msg := strings.TrimSpace(parsed.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	reason := fmt.Sprintf("%s: http %d: %s", platform, status, msg)
	if parsed.Error.FBTraceID != "" {
		reason += " (fbtrace " + parsed.Error.FBTraceID + ")"
	}
	cause := fmt.Errorf("%s api: status=%d code=%d type=%s", platform, status, parsed.Error.Code, parsed.Error.Type)

	switch {
	case status == 429 || status >= 500:
		return app.Retryable(cause, reason)
	case status == 401 || status == 403 || parsed.Error.Code == 190:
		return app.Permanent(cause, reason)
	case status == 400 && strings.Contains(strings.ToLower(msg), "not ready"):
		return app.Retryable(cause, reason)
	default:
		return app.Permanent(cause, reason)
	}
}
