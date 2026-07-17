// Package cli holds the operator-side commands that talk to a Daffa server over its
// API, rather than to a database directly.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/Mnshahawy/daffa/internal/backups"
)

// Restore reads a snapshot back into a live database.
//
// The division of labour here IS the security design, and it is worth being explicit
// about because the tempting alternative is much worse:
//
//	server  → holds the storage credentials, holds the Docker socket, streams the
//	          ENCRYPTED snapshot down to us, and later pipes a plaintext dump into the
//	          database container. It never sees the private key.
//	this CLI → holds the private key, decrypts locally, and streams plaintext back up.
//
// The tempting alternative is to let the server decrypt: then restore would be a button
// in the web UI. But the server would need the private key, which means the box taking
// the backups could also read them — and an attacker who owned that box would own every
// snapshot ever taken, including the ones from before they got in. The whole point of
// encrypting to a public key is that this cannot happen. So restore is a CLI command,
// and the UI shows you the command rather than asking for your key.
type RestoreOptions struct {
	Server   string
	Job      string
	Snapshot string
	Identity string // path to an age key file; empty for an unencrypted snapshot
	Username string
	Password string
	Token    string // API token; when set, no login round-trip and no password in the environment
	Insecure bool
	Yes      bool // skip the confirmation prompt (for scripts)
	// Wipe empties the volume before a VOLUME restore. Without it the server refuses a
	// non-empty volume — a restore that merges two states of the data is garbage that
	// only shows up later. Meaningless for a database job.
	Wipe bool
}

func Restore(ctx context.Context, o RestoreOptions) error {
	if o.Server == "" || o.Job == "" || o.Snapshot == "" {
		return fmt.Errorf("restore: --server, --job and --snapshot are required")
	}
	o.Server = strings.TrimSuffix(o.Server, "/")

	client, err := login(ctx, o)
	if err != nil {
		return err
	}

	// Resolve the job's name from its id. The confirmation — both the one this CLI asks
	// for and the one the server insists on — is on the NAME, because a name is
	// something a person can recognize and an id is something they will paste without
	// reading.
	jobName, err := jobName(ctx, client, o)
	if err != nil {
		return err
	}

	encrypted := strings.HasSuffix(o.Snapshot, ".age")

	var identity age.Identity
	if encrypted {
		raw, err := readIdentity(o.Identity)
		if err != nil {
			return err
		}
		identity, err = backups.ParseIdentity(raw)
		if err != nil {
			return err
		}
	} else if o.Identity != "" {
		fmt.Fprintln(os.Stderr, "note: this snapshot is not encrypted, so the key is not needed")
	}

	// This overwrites a live database. Make the person say so.
	if !o.Yes {
		fmt.Fprintf(os.Stderr,
			"\nThis will restore\n    %s\ninto the database behind job %q, OVERWRITING what is there now.\n\nType the job name to confirm: ",
			o.Snapshot, jobName)
		var typed string
		_, _ = fmt.Fscanln(os.Stdin, &typed)
		if strings.TrimSpace(typed) != jobName {
			return fmt.Errorf("restore: cancelled")
		}
	}

	fmt.Fprintln(os.Stderr, "downloading…")
	snapshot, err := download(ctx, client, o)
	if err != nil {
		return err
	}
	defer snapshot.Close()

	// Decrypt and decompress HERE, on this machine.
	plain, err := backups.Decrypt(snapshot, encrypted, identity)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "restoring…")
	return upload(ctx, client, o, jobName, plain)
}

// jobName looks up the job so the confirmation can be about something human-readable.
func jobName(ctx context.Context, client *http.Client, o RestoreOptions) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Server+"/api/backups", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("restore: listing backup jobs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("restore: listing backup jobs: %s", apiError(resp))
	}

	var jobs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return "", fmt.Errorf("restore: reading the job list: %w", err)
	}
	for _, j := range jobs {
		if j.ID == o.Job || j.Name == o.Job {
			return j.Name, nil
		}
	}
	return "", fmt.Errorf("restore: no backup job %q — check the id in the console", o.Job)
}

func readIdentity(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf(
			"restore: this snapshot is encrypted (its name ends in .age) — pass --identity with your age private key file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("restore: reading the key file: %w", err)
	}
	return string(b), nil
}

// login authenticates to the Daffa server: an API token rides every request as a bearer
// header, a username/password does one login round-trip and keeps the session cookie.
func login(ctx context.Context, o RestoreOptions) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 0} // no timeout: a restore can take a while
	client.Transport = transport(o.Insecure)

	if o.Token != "" {
		client.Transport = &bearerTransport{token: o.Token, next: client.Transport}
		return client, nil
	}

	if o.Username == "" {
		return nil, fmt.Errorf("restore: --user is required (or set DAFFA_TOKEN)")
	}
	password := o.Password
	if password == "" {
		password, err = promptPassword("Password: ")
		if err != nil {
			return nil, err
		}
	}

	body, _ := json.Marshal(map[string]string{"username": o.Username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Server+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("restore: connecting to %s: %w", o.Server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("restore: sign-in failed: %s", apiError(resp))
	}
	return client, nil
}

// bearerTransport stamps the token onto every request, so the rest of the CLI does not
// know or care which credential it is running under.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(r)
}

func download(ctx context.Context, client *http.Client, o RestoreOptions) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/api/backups/%s/download?key=%s", o.Server, o.Job, urlEscape(o.Snapshot))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("restore: downloading the snapshot: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("restore: could not download the snapshot: %s", apiError(resp))
	}
	return resp.Body, nil
}

func upload(ctx context.Context, client *http.Client, o RestoreOptions, jobName string, dump io.Reader) error {
	url := fmt.Sprintf("%s/api/backups/%s/restore?confirm=%s", o.Server, o.Job, urlEscape(jobName))
	if o.Wipe {
		url += "&wipe=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, dump)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// The server checks the Origin on mutating requests; a CLI has none, which is
	// allowed. Say who we are anyway, so the audit log is legible.
	req.Header.Set("User-Agent", "daffa-cli")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("restore: streaming the dump: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Output  string `json:"output"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if resp.StatusCode != http.StatusOK {
		if out.Output != "" {
			fmt.Fprintln(os.Stderr, "\n--- database output ---\n"+out.Output)
		}
		return fmt.Errorf("restore: %s", firstNonEmpty(out.Message, resp.Status))
	}

	fmt.Fprintf(os.Stderr, "restored in %s\n", time.Since(start).Round(time.Second))
	if out.Output != "" {
		fmt.Fprintln(os.Stderr, "\n--- database output ---\n"+out.Output)
	}
	return nil
}

func apiError(resp *http.Response) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return firstNonEmpty(e.Message, resp.Status)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
