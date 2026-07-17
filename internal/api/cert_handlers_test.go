package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The whole CA lifecycle, end to end through the handlers: create a CA, issue a leaf,
// stage a rotation, watch the bundle carry both roots, activate, and confirm the leaf was
// re-signed under the new root without its key changing. This is the state machine
// docs/certs.md promises, exercised as HTTP.
func TestCARotationLifecycle(t *testing.T) {
	s, ctx := certServer(t)

	// ── create a CA and issue a leaf ────────────────────────────────────────────
	ca := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"internal-ca","common_name":"Example Internal CA","org":"Example"}`, http.StatusOK)
	if !ca.CanSign || ca.Status != "active" {
		t.Fatalf("created CA: %+v", ca)
	}

	leaf := postJSON[certView](t, s.handleCreateCertificate, "POST", "/api/certs", nil,
		`{"name":"web-frontend","ca_id":"`+ca.ID+`","sans":["app.example.com","www.example.com"]}`, http.StatusOK)
	if len(leaf.SANs) != 2 || leaf.CAID != ca.ID {
		t.Fatalf("issued leaf: %+v", leaf)
	}

	stored, err := s.store.CertificateByID(ctx, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := s.sealer.Open(stored.KeyEnc)
	if err != nil {
		t.Fatal(err)
	}
	if err := certs.Verify(stored.CertPEM, mustCA(t, s, ctx, ca.ID).CertPEM); err != nil {
		t.Fatalf("the issued leaf does not verify against its CA: %v", err)
	}

	// A CA with leaves refuses deletion.
	rec := call(s.handleDeleteCA, "DELETE", "/api/certs/cas/"+ca.ID, map[string]string{"id": ca.ID}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a CA with live leaves returned %d; it must refuse", rec.Code)
	}

	// ── phase 1: rotate ─────────────────────────────────────────────────────────
	next := postJSON[caView](t, s.handleRotateCA, "POST", "/api/certs/cas/"+ca.ID+"/rotate",
		map[string]string{"id": ca.ID}, `{"overlap_days":30}`, http.StatusOK)
	if next.Status != "next" || next.RotatesID != ca.ID || next.Overlap == nil {
		t.Fatalf("staged CA: %+v", next)
	}

	// A second rotate while one is staged is refused — internal-ca.sh's guard.
	rec = call(s.handleRotateCA, "POST", "/api/certs/cas/"+ca.ID+"/rotate",
		map[string]string{"id": ca.ID}, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second rotate returned %d; want 409", rec.Code)
	}

	// During overlap the bundle carries BOTH roots, so distribution can begin while
	// nothing has changed for the leaves.
	bundle, err := s.trustBundle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := certs.ParseCerts(bundle); len(got) != 2 {
		t.Fatalf("the overlap bundle holds %d roots; want 2", len(got))
	}
	if err := certs.Verify(stored.CertPEM, bundle); err != nil {
		t.Fatalf("the untouched leaf must verify against the overlap bundle: %v", err)
	}

	// ── phase 2: activate ───────────────────────────────────────────────────────
	// Without confirmation it refuses — activating with an undistributed root is the
	// mistake the two-phase flow exists to prevent.
	rec = call(s.handleActivateCA, "POST", "/api/certs/cas/"+next.ID+"/activate",
		map[string]string{"id": next.ID}, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("activate without confirm returned %d; want 400", rec.Code)
	}

	promoted := postJSON[caView](t, s.handleActivateCA, "POST", "/api/certs/cas/"+next.ID+"/activate",
		map[string]string{"id": next.ID}, `{"confirm":true}`, http.StatusOK)
	if promoted.Status != "active" || promoted.RotatesID != "" {
		t.Fatalf("promoted CA: %+v", promoted)
	}
	old := mustCA(t, s, ctx, ca.ID)
	if old.Status != "retired" || old.OverlapUntil.IsZero() {
		t.Fatalf("the incumbent should be retired with an overlap tail: %+v", old)
	}

	// The leaf was re-signed under the new root — with the SAME key, so consumers that
	// mounted the key file never noticed.
	resigned, err := s.store.CertificateByID(ctx, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resigned.CAID != promoted.ID {
		t.Errorf("the leaf still points at the retired CA")
	}
	if err := certs.Verify(resigned.CertPEM, mustCA(t, s, ctx, promoted.ID).CertPEM); err != nil {
		t.Errorf("the re-signed leaf does not verify against the new root: %v", err)
	}
	keyAfter, err := s.sealer.Open(resigned.KeyEnc)
	if err != nil {
		t.Fatal(err)
	}
	if keyBefore != keyAfter {
		t.Error("activation changed a leaf's private key — renewal must reuse it")
	}
	if err := certs.CheckPair(resigned.CertPEM, keyAfter); err != nil {
		t.Errorf("the re-signed leaf and its key no longer match: %v", err)
	}

	// The retired root stays in the bundle until its overlap tail passes.
	bundle, _ = s.trustBundle(ctx)
	if got, _ := certs.ParseCerts(bundle); len(got) != 2 {
		t.Errorf("right after activation the bundle holds %d roots; want 2 (retired stays through the tail)", len(got))
	}
}

// The generate response is the ONLY place the private half ever exists, and the database
// row it leaves behind must hold nothing that could decrypt a backup.
func TestKeyGenerationNeverStoresThePrivateHalf(t *testing.T) {
	s, ctx := certServer(t)

	rec := call(s.handleCreateKey, "POST", "/api/keys", nil, `{"name":"personal"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp["recipient"], "age1") {
		t.Fatalf("recipient: %q", resp["recipient"])
	}
	if !strings.Contains(resp["identity_file"], "AGE-SECRET-KEY-") {
		t.Fatal("the one-time response must carry the identity file")
	}

	// The stored row: public half only.
	k, err := s.store.EncryptionKeyByID(ctx, resp["id"])
	if err != nil {
		t.Fatal(err)
	}
	if k.Recipient != resp["recipient"] || k.Source != "generated" {
		t.Fatalf("stored key: %+v", k)
	}
	// Nothing anywhere in the row smells like a private key. Belt and braces — the
	// struct has no field for one, but a marshaling bug could smuggle it into a name.
	blob, _ := json.Marshal(k)
	if strings.Contains(string(blob), "AGE-SECRET-KEY-") {
		t.Fatal("the private key reached the database")
	}

	// Importing a private key is refused with an error that says PRIVATE.
	rec = call(s.handleCreateKey, "POST", "/api/keys", nil,
		`{"name":"oops","recipient":"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Fatalf("importing a private key: %d %s", rec.Code, rec.Body)
	}

	// Importing the public recipient works.
	rec = call(s.handleCreateKey, "POST", "/api/keys", nil,
		`{"name":"break-glass","recipient":"`+resp["recipient"]+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("importing a valid recipient: %d %s", rec.Code, rec.Body)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func certServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := config.NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := config.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	return &Server{store: st, sealer: sealer, notify: notify.New(st, fakeSealer{}, log)}, ctx
}

func call(h http.HandlerFunc, method, path string, pathValues map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func postJSON[T any](t *testing.T, h http.HandlerFunc, method, path string, pathValues map[string]string, body string, want int) T {
	t.Helper()
	rec := call(h, method, path, pathValues, body)
	if rec.Code != want {
		t.Fatalf("%s %s returned %d: %s", method, path, rec.Code, rec.Body)
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s response: %v", path, err)
	}
	return out
}

func mustCA(t *testing.T, s *Server, ctx context.Context, id string) *store.CertAuthority {
	t.Helper()
	ca, err := s.store.CertAuthorityByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// The edge resources the installer provisions (CA, certificate, delivery) are marked
// protected, and protected is refused for deletion — the console cannot be made to delete
// its own TLS with a click. The flag must also survive a store round-trip.
func TestProtectedEdgeResourcesRefuseDeletion(t *testing.T) {
	s, ctx := certServer(t)

	ca := &store.CertAuthority{Name: "daffa-edge-ca", CertPEM: "ca", KeyEnc: "k", Status: "active", Protected: true}
	if err := s.store.CreateCertAuthority(ctx, ca); err != nil {
		t.Fatal(err)
	}
	cert := &store.Certificate{Name: "daffa-edge", CAID: ca.ID, SANs: "daffa.internal", CertPEM: "c", KeyEnc: "k", Protected: true}
	if err := s.store.CreateCertificate(ctx, cert); err != nil {
		t.Fatal(err)
	}
	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	dlv := &store.CertDelivery{EnvID: env.ID, CertID: cert.ID, Volume: "daffa-edge-certs", Traefik: true, Protected: true}
	if err := s.store.CreateCertDelivery(ctx, dlv); err != nil {
		t.Fatal(err)
	}

	// The flag round-trips rather than being dropped on write/read.
	if got := mustCA(t, s, ctx, ca.ID); !got.Protected {
		t.Fatal("CA.Protected did not survive a store round-trip")
	}

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		path string
		id   string
	}{
		{"ca", s.handleDeleteCA, "/api/certs/cas/" + ca.ID, ca.ID},
		{"cert", s.handleDeleteCertificate, "/api/certs/" + cert.ID, cert.ID},
		{"delivery", s.handleDeleteCertDelivery, "/api/certs/deliveries/" + dlv.ID, dlv.ID},
	} {
		rec := call(tc.h, "DELETE", tc.path, map[string]string{"id": tc.id}, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: delete returned %d, want 400", tc.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "protected") {
			t.Errorf("%s: refusal body %q lacks 'protected'", tc.name, rec.Body.String())
		}
	}
}
