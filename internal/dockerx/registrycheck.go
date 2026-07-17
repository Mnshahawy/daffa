package dockerx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CheckRegistry proves a registry credential works before it is saved — the same "do not store a
// configuration that is a future 3am surprise" rule a storage target follows. A wrong registry
// password otherwise sits silent until a deploy tries to pull a private image and fails.
//
// It speaks the Docker Registry v2 auth dance: hit /v2/, and if the registry answers 401 with a
// Bearer challenge (Docker Hub, GHCR, most hosted registries) follow the challenge to its token
// endpoint with the credentials; if it answers with a Basic challenge, send the credentials
// straight to /v2/. A 200 either way means the credentials are good.
func CheckRegistry(ctx context.Context, host, username, password string) error {
	base := registryBaseURL(host)
	client := &http.Client{Timeout: 12 * time.Second}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := registryGet(ctx, client, base+"/v2/", "")
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", host, err)
	}
	resp.Body.Close()

	// Anonymous access is allowed, so the credentials are trivially fine (and may be unnecessary,
	// but that is the user's call, not an error).
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("%s answered %s, which is not how a Docker registry replies — is the host right?",
			host, resp.Status)
	}

	challenge := resp.Header.Get("Www-Authenticate")
	switch {
	case strings.HasPrefix(strings.ToLower(challenge), "bearer"):
		return checkBearer(ctx, client, challenge, username, password)
	case strings.HasPrefix(strings.ToLower(challenge), "basic"):
		return checkBasic(ctx, client, base, username, password)
	default:
		return fmt.Errorf("%s asked for an authentication scheme Daffa does not recognise (%q)", host, challenge)
	}
}

// checkBearer proves a credential against a Bearer challenge — it only needs a token to come
// back. Tag operations need the token itself, so the work lives in bearerToken.
func checkBearer(ctx context.Context, client *http.Client, challenge, username, password string) error {
	_, err := bearerToken(ctx, client, challenge, username, password)
	return err
}

// bearerToken follows a `Bearer realm="…",service="…",scope="…"` challenge to its token endpoint
// and returns the token. It forwards the challenge's `scope` verbatim — a bare /v2/ probe carries
// none, but a manifest request carries `repository:<repo>:pull`, and a token minted without that
// scope cannot read the repo. A 200 with a token means the credentials authenticated.
func bearerToken(ctx context.Context, client *http.Client, challenge, username, password string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("the registry's Bearer challenge carried no realm to authenticate against")
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("the registry's token endpoint is not a valid URL: %w", err)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	resp, err := registryGet(ctx, client, u.String(), basicAuth(username, password))
	if err != nil {
		return "", fmt.Errorf("could not reach the registry's token endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// Some registries hand back a 200 with an empty token for a bad-but-anonymous-allowed
		// login. Require an actual token so a wrong password is not read as success.
		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
		switch {
		case body.Token != "":
			return body.Token, nil
		case body.AccessToken != "":
			return body.AccessToken, nil
		default:
			return "", fmt.Errorf("the registry accepted the request but issued no token — the username or password is wrong")
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("the registry rejected the username or password")
	}
	return "", fmt.Errorf("the registry's token endpoint answered %s", resp.Status)
}

func checkBasic(ctx context.Context, client *http.Client, base, username, password string) error {
	resp, err := registryGet(ctx, client, base+"/v2/", basicAuth(username, password))
	if err != nil {
		return fmt.Errorf("could not reach the registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the registry rejected the username or password")
	}
	return fmt.Errorf("the registry answered %s", resp.Status)
}

func registryGet(ctx context.Context, client *http.Client, url, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return client.Do(req)
}

// registryBaseURL turns a registry host into the base URL to probe. Docker's shorthand "docker.io"
// is really registry-1.docker.io; everything else is taken as-is, defaulting to https.
func registryBaseURL(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	if host == "docker.io" || host == "index.docker.io" || host == "registry.docker.io" {
		host = "registry-1.docker.io"
	}
	return "https://" + host
}

func basicAuth(username, password string) string {
	if username == "" && password == "" {
		return ""
	}
	// http.Request.SetBasicAuth without the request: encode it directly.
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// parseChallenge pulls the key="value" pairs out of a WWW-Authenticate header. It is small on
// purpose: the registry challenge is a well-defined, comma-separated list, not arbitrary HTTP
// auth-param grammar, and a full parser would be more surface than the input warrants.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	// Drop the leading scheme word ("Bearer" / "Basic").
	if i := strings.IndexByte(h, ' '); i >= 0 {
		h = h[i+1:]
	}
	for _, part := range splitParams(h) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[key] = val
	}
	return out
}

// splitParams splits on commas that are NOT inside quotes — a realm value can itself contain a
// comma in its query string, and splitting naively would truncate the token URL.
func splitParams(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}
