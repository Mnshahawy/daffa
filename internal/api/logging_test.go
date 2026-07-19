package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/httpx"
)

// capturedLogs runs fn with the default logger redirected to a buffer, and returns what it wrote.
func capturedLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// A failed request must say in the log WHY, not merely that it failed — the reason httpx handed
// the recorder rides on the one access-log line, so debugging "the user got an error" does not
// come down to guessing from a bare status code.
func TestLoggingLineCarriesTheFailureReason(t *testing.T) {
	h := logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, http.StatusConflict, "in_use", "Delete the deliveries first.")
	}))

	out := capturedLogs(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/x", nil))
	})

	for _, want := range []string{
		`"level":"WARN"`, `"status":409`,
		`"error_code":"in_use"`, `"error":"Delete the deliveries first."`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("access log is missing %s\nline: %s", want, out)
		}
	}
}

// A 500 hides its cause from the caller but must NOT hide it from the log — the underlying error
// is exactly what a maintainer needs.
func TestLoggingLineCarriesTheHiddenCause(t *testing.T) {
	h := logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, errString("disk is full"))
	}))

	out := capturedLogs(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/y", nil))
	})

	for _, want := range []string{`"level":"ERROR"`, `"status":500`, `"cause":"disk is full"`} {
		if !strings.Contains(out, want) {
			t.Errorf("access log is missing %s\nline: %s", want, out)
		}
	}
}

// A panicking handler must not take the connection down: it is caught, logged WITH its stack (the
// one error hardest to debug, and unrecovered the one logged least), and answered with a 500.
func TestLoggingRecoversAndReportsAPanic(t *testing.T) {
	h := logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	out := capturedLogs(t, func() {
		// If the panic escaped this call the test process would crash, which is the regression.
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/z", nil))
	})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 after a recovered panic", rec.Code)
	}
	for _, want := range []string{`"msg":"panic serving request"`, `"panic":"boom"`, `"stack":`, `"status":500`} {
		if !strings.Contains(out, want) {
			t.Errorf("panic was not logged with %s\nlog: %s", want, out)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
