package threads

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

type apiError struct {
	Error struct {
		Message      string `json:"message"`
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
	} `json:"error"`
}

// classify maps Graph API failures onto the adapter's retry/permanent split.
// Retryable: 429, 5xx, and "container not ready" style transient answers.
// Permanent: 400/401/403 and any other 4xx.
func classify(status int, body []byte) error {
	var parsed apiError
	_ = json.Unmarshal(body, &parsed)
	msg := strings.TrimSpace(parsed.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	reason := fmt.Sprintf("threads: http %d: %s", status, msg)
	if parsed.Error.FBTraceID != "" {
		reason += " (fbtrace " + parsed.Error.FBTraceID + ")"
	}
	cause := fmt.Errorf("threads api: status=%d code=%d type=%s", status, parsed.Error.Code, parsed.Error.Type)

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
