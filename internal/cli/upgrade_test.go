package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease is a GitHub stand-in serving one release: the latest-release API
// answer, one tar.gz asset for linux/amd64 carrying newBinary, and its SHA256SUMS.
// downloads counts asset fetches, so tests can pin that up-to-date and --check paths
// never download.
func fakeRelease(t *testing.T, tag string, newBinary []byte, corruptSums bool) (srv *httptest.Server, downloads *int) {
	t.Helper()
	n := 0
	member := "daffa_" + strings.TrimPrefix(tag, "v") + "_linux_amd64"

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member + "/daffa", Mode: 0o755, Size: int64(len(newBinary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(newBinary); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	sum := sha256.Sum256(archive.Bytes())
	digest := hex.EncodeToString(sum[:])
	if corruptSums {
		digest = strings.Repeat("0", 64)
	}
	sums := fmt.Sprintf("%s  %s.tar.gz\n", digest, member)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+upgradeRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, tag)
	})
	mux.HandleFunc("/"+upgradeRepo+"/releases/download/"+tag+"/"+member+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write(archive.Bytes())
	})
	mux.HandleFunc("/"+upgradeRepo+"/releases/download/"+tag+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sums)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &n
}

func upgradeTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "daffa")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestUpgradeReplacesTheBinary(t *testing.T) {
	srv, downloads := fakeRelease(t, "v1.2.3", []byte("NEW BINARY"), false)
	target := upgradeTarget(t)
	var out strings.Builder

	err := Upgrade(context.Background(), UpgradeOptions{
		Current: "v1.0.0", APIBase: srv.URL, DownloadBase: srv.URL,
		Target: target, OS: "linux", Arch: "amd64", Out: &out,
	})
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "NEW BINARY" {
		t.Fatalf("target after upgrade: %q, %v", got, err)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode %v, want 0755", info.Mode().Perm())
	}
	if *downloads != 1 {
		t.Fatalf("asset downloaded %d times, want 1", *downloads)
	}
	if !strings.Contains(out.String(), "upgraded daffa v1.0.0 → v1.2.3") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestUpgradeUpToDateDownloadsNothing(t *testing.T) {
	srv, downloads := fakeRelease(t, "v1.2.3", []byte("NEW"), false)
	target := upgradeTarget(t)
	var out strings.Builder

	err := Upgrade(context.Background(), UpgradeOptions{
		Current: "v1.2.3", APIBase: srv.URL, DownloadBase: srv.URL,
		Target: target, OS: "linux", Arch: "amd64", Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if *downloads != 0 {
		t.Fatal("an up-to-date binary downloaded the asset")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("target changed: %q", got)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestUpgradeCheckReportsWithoutInstalling(t *testing.T) {
	srv, downloads := fakeRelease(t, "v2.0.0", []byte("NEW"), false)
	target := upgradeTarget(t)
	var out strings.Builder

	err := Upgrade(context.Background(), UpgradeOptions{
		Current: "v1.0.0", Check: true, APIBase: srv.URL, DownloadBase: srv.URL,
		Target: target, OS: "linux", Arch: "amd64", Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if *downloads != 0 {
		t.Fatal("--check downloaded the asset")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("--check changed the target: %q", got)
	}
	if !strings.Contains(out.String(), "upgrade available: v1.0.0 → v2.0.0") {
		t.Fatalf("output: %q", out.String())
	}
}

// A bad checksum must refuse and leave the incumbent untouched — that is the entire
// point of fetching SHA256SUMS.
func TestUpgradeChecksumMismatchInstallsNothing(t *testing.T) {
	srv, _ := fakeRelease(t, "v1.2.3", []byte("NEW"), true)
	target := upgradeTarget(t)

	err := Upgrade(context.Background(), UpgradeOptions{
		Current: "v1.0.0", APIBase: srv.URL, DownloadBase: srv.URL,
		Target: target, OS: "linux", Arch: "amd64", Out: &strings.Builder{},
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum refusal", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("a failed verification still replaced the binary: %q", got)
	}
}

// A from-source build has no comparable version; refusing beats clobbering it.
func TestUpgradeRefusesDevBuilds(t *testing.T) {
	err := Upgrade(context.Background(), UpgradeOptions{Current: "dev", Out: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "version stamp") {
		t.Fatalf("err = %v, want the dev-build refusal", err)
	}
}

func TestParseSemver(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"v1.2.3", true}, {"1.2.3", true}, {"dev", false}, {"v1.2", false},
		{"v1.2.3-rc1", false}, {"", false}, {"v1.-2.3", false},
	} {
		if _, ok := parseSemver(tc.in); ok != tc.ok {
			t.Errorf("parseSemver(%q) ok = %t, want %t", tc.in, ok, tc.ok)
		}
	}
	if !newer([3]int{1, 2, 3}, [3]int{1, 2, 2}) || newer([3]int{1, 2, 3}, [3]int{1, 2, 3}) || newer([3]int{1, 2, 3}, [3]int{2, 0, 0}) {
		t.Error("newer() ordering is wrong")
	}
}
