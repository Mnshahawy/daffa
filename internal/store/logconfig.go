package store

// Default container logging for deployed stacks: a global default and an optional
// per-host override, injected into services that do not declare their own logging at
// deploy time. See docs/stacks.md — retention is Docker's own rotation options, because
// Daffa cannot reach a host's daemon.json.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// LogConfig is a default logging block for deployed services: Docker's driver name and
// its string->string options. Unset is a normal state — nothing is injected, and the
// daemon's own default applies.
type LogConfig struct {
	Driver    string            `json:"driver"`
	Opts      map[string]string `json:"opts"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// ErrInvalidLogConfig is every way a logging config can be wrong. One sentinel because
// they share an answer: the REQUEST is wrong, so the API owes a 400 with the sentence.
var ErrInvalidLogConfig = errors.New("store: not a logging configuration a deploy can carry")

// badLogConfig is the monitors' badRule technique: satisfies errors.Is on the sentinel
// while the message stays a clean sentence for the person who typed it.
type badLogConfig struct{ msg string }

func (e badLogConfig) Error() string        { return e.msg }
func (e badLogConfig) Is(target error) bool { return target == ErrInvalidLogConfig }

func invalidLog(format string, a ...any) error {
	return badLogConfig{msg: fmt.Sprintf(format, a...)}
}

// Free-form on purpose: Docker accepts plugin drivers (loki, fluentd, …) we cannot
// enumerate, and a wrong name fails the deploy with a legible runner log — so validation
// is shape-only, not an allowlist.
var logDriverRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

const (
	maxLogOpts     = 32
	maxLogKeyLen   = 128
	maxLogValueLen = 512
)

func (c *LogConfig) validate() error {
	if strings.TrimSpace(c.Driver) == "" {
		return invalidLog("A log driver is required — json-file and local are the ones that rotate.")
	}
	if len(c.Driver) > maxLogKeyLen || !logDriverRe.MatchString(c.Driver) {
		return invalidLog("%q is not a log driver name Docker would accept.", c.Driver)
	}
	if len(c.Opts) > maxLogOpts {
		return invalidLog("%d options is more than a logging block carries — the limit is %d.",
			len(c.Opts), maxLogOpts)
	}
	for k, v := range c.Opts {
		if k == "" || len(k) > maxLogKeyLen || strings.IndexFunc(k, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) >= 0 {
			return invalidLog("%q is not an option key — keys are words like max-size, without spaces.", k)
		}
		if len(v) > maxLogValueLen || strings.ContainsAny(v, "\n\r") {
			return invalidLog("The value of %q cannot span lines or exceed %d characters.", k, maxLogValueLen)
		}
	}
	return nil
}

const logConfigCols = `driver, opts, updated_at`

// GlobalLogConfig reads the fleet-wide default. Never configured returns (nil, nil):
// unlike monitor settings there is no default worth inventing — "inject nothing" is a
// deliberate, visible state, not a gap to paper over.
func (s *Store) GlobalLogConfig(ctx context.Context) (*LogConfig, error) {
	return scanLogConfig(s.queryRow(ctx,
		`SELECT `+logConfigCols+` FROM log_settings WHERE id = 'logging'`))
}

func (s *Store) SaveGlobalLogConfig(ctx context.Context, c *LogConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	opts, err := marshalLogOpts(c.Opts)
	if err != nil {
		return err
	}
	// Stamped here and written back onto the struct, so the handler returns the row as it
	// now IS rather than with a zero time where the timestamp should be.
	c.UpdatedAt = now()

	_, err = s.exec(ctx, `
        INSERT INTO log_settings (id, driver, opts, updated_at)
        VALUES ('logging', ?, ?, ?)
        ON CONFLICT (id) DO UPDATE SET
            driver = EXCLUDED.driver,
            opts = EXCLUDED.opts,
            updated_at = EXCLUDED.updated_at`,
		c.Driver, opts, ts(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: saving the global log config: %w", err)
	}
	return nil
}

// DeleteGlobalLogConfig unsets the fleet default. Idempotent: deleting what is already
// unset is the state the caller wanted, not an error.
func (s *Store) DeleteGlobalLogConfig(ctx context.Context) error {
	if _, err := s.exec(ctx, `DELETE FROM log_settings WHERE id = 'logging'`); err != nil {
		return fmt.Errorf("store: clearing the global log config: %w", err)
	}
	return nil
}

// EnvLogConfig reads one host's override. (nil, nil) when the host follows the global
// default.
func (s *Store) EnvLogConfig(ctx context.Context, envID string) (*LogConfig, error) {
	return scanLogConfig(s.queryRow(ctx,
		`SELECT `+logConfigCols+` FROM env_log_configs WHERE env_id = ?`, envID))
}

func (s *Store) SaveEnvLogConfig(ctx context.Context, envID string, c *LogConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	opts, err := marshalLogOpts(c.Opts)
	if err != nil {
		return err
	}
	c.UpdatedAt = now()

	_, err = s.exec(ctx, `
        INSERT INTO env_log_configs (env_id, driver, opts, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT (env_id) DO UPDATE SET
            driver = EXCLUDED.driver,
            opts = EXCLUDED.opts,
            updated_at = EXCLUDED.updated_at`,
		envID, c.Driver, opts, ts(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: saving the log config for %s: %w", envID, err)
	}
	return nil
}

// DeleteEnvLogConfig reverts a host to the global default. Idempotent for the same
// reason as the global delete.
func (s *Store) DeleteEnvLogConfig(ctx context.Context, envID string) error {
	if _, err := s.exec(ctx, `DELETE FROM env_log_configs WHERE env_id = ?`, envID); err != nil {
		return fmt.Errorf("store: clearing the log config for %s: %w", envID, err)
	}
	return nil
}

// EffectiveLogConfig is what a deploy to this host injects: the host's override if there
// is one, else the global default, else nil. The precedence lives here, exactly once —
// the deploy path calls this and nothing else.
func (s *Store) EffectiveLogConfig(ctx context.Context, envID string) (*LogConfig, error) {
	c, err := s.EnvLogConfig(ctx, envID)
	if err != nil || c != nil {
		return c, err
	}
	return s.GlobalLogConfig(ctx)
}

func marshalLogOpts(opts map[string]string) (string, error) {
	if len(opts) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("store: encoding log options: %w", err)
	}
	return string(b), nil
}

func scanLogConfig(row scanner) (*LogConfig, error) {
	var (
		c         LogConfig
		opts      string
		updatedAt string
	)
	err := row.Scan(&c.Driver, &opts, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading a log config: %w", err)
	}
	if err := json.Unmarshal([]byte(opts), &c.Opts); err != nil {
		return nil, fmt.Errorf("store: decoding log options: %w", err)
	}
	c.UpdatedAt = parseTS(updatedAt)
	return &c, nil
}
