package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── registries ──────────────────────────────────────────────────────────────────

type Registry struct {
	ID          string
	Name        string
	URL         string
	Username    string
	PasswordEnc string // sealed; only the runner ever sees the plaintext
	CreatedAt   time.Time
}

func (s *Store) CreateRegistry(ctx context.Context, r *Registry) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	r.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO registries (id, name, url, username, password_enc, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.URL, r.Username, r.PasswordEnc, ts(r.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: creating registry: %w", err)
	}
	return nil
}

func (s *Store) ListRegistries(ctx context.Context) ([]*Registry, error) {
	rows, err := s.query(ctx, `SELECT id, name, url, username, password_enc, created_at
        FROM registries ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing registries: %w", err)
	}
	defer rows.Close()

	var out []*Registry
	for rows.Next() {
		var r Registry
		var createdAt string
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Username, &r.PasswordEnc, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(createdAt)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) RegistryByID(ctx context.Context, id string) (*Registry, error) {
	var r Registry
	var createdAt string
	err := s.queryRow(ctx, `SELECT id, name, url, username, password_enc, created_at
        FROM registries WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.URL, &r.Username, &r.PasswordEnc, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTS(createdAt)
	return &r, nil
}

func (s *Store) DeleteRegistry(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM registries WHERE id = ?`, id)
	return err
}

// ── stacks ──────────────────────────────────────────────────────────────────────

type Stack struct {
	ID    string
	EnvID string
	// NodeID is PLACEMENT: which node the containers land on. Empty means "the environment
	// decides" — a standalone environment has one node, and a swarm stack is placed by the
	// scheduler, which is the entire point of Swarm. It is set only for a compose stack pinned to
	// one node of a swarm. Engine (how the file is applied) and placement (where it runs) are
	// different questions; see docs/swarm.md §6.
	NodeID string
	Name   string
	// Engine decides which commands apply this stack and which actions it even has.
	// It used to be implicit — see stacks.Engine.
	Engine string // compose | swarm
	// GroupName is a label the list collapses under. Not a hierarchy.
	GroupName  string
	SourceKind string // git | inline
	GitURL     string
	GitRef     string
	GitPath    string
	// GitCredentialID is empty for a public repository.
	GitCredentialID string
	InlineYAML      string
	RegistryID      string

	// What is live, as of the last SUCCESSFUL deploy.
	DeployedHash   string
	DeployedCommit string
	DeployedAt     time.Time

	// AutoDeploy makes a verified push deploy this stack. Off unless someone says so.
	AutoDeploy       bool
	WebhookSecretEnc string
	// WatchPaths is newline-separated globs; empty means just the compose file.
	WatchPaths string

	CreatedAt time.Time
	CreatedBy string

	// LastDeployStatus is the outcome of the most recent `up` or `pull`: ok | failed |
	// cancelled | running, or "" if nobody has ever tried. Read-only; see stackReadCols.
	LastDeployStatus string
}

const stackCols = `id, env_id, node_id, name, engine, group_name, source_kind, git_url, git_ref, git_path,
    git_credential_id, inline_yaml, registry_id, deployed_hash, deployed_commit, deployed_at,
    auto_deploy, webhook_secret_enc, watch_paths, created_at, created_by`

// stackReadCols adds the outcome of the most recent DEPLOY — and only a deploy: `up` and
// `pull` apply the bundle, `stop`/`restart`/`down` act on what is already there.
//
// Without it the UI cannot tell "nobody has ever deployed this" from "somebody tried and it
// failed", and it says the first when it means the second. Which is how a stack that is up and
// serving traffic ends up labelled "never deployed": compose got as far as CREATING the
// container and then failed to start it, a restart started it, and Daffa — which only records a
// deploy on a clean `up` — had nothing to show.
const stackReadCols = stackCols + `,
    (SELECT d.status FROM deployments d
      WHERE d.stack_id = stacks.id AND d.action IN ('up', 'pull')
      ORDER BY d.started_at DESC LIMIT 1)`

func scanStack(sc interface{ Scan(...any) error }) (*Stack, error) {
	var s Stack
	var credID, registryID, deployedAt, createdBy, lastDeploy sql.NullString
	var createdAt string
	var autoDeploy int
	err := sc.Scan(&s.ID, &s.EnvID, &s.NodeID, &s.Name, &s.Engine, &s.GroupName, &s.SourceKind, &s.GitURL,
		&s.GitRef, &s.GitPath, &credID, &s.InlineYAML, &registryID, &s.DeployedHash,
		&s.DeployedCommit, &deployedAt, &autoDeploy, &s.WebhookSecretEnc, &s.WatchPaths,
		&createdAt, &createdBy, &lastDeploy)
	if err != nil {
		return nil, err
	}
	s.LastDeployStatus = lastDeploy.String
	s.GitCredentialID = credID.String
	s.RegistryID, s.CreatedBy = registryID.String, createdBy.String
	s.AutoDeploy = autoDeploy != 0
	s.CreatedAt = parseTS(createdAt)
	if deployedAt.Valid {
		s.DeployedAt = parseTS(deployedAt.String)
	}
	return &s, nil
}

func (s *Store) CreateStack(ctx context.Context, st *Stack) error {
	if st.ID == "" {
		st.ID = NewID()
	}
	st.CreatedAt = now()
	_, err := s.exec(ctx, `INSERT INTO stacks (`+stackCols+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', NULL, 0, '', '', ?, ?)`,
		st.ID, st.EnvID, st.NodeID, st.Name, st.Engine, st.GroupName, st.SourceKind, st.GitURL, st.GitRef,
		st.GitPath, nullStr(st.GitCredentialID), st.InlineYAML, nullStr(st.RegistryID),
		ts(st.CreatedAt), nullStr(st.CreatedBy))
	if err != nil {
		return fmt.Errorf("store: creating stack: %w", err)
	}
	return nil
}

// SetStackAutoDeploy stores the opt-in and what it watches.
func (s *Store) SetStackAutoDeploy(ctx context.Context, stackID string, enabled bool, watchPaths string) error {
	_, err := s.exec(ctx, `UPDATE stacks SET auto_deploy = ?, watch_paths = ? WHERE id = ?`,
		boolInt(enabled), watchPaths, stackID)
	return err
}

// SetStackWebhookSecret stores the sealed HMAC secret. Called when auto-deploy is first
// enabled and whenever the secret is rotated.
func (s *Store) SetStackWebhookSecret(ctx context.Context, stackID, sealed string) error {
	_, err := s.exec(ctx, `UPDATE stacks SET webhook_secret_enc = ? WHERE id = ?`, sealed, stackID)
	return err
}

func (s *Store) UpdateStackSource(ctx context.Context, st *Stack) error {
	_, err := s.exec(ctx, `UPDATE stacks SET engine = ?, group_name = ?, source_kind = ?,
        git_url = ?, git_ref = ?, git_path = ?, git_credential_id = ?, inline_yaml = ?,
        registry_id = ? WHERE id = ?`,
		st.Engine, st.GroupName, st.SourceKind, st.GitURL, st.GitRef, st.GitPath,
		nullStr(st.GitCredentialID), st.InlineYAML, nullStr(st.RegistryID), st.ID)
	return err
}

func (s *Store) StackByID(ctx context.Context, id string) (*Stack, error) {
	st, err := scanStack(s.queryRow(ctx, `SELECT `+stackReadCols+` FROM stacks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return st, err
}

// ListStacks returns the stacks on hosts the caller may see. global means fleet-wide; envs
// is the set of hosts they hold stacks.view on.
//
// A stack on a host you hold nothing on is not yours to know about — not even its name,
// which in a compose project is usually also the name of a customer or a service.
func (s *Store) ListStacks(ctx context.Context, global bool, envs []string) ([]*Stack, error) {
	where, args := envIn(global, envs)
	if where == neverMatches {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+stackReadCols+` FROM stacks`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing stacks: %w", err)
	}
	defer rows.Close()

	var out []*Stack
	for rows.Next() {
		st, err := scanStack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) DeleteStack(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM stacks WHERE id = ?`, id)
	return err
}

// MarkStackDeployed records what is now live. Only a SUCCESSFUL deploy calls this, so "the
// source changed since the last deploy" stays truthful after a failure.
//
// The commit is stored alongside the hash because they answer different questions. The hash
// answers "has the source moved since?"; only the commit answers "which commit is running?",
// and that is the one an operator actually asks.
func (s *Store) MarkStackDeployed(ctx context.Context, stackID, hash, commit string) error {
	_, err := s.exec(ctx, `UPDATE stacks SET deployed_hash = ?, deployed_commit = ?,
        deployed_at = ? WHERE id = ?`, hash, commit, ts(now()), stackID)
	return err
}

// ── stack env vars ──────────────────────────────────────────────────────────────

type StackEnv struct {
	Key      string
	ValueEnc string
	IsSecret bool
}

func (s *Store) SetStackEnv(ctx context.Context, stackID string, vars []StackEnv) error {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: setting stack env: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Replace wholesale: the UI edits the set, not individual rows, and a partial
	// update would leave a deleted variable behind.
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM stack_envs WHERE stack_id = ?`), stackID); err != nil {
		return err
	}
	for _, v := range vars {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO stack_envs (stack_id, k, v_enc, is_secret) VALUES (?, ?, ?, ?)`),
			stackID, v.Key, v.ValueEnc, boolInt(v.IsSecret)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StackEnv(ctx context.Context, stackID string) ([]StackEnv, error) {
	rows, err := s.query(ctx, `SELECT k, v_enc, is_secret FROM stack_envs WHERE stack_id = ? ORDER BY k`, stackID)
	if err != nil {
		return nil, fmt.Errorf("store: reading stack env: %w", err)
	}
	defer rows.Close()

	var out []StackEnv
	for rows.Next() {
		var v StackEnv
		var secret int
		if err := rows.Scan(&v.Key, &v.ValueEnc, &secret); err != nil {
			return nil, err
		}
		v.IsSecret = secret != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// ── stack secrets ─────────────────────────────────────────────────────────────────

// StackSecret is the file-shaped twin of a StackEnv: sealed material a stack carries,
// delivered as a file the deploy writes into the bundle. Content is write-only through the
// API — the plaintext exists only in the runner. See docs/secrets.md.
type StackSecret struct {
	Name       string
	ContentEnc string
}

// SetStackSecrets replaces a stack's secrets wholesale, like SetStackEnv: the UI edits the
// set, and a partial update would strand a deleted secret. "Unchanged" (an empty content on
// submit) is resolved to the stored row by the handler before it reaches here.
func (s *Store) SetStackSecrets(ctx context.Context, stackID string, secs []StackSecret) error {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: setting stack secrets: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM stack_secrets WHERE stack_id = ?`), stackID); err != nil {
		return err
	}
	for _, sec := range secs {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO stack_secrets (stack_id, name, content_enc) VALUES (?, ?, ?)`),
			stackID, sec.Name, sec.ContentEnc); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StackSecrets(ctx context.Context, stackID string) ([]StackSecret, error) {
	rows, err := s.query(ctx, `SELECT name, content_enc FROM stack_secrets WHERE stack_id = ? ORDER BY name`, stackID)
	if err != nil {
		return nil, fmt.Errorf("store: reading stack secrets: %w", err)
	}
	defer rows.Close()

	var out []StackSecret
	for rows.Next() {
		var sec StackSecret
		if err := rows.Scan(&sec.Name, &sec.ContentEnc); err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// ── deployments ─────────────────────────────────────────────────────────────────

// Deployment is one attempt to change what is running — including the attempts that never
// reached a container. See docs/stacks.md §2.
type Deployment struct {
	ID              string
	StackID         string
	Action          string
	Status          string // running | ok | failed | cancelled
	Engine          string
	TriggerKind     string // manual | webhook | rollback
	StartedBy       string // empty for a webhook
	RunnerCtrID     string
	ExitCode        *int
	Log             string
	LogTruncated    bool
	CancelRequested bool
	BundleHash      string

	// What it shipped. Empty for an inline source.
	CommitSHA     string
	CommitSubject string

	// ComposeYAML is the resolved file, and it is what a rollback re-applies. Never contains
	// secrets: those live in stack_envs and in the .env rendered inside the runner.
	ComposeYAML string
	RollbackOf  string

	StartedAt time.Time
	EndedAt   time.Time
}

// Trigger kinds. What started a deployment is the first thing you want to know about one you
// did not start yourself.
const (
	TriggerManual   = "manual"
	TriggerWebhook  = "webhook"
	TriggerRollback = "rollback"
)

// Deployment statuses. Cancelled is deliberately not failed: somebody stopped it on purpose,
// and reporting that as a failure would page people about their own decision.
const (
	DeployRunning   = "running"
	DeployOK        = "ok"
	DeployFailed    = "failed"
	DeployCancelled = "cancelled"
)

var ErrRunInProgress = errors.New("store: a deployment is already in progress for this stack")

// ClaimDeployment starts a deployment, refusing if one is already going.
//
// The claim lives in the DATABASE rather than in a mutex, because the thing it guards
// outlives the process: the runner is a detached container, and Daffa restarting (or
// redeploying itself) must not lose track of a deploy that is still going.
func (s *Store) ClaimDeployment(ctx context.Context, d *Deployment) error {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM deployments WHERE stack_id = ? AND status = 'running'`),
		d.StackID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrRunInProgress
	}

	if d.ID == "" {
		d.ID = NewID()
	}
	if d.TriggerKind == "" {
		d.TriggerKind = TriggerManual
	}
	if d.Engine == "" {
		d.Engine = "compose"
	}
	d.Status = DeployRunning
	d.StartedAt = now()

	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO deployments (id, stack_id, action, status, engine, trigger_kind, started_by,
            runner_ctr_id, exit_code, log, log_truncated, cancel_requested, bundle_hash,
            commit_sha, commit_subject, compose_yaml, rollback_of, started_at, ended_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, '', NULL, '', 0, 0, ?, ?, ?, ?, ?, ?, NULL)`),
		d.ID, d.StackID, d.Action, d.Status, d.Engine, d.TriggerKind, nullStr(d.StartedBy),
		d.BundleHash, d.CommitSHA, d.CommitSubject, d.ComposeYAML, nullStr(d.RollbackOf),
		ts(d.StartedAt)); err != nil {
		// The idx_deploy_one_running partial unique index is the real guard on Postgres, where
		// writeMu is a no-op and two claims can both clear the COUNT above; the loser's INSERT
		// violates the index. Rather than sniff the driver's constraint-error type, re-check on a
		// fresh connection (the tx is now aborted): if a run exists, that IS why we failed, and
		// the honest answer is the same "already running" the COUNT gives — a 409, not a 500.
		var running int
		if s.queryRow(ctx,
			`SELECT COUNT(*) FROM deployments WHERE stack_id = ? AND status = 'running'`,
			d.StackID).Scan(&running) == nil && running > 0 {
			return ErrRunInProgress
		}
		return err
	}
	return tx.Commit()
}

func (s *Store) SetDeploymentContainer(ctx context.Context, id, ctrID string) error {
	_, err := s.exec(ctx, `UPDATE deployments SET runner_ctr_id = ? WHERE id = ?`, ctrID, id)
	return err
}

// SetDeploymentBundle records what a deployment is actually shipping, once it is known.
//
// The deployment is claimed BEFORE the bundle is built — see api.deploy for why — so this
// arrives a moment later than the row does.
func (s *Store) SetDeploymentBundle(ctx context.Context, id, hash, commitSHA, commitSubject, yaml string) error {
	_, err := s.exec(ctx, `UPDATE deployments SET bundle_hash = ?, commit_sha = ?,
        commit_subject = ?, compose_yaml = ? WHERE id = ?`,
		hash, commitSHA, commitSubject, yaml, id)
	return err
}

// FinishDeployment closes a deployment with its verdict.
//
// A killed runner and a broken one both exit non-zero, and the daemon cannot tell them apart —
// only the cancel flag can. Reading it HERE, in the one place every deployment ends, is what
// keeps a deploy somebody stopped on purpose out of the failure count and out of the alert
// channel.
func (s *Store) FinishDeployment(ctx context.Context, id string, exitCode int, log string, truncated bool) (status string, err error) {
	defer s.lockWrites()()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var cancelled int
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT cancel_requested FROM deployments WHERE id = ?`), id).Scan(&cancelled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}

	switch {
	case cancelled != 0:
		status = DeployCancelled
	case exitCode != 0:
		status = DeployFailed
	default:
		status = DeployOK
	}

	if _, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE deployments SET status = ?, exit_code = ?, log = ?, log_truncated = ?,
            ended_at = ? WHERE id = ?`),
		status, exitCode, log, boolInt(truncated), ts(now()), id); err != nil {
		return "", err
	}
	return status, tx.Commit()
}

// RequestCancel marks a running deployment as cancelled-on-purpose. The caller then kills the
// runner; FinishDeployment reads this flag to tell that apart from a crash.
//
// It reports whether it actually flagged anything: a deployment that finished a moment ago is
// not cancellable, and telling the operator "cancelled" when nothing was would be a lie they
// would act on.
func (s *Store) RequestCancel(ctx context.Context, id string) (bool, error) {
	res, err := s.exec(ctx, `UPDATE deployments SET cancel_requested = 1
        WHERE id = ? AND status = 'running'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) DeploymentByID(ctx context.Context, id string) (*Deployment, error) {
	return scanDeployment(s.queryRow(ctx, `SELECT `+deployCols+` FROM deployments WHERE id = ?`, id))
}

const deployCols = `id, stack_id, action, status, engine, trigger_kind, started_by, runner_ctr_id,
    exit_code, log, log_truncated, cancel_requested, bundle_hash, commit_sha, commit_subject,
    compose_yaml, rollback_of, started_at, ended_at`

func scanDeployment(sc interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var exitCode sql.NullInt64
	var endedAt, startedBy, rollbackOf sql.NullString
	var startedAt string
	var truncated, cancelled int
	err := sc.Scan(&d.ID, &d.StackID, &d.Action, &d.Status, &d.Engine, &d.TriggerKind, &startedBy,
		&d.RunnerCtrID, &exitCode, &d.Log, &truncated, &cancelled, &d.BundleHash, &d.CommitSHA,
		&d.CommitSubject, &d.ComposeYAML, &rollbackOf, &startedAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exitCode.Valid {
		c := int(exitCode.Int64)
		d.ExitCode = &c
	}
	d.LogTruncated = truncated != 0
	d.CancelRequested = cancelled != 0
	d.StartedBy, d.RollbackOf = startedBy.String, rollbackOf.String
	d.StartedAt = parseTS(startedAt)
	if endedAt.Valid {
		d.EndedAt = parseTS(endedAt.String)
	}
	return &d, nil
}

func collectDeployments(rows *sql.Rows) ([]*Deployment, error) {
	defer rows.Close()
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDeployments returns one stack's history, newest first.
func (s *Store) ListDeployments(ctx context.Context, stackID string, limit int) ([]*Deployment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.query(ctx, `SELECT `+deployCols+` FROM deployments WHERE stack_id = ?
        ORDER BY started_at DESC LIMIT ?`, stackID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing deployments: %w", err)
	}
	return collectDeployments(rows)
}

// DeploymentFilter narrows the cross-stack feed. Every field is optional.
type DeploymentFilter struct {
	Status  string
	StackID string
	EnvID   string
	Trigger string
	// Before pages backwards through time. Empty starts at the newest.
	Before time.Time
	Limit  int
}

// RecentDeployments is the cross-stack feed: what has been deployed lately, anywhere.
//
// It exists because the per-stack history only helps once you already know which stack broke.
// When you do not — which is the situation you are in when someone says "the site is down" —
// there was previously nowhere to look.
//
// global/envs carry the same rule as ListStacks: a deployment on a host you hold nothing on is
// not yours to see, because its stack name alone usually names a customer or a service.
func (s *Store) RecentDeployments(ctx context.Context, global bool, envs []string, f DeploymentFilter) ([]*Deployment, error) {
	if !global && len(envs) == 0 {
		return nil, nil
	}

	// Deployments do not carry a host; their stack does. So the scope check joins.
	conds := []string{}
	args := []any{}

	if !global {
		ph := make([]string, len(envs))
		for i, e := range envs {
			ph[i] = "?"
			args = append(args, e)
		}
		conds = append(conds, `s.env_id IN (`+strings.Join(ph, ",")+`)`)
	}
	if f.Status != "" {
		conds = append(conds, `d.status = ?`)
		args = append(args, f.Status)
	}
	if f.StackID != "" {
		conds = append(conds, `d.stack_id = ?`)
		args = append(args, f.StackID)
	}
	if f.EnvID != "" {
		conds = append(conds, `s.env_id = ?`)
		args = append(args, f.EnvID)
	}
	if f.Trigger != "" {
		conds = append(conds, `d.trigger_kind = ?`)
		args = append(args, f.Trigger)
	}
	if !f.Before.IsZero() {
		conds = append(conds, `d.started_at < ?`)
		args = append(args, ts(f.Before))
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit)

	// The log is deliberately not selected: a feed that dragged fifty full compose logs down
	// the wire would be slowest exactly when someone is in a hurry to read one of them.
	cols := `d.id, d.stack_id, d.action, d.status, d.engine, d.trigger_kind, d.started_by,
        d.runner_ctr_id, d.exit_code, '', d.log_truncated, d.cancel_requested, d.bundle_hash,
        d.commit_sha, d.commit_subject, '', d.rollback_of, d.started_at, d.ended_at`

	rows, err := s.query(ctx, `SELECT `+cols+` FROM deployments d
        JOIN stacks s ON s.id = d.stack_id`+where+`
        ORDER BY d.started_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing recent deployments: %w", err)
	}
	return collectDeployments(rows)
}

// UnfinishedDeployments returns the ones still marked running — whose runner container the
// server must go and reap after a restart.
func (s *Store) UnfinishedDeployments(ctx context.Context) ([]*Deployment, error) {
	rows, err := s.query(ctx, `SELECT `+deployCols+` FROM deployments WHERE status = 'running'`)
	if err != nil {
		return nil, fmt.Errorf("store: listing unfinished deployments: %w", err)
	}
	return collectDeployments(rows)
}

// PruneDeployments keeps the last keepPerStack deployments of every stack, plus everything
// newer than maxAge, and deletes the rest.
//
// Nothing pruned these before, and each one carries its whole log, so the database grew without
// bound — worst on the busiest stack, which is the one you can least afford to have slow.
//
// The two rules are a union, not an intersection: a stack deployed fifty times this morning
// keeps this morning, and a stack deployed twice last year still has both. Either rule alone
// throws away something somebody would miss.
func (s *Store) PruneDeployments(ctx context.Context, keepPerStack int, maxAge time.Duration) (int64, error) {
	if keepPerStack <= 0 || maxAge <= 0 {
		return 0, fmt.Errorf("store: refusing to prune deployments with keep=%d age=%s", keepPerStack, maxAge)
	}

	// Never delete a running deployment: it is the stack's lock, and removing it would let a
	// second deploy start on top of a live one.
	res, err := s.exec(ctx, `DELETE FROM deployments WHERE status != 'running'
        AND started_at < ?
        AND id NOT IN (
            SELECT id FROM deployments d2
            WHERE d2.stack_id = deployments.stack_id
            ORDER BY d2.started_at DESC LIMIT ?
        )`, ts(now().Add(-maxAge)), keepPerStack)
	if err != nil {
		return 0, fmt.Errorf("store: pruning deployments: %w", err)
	}
	return res.RowsAffected()
}
