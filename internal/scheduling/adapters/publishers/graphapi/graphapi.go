// Package graphapi holds what the Meta Graph adapters (threads, facebook,
// instagram) share: the error envelope shape, its mapping onto the
// retryable/permanent split, and the single form-POST client call.
package graphapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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

// PostForm sends one urlencoded POST to the Graph API and decodes the JSON
// response into out. Non-2xx answers are classified with Classify; network
// errors and unreadable responses are retryable.
//
// Known tradeoff: a 2xx body that does not parse is retryable even though the
// server may have applied the change. The retry flows tolerate a re-send
// (Threads re-publishes an existing container), but Facebook's single feed call
// could double-post in that corner; the exception is documented in its
// adapter and stayed inheritable so all three adapters keep one code path.
func PostForm(ctx context.Context, platform, baseURL string, client *http.Client, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return app.Permanent(err, platform+": bad request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return app.Retryable(err, platform+": network error")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return app.Retryable(err, platform+": unreadable response")
		}
		return nil
	}
	return Classify(platform, resp.StatusCode, body)
}

// GetJSON sends one GET request and decodes the JSON response into out; used
// by polling flows (Instagram container status).
func GetJSON(ctx context.Context, platform, baseURL string, client *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return app.Permanent(err, platform+": bad request")
	}

	resp, err := client.Do(req)
	if err != nil {
		return app.Retryable(err, platform+": network error")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return app.Retryable(err, platform+": unreadable response")
		}
		return nil
	}
	return Classify(platform, resp.StatusCode, body)
}

// PollTiming bounds a container-status wait. Defaults follow the
// facebook/instagram plan: first poll after ~15s, at most ~90s total, so the
// whole prepare phase fits inside the default 2-minute publish lease.
type PollTiming struct {
	Start    time.Duration // delay before the first status poll
	Interval time.Duration // delay between polls after the first
	Deadline time.Duration // total time to wait for a terminal status
}

func (t PollTiming) WithDefaults() PollTiming {
	if t.Start <= 0 {
		t.Start = 15 * time.Second
	}
	if t.Interval <= 0 {
		t.Interval = 15 * time.Second
	}
	if t.Deadline <= 0 {
		t.Deadline = 90 * time.Second
	}
	return t
}

// Wait runs poll() until it reports done or an error, or until the deadline
// elapses. Missing a still-processing container by the deadline is a retryable
// outcome: the worker requeues with backoff, and the container lives 24h.
func (t PollTiming) Wait(ctx context.Context, startedAt time.Time, poll func() (done bool, err error)) error {
	if delay := t.Start; delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return app.Retryable(ctx.Err(), "context canceled while waiting for container")
		case <-timer.C:
		}
	}
	for {
		done, err := poll()
		if err != nil || done {
			return err
		}
		if time.Since(startedAt) >= t.Deadline {
			return app.Retryable(fmt.Errorf("instagram: container not ready in %s", t.Deadline),
				"instagram: container still processing; requeued")
		}
		select {
		case <-ctx.Done():
			return app.Retryable(ctx.Err(), "context canceled while polling container")
		case <-time.After(t.Interval):
		}
	}
}
