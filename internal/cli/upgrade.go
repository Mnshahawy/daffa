package cli

// Self-update. The one command in this package that talks to GitHub rather than to a
// Daffa server: it reads the PUBLIC releases feed, compares the latest stable tag with
// the version stamped into this binary, downloads the platform's release asset,
// verifies it against the release's SHA256SUMS, and swaps the executable in place.
//
// It upgrades THE BINARY IT IS, nothing else. A server running from the container
// image upgrades by pulling a new image tag — replacing a binary inside a container
// outlives nothing — so this exists for the other homes the binary has: the operator
// laptop running `daffa plan`/`daffa apply`, the CI runner, the bare-metal install.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// upgradeRepo is where releases live. A constant, not a flag: an upgrade command that
// can be pointed at an arbitrary repository is a downloader for someone else's code.
const upgradeRepo = "Mnshahawy/daffa"

// UpgradeOptions parameterises Upgrade. The empty value is the real thing; the
// overridable endpoints and target exist for tests, which must never touch GitHub or
// replace the test binary.
type UpgradeOptions struct {
	Current string // the running version ("dev" when unstamped)
	Check   bool   // report only; download nothing, replace nothing

	APIBase      string    // default https://api.github.com
	DownloadBase string    // default https://github.com
	Target       string    // executable to replace; default: this one
	OS, Arch     string    // default: runtime.GOOS / runtime.GOARCH
	Out          io.Writer // default os.Stdout
}

// Upgrade brings the binary to the latest stable release, if there is one.
func Upgrade(ctx context.Context, o UpgradeOptions) error {
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.DownloadBase == "" {
		o.DownloadBase = "https://github.com"
	}
	if o.OS == "" {
		o.OS, o.Arch = runtime.GOOS, runtime.GOARCH
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}

	current, ok := parseSemver(o.Current)
	if !ok {
		return fmt.Errorf("upgrade: this build reports version %q, which is not a release version — "+
			"it was built from source without a version stamp. Install a release binary once "+
			"(https://github.com/%s/releases) and it can keep itself current from then on", o.Current, upgradeRepo)
	}

	// Downloads are big and GitHub can be slow; the timeout bounds a hang, not a
	// download.
	client := &http.Client{Timeout: 5 * time.Minute}

	tag, err := latestReleaseTag(ctx, client, o.APIBase)
	if err != nil {
		return err
	}
	latest, ok := parseSemver(tag)
	if !ok {
		return fmt.Errorf("upgrade: the latest release is tagged %q, which is not a version this can compare", tag)
	}

	switch {
	case !newer(latest, current):
		fmt.Fprintf(o.Out, "daffa %s is up to date (latest release is %s)\n", o.Current, tag)
		return nil
	case o.Check:
		fmt.Fprintf(o.Out, "upgrade available: %s → %s — run `daffa upgrade`\n", o.Current, tag)
		return nil
	}

	asset := fmt.Sprintf("daffa_%s_%s_%s", strings.TrimPrefix(tag, "v"), o.OS, o.Arch)
	ext := ".tar.gz"
	if o.OS == "windows" {
		ext = ".zip"
	}
	base := o.DownloadBase + "/" + upgradeRepo + "/releases/download/" + tag + "/"

	fmt.Fprintf(o.Out, "downloading %s%s…\n", asset, ext)
	archive, err := fetch(ctx, client, base+asset+ext)
	if err != nil {
		return fmt.Errorf("upgrade: downloading %s%s: %w", asset, ext, err)
	}

	// The checksum file rides the same release; verifying against it turns a
	// truncated or tampered download into a refusal instead of a broken binary.
	sums, err := fetch(ctx, client, base+"SHA256SUMS")
	if err != nil {
		return fmt.Errorf("upgrade: downloading SHA256SUMS: %w", err)
	}
	if err := verifySum(sums, asset+ext, archive); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}

	bin := "daffa"
	if o.OS == "windows" {
		bin = "daffa.exe"
	}
	payload, err := extract(archive, o.OS == "windows", asset+"/"+bin)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}

	target := o.Target
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("upgrade: locating this executable: %w", err)
		}
		// Replace the real file, not a symlink pointing at it — swapping the link
		// would strand the installation the link was managing.
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		target = exe
	}

	if err := install(payload, target, o.OS == "windows"); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	fmt.Fprintf(o.Out, "upgraded daffa %s → %s (%s)\n", o.Current, tag, target)
	return nil
}

func latestReleaseTag(ctx context.Context, client *http.Client, apiBase string) (string, error) {
	url := apiBase + "/repos/" + upgradeRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "daffa-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upgrade: reaching the releases API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errors.New("upgrade: the repository has no releases yet")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upgrade: the releases API answered %s", resp.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("upgrade: reading the release: %w", err)
	}
	return release.TagName, nil
}

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "daffa-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	// A release asset is tens of megabytes; anything approaching this bound is not
	// one of ours.
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// verifySum checks the archive against its SHA256SUMS line. The file's format is
// sha256sum's own: "<hex>  <name>" per line.
func verifySum(sums []byte, name string, archive []byte) error {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		got := sha256.Sum256(archive)
		if hex.EncodeToString(got[:]) != strings.ToLower(fields[0]) {
			return fmt.Errorf("checksum mismatch for %s — the download is corrupt or tampered with; nothing was installed", name)
		}
		return nil
	}
	return fmt.Errorf("SHA256SUMS carries no entry for %s", name)
}

// extract pulls the binary out of the release archive (tar.gz, or zip on Windows).
func extract(archive []byte, isZip bool, member string) ([]byte, error) {
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		for _, f := range zr.File {
			if f.Name != member {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		return nil, fmt.Errorf("the archive carries no %s", member)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive carries no %s", member)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == member {
			return io.ReadAll(tr)
		}
	}
}

// install writes the new binary beside the target and renames it into place — same
// directory, so the rename is atomic on one filesystem. Windows cannot rename over a
// running executable, so the incumbent is moved aside first; the leftover .old is
// removed best-effort (it may be locked exactly while this process still runs).
func install(payload []byte, target string, windows bool) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".daffa-upgrade-*")
	if err != nil {
		return fmt.Errorf("writing beside %s: %w", target, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}

	if windows {
		old := target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return fmt.Errorf("moving the current binary aside: %w", err)
		}
		if err := os.Rename(tmpName, target); err != nil {
			_ = os.Rename(old, target) // put the incumbent back; best effort
			cleanup()
			return fmt.Errorf("installing the new binary: %w", err)
		}
		_ = os.Remove(old)
		return nil
	}

	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return fmt.Errorf("installing the new binary: %w", err)
	}
	return nil
}

// parseSemver reads a vX.Y.Z (or X.Y.Z) tag. Anything else — "dev", an rc suffix —
// is not comparable, and the caller says why.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
