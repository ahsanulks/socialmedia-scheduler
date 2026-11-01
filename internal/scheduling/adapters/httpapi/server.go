package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

const (
	maxBodyBytes         = 1 << 20 // 1 MiB
	maxIdempotencyKeyLen = 255
)

type Deps struct {
	Idempotency     app.IdempotencyStore
	Create          *app.SchedulePostHandler
	Update          *app.UpdateScheduleHandler
	Cancel          *app.CancelScheduleHandler
	Get             *app.GetScheduleHandler
	List            *app.ListSchedulesHandler
	Registry        *app.Registry
	Clock           app.Clock
	Token           string
	DefaultPlatform string
	Logger          *slog.Logger
	Ready           func(context.Context) error
}

type Server struct {
	deps Deps
	log  *slog.Logger
}

func New(deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{deps: deps, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /api/v1/platforms", s.auth(s.handlePlatforms))
	mux.HandleFunc("POST /api/v1/schedules", s.auth(s.handleCreate))
	mux.HandleFunc("GET /api/v1/schedules", s.auth(s.handleList))
	mux.HandleFunc("GET /api/v1/schedules/{id}", s.auth(s.handleGet))
	mux.HandleFunc("PATCH /api/v1/schedules/{id}", s.auth(s.handleUpdate))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.auth(s.handleCancel))
	return s.withRequestID(s.withLogging(mux))
}

// Middleware.

type ctxKey string

const requestIDKey ctxKey = "request_id"

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestIDFrom(r.Context()),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			s.problem(w, r, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		token := strings.TrimPrefix(header, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.Token)) != 1 {
			s.problem(w, r, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next(w, r)
	}
}

// Handlers.

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ready != nil {
		if err := s.deps.Ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	type info struct {
		Name           string   `json:"name"`
		MaxTextLength  int      `json:"max_text_length"`
		TextLengthUnit string   `json:"max_text_length_unit"`
		MediaKinds     []string `json:"media_kinds"`
		RequiresMedia  bool     `json:"requires_media"`
		SupportsText   bool     `json:"supports_text"`
		PreparesDraft  bool     `json:"prepares_draft"`
		DailyLimit     int      `json:"daily_limit"`
	}
	out := make([]info, 0)
	for _, p := range s.deps.Registry.Platforms() {
		caps, _ := s.deps.Registry.Capabilities(p)
		unit := "characters"
		if caps.TextLengthInBytes {
			unit = "bytes"
		}
		kinds := []string{}
		kinds = append(kinds, caps.MediaKinds...)
		out = append(out, info{
			Name:           p.String(),
			MaxTextLength:  caps.MaxTextLength,
			TextLengthUnit: unit,
			MediaKinds:     kinds,
			RequiresMedia:  caps.RequiresMedia,
			SupportsText:   caps.SupportsText,
			PreparesDraft:  caps.PreparesDraft,
			DailyLimit:     caps.DailyLimit,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"platforms": out})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Read the raw body first: the idempotency request hash is taken over
	// exactly the bytes the client sent, before the decoder re-formats them.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		if s.deps.Idempotency == nil {
			s.problem(w, r, http.StatusServiceUnavailable, "idempotency_not_configured",
				"Idempotency-Key support is not enabled on this deployment")
			return
		}
		if len(idemKey) > maxIdempotencyKeyLen {
			s.problem(w, r, http.StatusBadRequest, "bad_request",
				"Idempotency-Key must be at most 255 characters")
			return
		}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.problem(w, r, http.StatusBadRequest, "bad_request",
			"body must be valid JSON under "+strconv.Itoa(maxBodyBytes)+" bytes")
		return
	}
	if idemKey != "" {
		sum := sha256.Sum256(raw)
		bodyHash = hex.EncodeToString(sum[:])
		rec, found, err := s.deps.Idempotency.Get(idemKey)
		if err != nil {
			s.problem(w, r, http.StatusInternalServerError, "internal_error", "idempotency lookup failed")
			return
		}
		if found {
			if rec.RequestHash != bodyHash {
				s.problem(w, r, http.StatusUnprocessableEntity, "idempotency_key_mismatch",
					"this Idempotency-Key was used with a different request body")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(rec.StatusCode)
			_, _ = w.Write([]byte(rec.Response))
			return
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		s.problem(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	at, err := parseUTCTime(req.ScheduledAt)
	if err != nil {
		s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	platform := req.Platform
	if platform == "" {
		platform = s.deps.DefaultPlatform
	}
	res, err := s.deps.Create.Handle(r.Context(), app.SchedulePost{
		Content:     req.Content,
		ScheduledAt: at,
		Platform:    platform,
		MediaURL:    req.MediaURL(),
		MediaKind:   req.MediaKind(),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if idemKey != "" {
		resp, err := json.Marshal(createResponse{ID: res.ID, Version: res.Version})
		if err == nil {
			// Stored before the response is written so an interrupted
			// response still replays. Failure to store is logged, never
			// failed: the create itself succeeded.
			if err := s.deps.Idempotency.Put(app.IdempotencyRecord{
				Key:         idemKey,
				RequestHash: bodyHash,
				StatusCode:  http.StatusCreated,
				Response:    string(resp),
			}); err != nil {
				s.log.Warn("storing idempotency key failed", "key_id", idemKey, "error", err)
			}
		}
	}
	w.Header().Set("Location", "/api/v1/schedules/"+res.ID)
	writeJSON(w, http.StatusCreated, createResponse{ID: res.ID, Version: res.Version})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cmd := app.ListSchedules{
		Platform: q.Get("platform"),
		Cursor:   q.Get("cursor"),
	}
	if raw := q.Get("status"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				cmd.Statuses = append(cmd.Statuses, trimmed)
			}
		}
	}
	if raw := q.Get("from"); raw != "" {
		at, err := parseUTCTime(raw)
		if err != nil {
			s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", "from: "+err.Error())
			return
		}
		cmd.From = &at
	}
	if raw := q.Get("to"); raw != "" {
		at, err := parseUTCTime(raw)
		if err != nil {
			s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", "to: "+err.Error())
			return
		}
		cmd.To = &at
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.problem(w, r, http.StatusBadRequest, "bad_request", "limit must be an integer")
			return
		}
		cmd.Limit = n
	}
	switch strings.ToLower(q.Get("order")) {
	case "", "asc":
	case "desc":
		cmd.Desc = true
	default:
		s.problem(w, r, http.StatusBadRequest, "bad_request", "order must be asc or desc")
		return
	}

	page, err := s.deps.List.Handle(r.Context(), cmd)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]scheduleResponse, 0, len(page.Items))
	for _, v := range page.Items {
		items = append(items, viewToResponse(v))
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items, NextCursor: optional(page.NextCursor)})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	view, err := s.deps.Get.Handle(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewToResponse(view))
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := decodeJSON(r, &req); err != nil {
		s.problem(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.ExpectedVersion == nil {
		s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", "expected_version is required")
		return
	}
	var scheduledAt *time.Time
	if req.ScheduledAt != nil {
		at, err := parseUTCTime(*req.ScheduledAt)
		if err != nil {
			s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", "scheduled_at: "+err.Error())
			return
		}
		scheduledAt = &at
	}
	res, err := s.deps.Update.Handle(r.Context(), app.UpdateSchedule{
		ID:              r.PathValue("id"),
		Content:         req.Content,
		ScheduledAt:     scheduledAt,
		ExpectedVersion: *req.ExpectedVersion,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, createResponse{ID: res.ID, Version: res.Version})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	cmd := app.CancelSchedule{ID: r.PathValue("id")}
	if raw := r.URL.Query().Get("expected_version"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.problem(w, r, http.StatusBadRequest, "bad_request", "expected_version must be an integer")
			return
		}
		cmd.ExpectedVersion = &n
	}
	if _, err := s.deps.Cancel.Handle(r.Context(), cmd); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Error mapping.

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var validation domain.ValidationError
	if errors.As(err, &validation) {
		s.problem(w, r, http.StatusUnprocessableEntity, "validation_failed", validation.Error())
		return
	}
	var notFound domain.NotFoundError
	if errors.As(err, &notFound) {
		s.problem(w, r, http.StatusNotFound, "not_found", notFound.Error())
		return
	}
	var conflict domain.VersionConflictError
	if errors.As(err, &conflict) {
		s.problem(w, r, http.StatusConflict, "version_conflict", conflict.Error())
		return
	}
	var invalid domain.InvalidTransitionError
	if errors.As(err, &invalid) {
		s.problem(w, r, http.StatusConflict, "invalid_state", invalid.Error())
		return
	}
	var unsupported app.UnsupportedPlatformError
	if errors.As(err, &unsupported) {
		s.problem(w, r, http.StatusUnprocessableEntity, "unsupported_platform", unsupported.Error())
		return
	}
	var limit app.PlatformLimitError
	if errors.As(err, &limit) {
		s.problem(w, r, http.StatusUnprocessableEntity, "platform_limit_exceeded", limit.Error())
		return
	}
	var mediaRequired app.MediaRequiredError
	if errors.As(err, &mediaRequired) {
		s.problem(w, r, http.StatusUnprocessableEntity, "media_required", mediaRequired.Error())
		return
	}
	var unsupportedMedia app.UnsupportedMediaError
	if errors.As(err, &unsupportedMedia) {
		s.problem(w, r, http.StatusUnprocessableEntity, "unsupported_media_type", unsupportedMedia.Error())
		return
	}
	s.log.Error("unhandled error", "error", err, "request_id", requestIDFrom(r.Context()))
	s.problem(w, r, http.StatusInternalServerError, "internal_error", "unexpected error")
}

func (s *Server) problem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:      "https://social-scheduler.local/problems/" + code,
		Title:     code,
		Status:    status,
		Code:      code,
		Detail:    detail,
		RequestID: requestIDFrom(r.Context()),
	})
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}
