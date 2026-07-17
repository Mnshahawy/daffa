// Package config loads Daffa's configuration from the environment.
//
// Nothing here is specific to any deployment: issuers, hostnames, sockets and
// registries are all configuration. A bare `daffa serve` with no environment set
// must come up with SQLite and local auth, which is what makes the binary useful
// to someone who is not us.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr    string // listen address
	DataDir string // default parent for the SQLite file and other state
	DBURL   string // sqlite://<path> | postgres://…  (empty → SQLite in DataDir)

	// MasterKey decrypts secrets at rest (registry passwords, stack env values…).
	// Generated into DataDir on first run if absent.
	MasterKey []byte

	DockerHost string // local Docker endpoint, e.g. unix:///var/run/docker.sock

	// LocalAuth allows username/password sign-in. It is also the bootstrap path and the
	// way back in when an identity provider is misconfigured, so turning it off is a
	// decision to depend entirely on the IdP plus break-glass.
	LocalAuth bool

	SessionTTL time.Duration

	// SecureCookie marks the session cookie Secure + __Host-. It must be off for a
	// plain-http:// localhost dev run (the browser would drop the cookie) and on
	// everywhere real, where Traefik terminates TLS in front of us.
	SecureCookie bool

	// TrustProxy says a trusted reverse proxy sits directly in front of Daffa and sets
	// X-Forwarded-For. Only then is that header believed — and only its RIGHTMOST entry, the
	// address the trusted proxy actually saw. Off by default, because a client can send any
	// X-Forwarded-For it likes: trusting it unconditionally lets an attacker rotate the header
	// to dodge the login rate limiter and forge the source IP in the audit log. When off, the
	// peer's RemoteAddr is used — correct for a direct connection, and the proxy's own address
	// (honest, if less useful) when one is present but not trusted.
	TrustProxy bool

	// SystemNetworks and SystemVolumes name Docker resources this deployment depends on —
	// Daffa's own database volume, the edge-certificate volume, the networks the console
	// sits on. They are marked `system` in the API and refused for removal, the same way
	// bridge/host/none already are, so nobody deletes the console's own plumbing from
	// inside the console. Built-in system networks (bridge/host/none) are always protected;
	// these extend the set. The installer populates them for the stack it writes.
	SystemNetworks []string
	SystemVolumes  []string
}

// Identity providers are NOT configured here. They live in the database — there may be
// several, they are administered from the UI, and their client secrets are sealed with the
// master key like every other secret Daffa stores. See docs/rbac.md.
//
// Roles are not configured here either. They are rows, and what they grant is a capability
// mask; see internal/caps.

func Load() (*Config, error) {
	c := &Config{
		Addr:       env("DAFFA_ADDR", ":8080"),
		DataDir:    env("DAFFA_DATA_DIR", "/var/lib/daffa"),
		DBURL:      os.Getenv("DAFFA_DB_URL"),
		DockerHost: env("DAFFA_DOCKER_HOST", "unix:///var/run/docker.sock"),
		LocalAuth:  envBool("DAFFA_LOCAL_AUTH", true),
		SessionTTL: envDuration("DAFFA_SESSION_TTL", 12*time.Hour),
		// Safe by default; DAFFA_SECURE_COOKIE=false is the documented dev escape.
		SecureCookie: envBool("DAFFA_SECURE_COOKIE", true),
		// Off by default: believing X-Forwarded-For from an untrusted peer is a spoofing hole.
		// Turn on when Daffa sits behind a reverse proxy you control (Traefik, nginx, …).
		TrustProxy: envBool("DAFFA_TRUST_PROXY", false),

		SystemNetworks: envList("DAFFA_SYSTEM_NETWORKS"),
		SystemVolumes:  envList("DAFFA_SYSTEM_VOLUMES"),
	}

	if c.DBURL == "" {
		c.DBURL = "sqlite://" + c.DataDir + "/daffa.db"
	}

	key, err := loadMasterKey(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.MasterKey = key

	// A superseded variable is worth one loud line, because the failure it prevents is
	// "SSO silently stopped working after an upgrade and nobody could log in".
	for _, k := range []string{
		"DAFFA_OIDC_ISSUER", "DAFFA_OIDC_CLIENT_ID", "DAFFA_OIDC_CLIENT_SECRET",
		"DAFFA_OIDC_REDIRECT_URL", "DAFFA_OIDC_SCOPES", "DAFFA_OIDC_ROLES_CLAIM",
		"DAFFA_OIDC_ROLE_MAP",
	} {
		if os.Getenv(k) != "" {
			return nil, fmt.Errorf("config: %s is no longer used — identity providers are configured "+
				"in the database now (Settings → Authentication), and there may be more than one. "+
				"Sign in locally and add the provider there, then remove this variable", k)
		}
	}
	return c, nil
}

// loadMasterKey reads (or on first run creates) the 32-byte secretbox key.
// DAFFA_MASTER_KEY_FILE overrides the default location so it can come from a
// mounted secret.
func loadMasterKey(dataDir string) ([]byte, error) {
	path := env("DAFFA_MASTER_KEY_FILE", dataDir+"/master.key")

	b, err := os.ReadFile(path)
	if err == nil {
		// We write this 0o600, but a key restored from a backup, copied by hand, or mounted from
		// a secret manager can arrive group- or world-readable. That is the whole security of the
		// at-rest seal sitting in a file anyone on the box can read, so refuse it loudly rather
		// than load it silently. A bit-mode override (a read-only mount) is the caller's to fix.
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("config: master key at %s is readable by group or others "+
				"(mode %04o); tighten it with: chmod 600 %s", path, info.Mode().Perm(), path)
		}
		key, err := parseKey(b)
		if err != nil {
			return nil, fmt.Errorf("config: master key at %s: %w", path, err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config: reading master key: %w", err)
	}

	key, err := NewMasterKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("config: creating data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(encodeKey(key)), 0o600); err != nil {
		return nil, fmt.Errorf("config: writing master key: %w", err)
	}
	return key, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envList splits a comma-separated variable into trimmed, non-empty entries.
// "a, b ,,c" → ["a","b","c"]; unset → nil.
func envList(k string) []string {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
