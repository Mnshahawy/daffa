package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The sampling panel's Save button did not work. Not "worked badly" — did not work, ever, for any
// value, valid or not.
//
// The form posted the settings object it had been handed, `updated_at` and all, and updated_at is
// a Go time.Time. The UI sent it as "", the decoder refused it, and every save came back 400
// "invalid JSON body: parsing time ...". A person could change the interval, press Save, read an
// error about JSON, and have no idea what they had done wrong. The answer was: nothing.
//
// updated_at is the SERVER's — it stamps it. So the request no longer has one to get wrong.
func TestSamplingSettingsSaveWithoutATimestamp(t *testing.T) {
	s, ctx := monitorServer(t)

	// Exactly what the form sends: the three numbers a person can choose, and nothing else.
	rec := put(s, `{"enabled":true,"interval_secs":60,"retention_days":14}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("saving valid settings returned %d: %s", rec.Code, rec.Body)
	}

	var got store.MonitorSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.IntervalSecs != 60 || got.RetentionDays != 14 {
		t.Errorf("saved 60s/14d, got back %ds/%dd", got.IntervalSecs, got.RetentionDays)
	}
	// The response must carry the timestamp the store actually wrote, not a zero time.
	if got.UpdatedAt.IsZero() {
		t.Error("the response carried a zero updated_at — the client is being told the settings " +
			"have never been saved, immediately after saving them")
	}

	// And it landed.
	cfg, err := s.store.MonitorSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IntervalSecs != 60 {
		t.Errorf("the interval was not persisted: %ds", cfg.IntervalSecs)
	}
}

// Sampling faster than the floor is a choice a PERSON made, so it owes them a 400 and the reason —
// not a 500 and "something went wrong on our side", which is both untrue and unactionable.
func TestSamplingBelowTheFloorIsRefusedWithAReason(t *testing.T) {
	s, _ := monitorServer(t)

	for _, secs := range []int{1, 10, 29} {
		rec := put(s, `{"enabled":true,"interval_secs":`+strconv.Itoa(secs)+`,"retention_days":7}`)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("an interval of %ds returned %d; a value a person typed and we refuse is a "+
				"400, and it must say why", secs, rec.Code)
			continue
		}
		if body := rec.Body.String(); !strings.Contains(body, "30") {
			t.Errorf("the refusal of %ds never names the minimum: %s", secs, body)
		}
	}

	// The floor itself saves. An off-by-one here would make the DEFAULT unsaveable.
	if rec := put(s, `{"enabled":true,"interval_secs":30,"retention_days":7}`); rec.Code != http.StatusOK {
		t.Errorf("the minimum interval was itself refused: %d %s", rec.Code, rec.Body)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func monitorServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	return &Server{store: st, notify: notify.New(st, fakeSealer{}, log)}, ctx
}

func put(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/settings/monitoring", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleSaveMonitorSettings(rec, req)
	return rec
}
