// Package httpx contains HTTP helpers: JSON encoding, RFC 9457 problem responses, pagination
// cursors and common middleware.
package httpx

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
)

// Problem is the RFC 9457 error body returned for every error response.
type Problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Code      string              `json:"code"`
	Detail    string              `json:"detail,omitempty"`
	RequestID string              `json:"request_id,omitempty"`
	Errors    []apperr.FieldError `json:"errors,omitempty"`
	Details   map[string]any      `json:"details,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil || status == http.StatusNoContent {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// StatusFor maps an error kind to an HTTP status code.
func StatusFor(k apperr.Kind) int {
	switch k {
	case apperr.KindBadRequest:
		return http.StatusBadRequest
	case apperr.KindUnauthenticated:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindValidation:
		return http.StatusUnprocessableEntity
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	case apperr.KindUnavailable:
		return http.StatusServiceUnavailable
	case apperr.KindGone:
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// WriteError renders any error as application/problem+json without leaking internals.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := apperr.As(err)
	if !ok {
		e = apperr.Internal(err)
	}
	status := StatusFor(e.Kind)
	if status >= 500 {
		logx.From(r.Context()).Error("request failed", "code", e.Code, "error", err.Error(), "path", r.URL.Path)
	}
	if e.Kind == apperr.KindUnauthenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="newsintel"`)
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	p := Problem{
		Type:      "https://docs.newsintel.invalid/errors/" + e.Code,
		Title:     http.StatusText(status),
		Status:    status,
		Code:      e.Code,
		Detail:    e.Message,
		RequestID: logx.RequestID(r.Context()),
		Errors:    e.Fields,
		Details:   e.Details,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// DecodeJSON decodes a JSON request body strictly (unknown fields rejected, single object only).
func DecodeJSON(r *http.Request, dst any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return apperr.New(apperr.KindBadRequest, "unsupported_media_type", "Content-Type must be application/json.")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return apperr.BadRequest("body_too_large", "Request body is too large.")
		case errors.Is(err, io.EOF):
			return apperr.BadRequest("empty_body", "Request body is required.")
		default:
			var ute *json.UnmarshalTypeError
			if errors.As(err, &ute) {
				return apperr.Validation(apperr.FieldError{Field: ute.Field, Code: "invalid_type", Message: "Invalid type."})
			}
			return apperr.BadRequest("malformed_json", "Malformed JSON body: "+sanitize(err.Error()))
		}
	}
	if dec.More() {
		return apperr.BadRequest("malformed_json", "Request body must contain a single JSON object.")
	}
	return nil
}

func sanitize(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Page is the standard cursor-paginated list envelope.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Cursor is an opaque keyset cursor: (timestamp, id) pair.
type Cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
	S  float64   `json:"s,omitempty"` // optional score for ranked lists
	O  int       `json:"o,omitempty"` // optional offset for small ranked lists
}

func EncodeCursor(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, apperr.BadRequest("invalid_cursor", "Invalid pagination cursor.")
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, apperr.BadRequest("invalid_cursor", "Invalid pagination cursor.")
	}
	return &c, nil
}

// Limit parses ?limit= with bounds.
func Limit(r *http.Request, def, max int) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// QueryTime parses an RFC3339 query parameter.
func QueryTime(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.Validation(apperr.FieldError{Field: name, Code: "invalid_time", Message: "Must be RFC3339."})
	}
	t = t.UTC()
	return &t, nil
}

// Accepted writes a 202 with an async job reference.
func Accepted(w http.ResponseWriter, jobID, statusURL string) {
	w.Header().Set("Location", statusURL)
	WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "queued", "status_url": statusURL})
}

func Errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }
