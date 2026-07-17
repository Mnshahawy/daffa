// Package httpx holds the small HTTP conveniences the rest of Daffa shares: one
// JSON envelope, one error shape, one SSE writer. It is deliberately thin — a
// framework here would cost more than it saves.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// ErrorBody is the single error shape the API returns. The frontend keys on `code`,
// shows `message`, and never has to parse prose.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; all we can do is leave a trace.
		slog.Error("writing JSON response", "err", err)
	}
}

// Fail writes a client-visible error.
func Fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if status >= 500 {
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "code", code, "message", message)
	}
	JSON(w, status, ErrorBody{Code: code, Message: message})
}

// Error maps an unexpected server-side error to a 500 without leaking its text to
// the caller. The detail goes to the log, where it belongs.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("unhandled error", "method", r.Method, "path", r.URL.Path, "err", err)
	JSON(w, http.StatusInternalServerError, ErrorBody{
		Code:    "internal",
		Message: "Something went wrong on our side.",
	})
}

// Decode reads a JSON body with a size cap, so a hostile or broken client cannot
// make us allocate arbitrarily.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	const maxBody = 1 << 20 // 1 MiB — every request body in Daffa is small
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("request body exceeds %d bytes", maxBody)
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// BadRequest is the common "the client sent something wrong" path.
func BadRequest(w http.ResponseWriter, r *http.Request, message string) {
	Fail(w, r, http.StatusBadRequest, "bad_request", message)
}
