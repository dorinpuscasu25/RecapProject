// Package apperr defines typed, transport-agnostic domain errors with stable machine codes.
package apperr

import (
	"errors"
	"fmt"
	"time"
)

type Kind int

const (
	KindInternal Kind = iota
	KindBadRequest
	KindUnauthenticated
	KindForbidden
	KindNotFound
	KindConflict
	KindValidation
	KindRateLimited
	KindUnavailable
	KindGone
)

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Error struct {
	Kind       Kind
	Code       string // stable machine-readable code, e.g. "auth.invalid_credentials"
	Message    string // safe, human-readable message
	Fields     []FieldError
	RetryAfter time.Duration
	Details    map[string]any
	Err        error // wrapped cause; never exposed to clients
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, code, msg string) *Error { return &Error{Kind: kind, Code: code, Message: msg} }

func BadRequest(code, msg string) *Error    { return New(KindBadRequest, code, msg) }
func Unauthenticated(code, msg string) *Error { return New(KindUnauthenticated, code, msg) }
func Forbidden(code, msg string) *Error     { return New(KindForbidden, code, msg) }
func NotFound(code, msg string) *Error      { return New(KindNotFound, code, msg) }
func Conflict(code, msg string) *Error      { return New(KindConflict, code, msg) }
func Unavailable(code, msg string) *Error   { return New(KindUnavailable, code, msg) }
func Gone(code, msg string) *Error          { return New(KindGone, code, msg) }

func Validation(fields ...FieldError) *Error {
	return &Error{Kind: KindValidation, Code: "validation_failed", Message: "One or more fields are invalid.", Fields: fields}
}

func ValidationMsg(code, msg string) *Error { return New(KindValidation, code, msg) }

func RateLimited(retryAfter time.Duration) *Error {
	return &Error{Kind: KindRateLimited, Code: "rate_limited", Message: "Too many requests. Try again later.", RetryAfter: retryAfter}
}

func Internal(err error) *Error {
	return &Error{Kind: KindInternal, Code: "internal_error", Message: "An unexpected error occurred.", Err: err}
}

// VersionConflict is returned when optimistic concurrency checks fail.
func VersionConflict(resource string) *Error {
	return Conflict("version_conflict", fmt.Sprintf("The %s was modified by someone else. Reload and retry.", resource))
}

func (e *Error) WithErr(err error) *Error { e.Err = err; return e }
func (e *Error) WithDetail(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// As extracts an *Error from err, if present.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Is reports whether err is an *Error with the given code.
func Is(err error, code string) bool {
	e, ok := As(err)
	return ok && e.Code == code
}

// Validator accumulates field errors.
type Validator struct{ fields []FieldError }

func (v *Validator) Add(field, code, msg string) { v.fields = append(v.fields, FieldError{field, code, msg}) }
func (v *Validator) Check(ok bool, field, code, msg string) {
	if !ok {
		v.Add(field, code, msg)
	}
}
func (v *Validator) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return Validation(v.fields...)
}
