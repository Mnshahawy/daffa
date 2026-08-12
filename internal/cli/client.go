package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
)

// connectOptions is the connection half every remote command shares: where the
// server is and how to prove who is calling.
type connectOptions struct {
	server   string
	username string
	password string
	token    string
	insecure bool
}

// connect authenticates to the Daffa server: an API token rides every request as a
// bearer header, a username/password does one login round-trip and keeps the session
// cookie. Errors carry no command prefix — the caller wraps them with its own.
func connect(ctx context.Context, o connectOptions) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 0} // no timeout: some commands stream for a while
	client.Transport = transport(o.insecure)

	if o.token != "" {
		client.Transport = &bearerTransport{token: o.token, next: client.Transport}
		return client, nil
	}

	if o.username == "" {
		return nil, fmt.Errorf("--user is required (or set DAFFA_TOKEN)")
	}
	password := o.password
	if password == "" {
		password, err = promptPassword("Password: ")
		if err != nil {
			return nil, err
		}
	}

	body, _ := json.Marshal(map[string]string{"username": o.username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.server+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", o.server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sign-in failed: %s", apiError(resp))
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
