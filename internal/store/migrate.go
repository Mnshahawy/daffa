package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrations are ordered and applied once. The SQL below is the common subset of
// SQLite and Postgres: TEXT/INTEGER only, no SERIAL, no dialect functions. Where a
// dialect genuinely differs we branch — but the goal is that this list stays shared.
//
// Append-only: once a migration has run anywhere, never edit it. A dropped database is
// somebody's history; the next shape change is migration 0002, never an edit to 0001.
//
// pg is Postgres-only SQL, applied after sql in the same transaction — the seam for the
// day the dialects disagree about something that cannot be papered over: SQLite's INTEGER
// is 64-bit, Postgres's is 32.
//
// fn is a Go step, run after sql and pg in the SAME transaction — the seam for a data
// transform common-subset SQL cannot express (splitting a delimited string: SQLite spells
// position instr(), Postgres spells it strpos(), and neither has the other's). A fn is
// held to the migration rules like the SQL is — append-only, deterministic, and it must
// produce the identical end schema on a fresh database and a migrated one.
var migrations = []struct {
	name string
	sql  string
	pg   string
	fn   func(ctx context.Context, tx *sql.Tx, s *Store) error
}{
	{name: "0001_init", sql: `
-- ── hosts ─────────────────────────────────────────────────────────────────────────

-- An environment is WHERE THINGS RUN, and it is what you select, scope a grant to, and deploy
-- to. It is one of two shapes:
--
--   standalone — one node, not in a Swarm. Every environment was this, before nodes existed.
--   swarm      — a Swarm cluster: one or more nodes, one or more of them managers.
--
-- There is deliberately no kind column. An environment is a swarm exactly when it has a
-- swarm_id, so a kind would be a second copy of a fact already in the row — and a copy that can
-- disagree with its original is a bug waiting for a migration to fix it. See Environment.IsSwarm.
--
-- The environment is the RBAC scope (role_members.scope_id), which is the reason a Swarm gets a
-- row at all rather than being derived per request: a grant needs a stable id to hang on. It is
-- also why a Swarm cluster cannot be modelled as N environments — any manager drives the whole
-- cluster, so a grant on one machine of it would confer the run of all of them.
CREATE TABLE environments (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    -- Info().Swarm.Cluster.ID, as the daemon reports it. '' means standalone.
    swarm_id    TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'unknown',
    created_at  TEXT NOT NULL
);

-- Two environments cannot be the same Swarm. If two of them ever claim to be, that is a conflict
-- a person has to resolve — merging them would silently merge two sets of grants — so the index
-- makes the bad state unrepresentable rather than merely discouraged.
CREATE UNIQUE INDEX idx_env_swarm ON environments (swarm_id) WHERE swarm_id != '';

-- A node is ONE DOCKER DAEMON — i.e. one machine. It is not a thing you select or scope to; it
-- is what an environment is made OF. A standalone environment has exactly one.
--
-- The local/agent distinction lives here, not on the environment, because it was always a
-- property of a daemon: how Daffa dials it. That is the whole of dockerx's founding idea.
CREATE TABLE nodes (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,             -- local | agent
    docker_host TEXT NOT NULL DEFAULT '',  -- local: unix:///var/run/docker.sock
    agent_id    TEXT,                      -- agent: FK-by-convention (M2)

    -- Swarm's own view of this daemon, reconciled from Info().Swarm on connect and on every
    -- liveness ping. The DAEMON is authoritative; these columns are a cache with a name on it.
    -- swarm_node_id is the join key that turns a task's NodeID into the client that can exec
    -- into it — which is how cross-node exec falls out of the model rather than needing an
    -- agent mesh to route it.
    swarm_node_id TEXT NOT NULL DEFAULT '',
    swarm_role    TEXT NOT NULL DEFAULT 'none', -- none | worker | manager
    is_leader     INTEGER NOT NULL DEFAULT 0,

    status       TEXT NOT NULL DEFAULT 'unknown',
    last_seen_at TEXT
);

CREATE INDEX idx_nodes_env ON nodes (env_id);
CREATE UNIQUE INDEX idx_nodes_agent ON nodes (agent_id) WHERE agent_id IS NOT NULL;

-- An agent is a remote host that dials US. Its environment row is created when it
-- first connects; the agent row exists from the moment an admin declares it, so a
-- join token can be minted before the machine is even provisioned.
CREATE TABLE agents (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    token_hash   TEXT,                    -- long-lived agent token; NULL until enrolled
    version      TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT,
    created_at   TEXT NOT NULL,
    created_by   TEXT
);

-- Join tokens are single-use and short-lived: they buy exactly one enrollment, after
-- which the agent authenticates with its own long-lived token. Only the hash is kept.
CREATE TABLE join_tokens (
    id         TEXT PRIMARY KEY,          -- hash of the token
    agent_id   TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- ── identity and authorization ────────────────────────────────────────────────────

-- Capabilities are the only authority — there is no rank ladder. See docs/rbac.md.
CREATE TABLE roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- is_admin resolves to EVERY capability at runtime rather than to a stored all-ones
    -- set. Otherwise the day we add a capability, every existing admin silently would not
    -- have it.
    is_admin    INTEGER NOT NULL DEFAULT 0,
    builtin     INTEGER NOT NULL DEFAULT 0,   -- cannot be deleted
    created_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX roles_name_uq ON roles (name);

-- A role's capabilities: ONE MASK PER FUNCTIONAL AREA, one row each.
--
-- A ROW per area rather than a COLUMN per area, deliberately. Both store "one int per
-- namespace"; only this one lets a new area be added with a single line in
-- internal/caps/caps.go and no migration, no new column, and no edit to the scan and insert
-- lists that a future namespace would otherwise have to be threaded through.
--
-- INTEGER, on purpose, and the ceiling is real: Postgres's INTEGER is 32-bit and SIGNED, so
-- the highest bit a mask may carry is 30. That is caps.MaxBit, TestCeiling holds the registry
-- under it, and TestAMaskColumnHoldsAHighBitOnPostgres holds this column to it. The masks are
-- cached in memory for every user and no area is anywhere near 31 bits — an area that ever
-- fills up becomes two areas, not a wider column.
--
-- ns is not constrained to a fixed list. An area Daffa does not recognise is DROPPED on read by
-- caps.Normalize rather than rejected on write, which is what makes a downgrade safe: a newer
-- Daffa's rows survive in the database and grant nothing to the older one, instead of being
-- resolved against a registry where the same bit numbers mean different things.
CREATE TABLE role_caps (
    role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    ns      TEXT NOT NULL,               -- docker | deploy | data | observe | … see caps.Namespaces
    mask    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (role_id, ns)
);

-- Seeded roles. Admin is builtin and undeletable; Operator and Viewer are ordinary
-- editable presets, not sacred.
--
-- Admin carries NO role_caps rows at all: is_admin resolves to the whole registry at runtime,
-- and a stored set would leave every administrator one capability short the day we add one.
INSERT INTO roles (id, name, description, is_admin, builtin, created_at) VALUES
    ('role_admin', 'Admin',
     'Every capability, including any added in future versions.',
     1, 1, '2026-07-12T00:00:00Z'),
    ('role_operator', 'Operator',
     'Run the fleet: deploy stacks, manage containers, run backups. No shell, no prune, no restore.',
     0, 0, '2026-07-12T00:00:00Z'),
    ('role_viewer', 'Viewer',
     'Read-only across containers, stacks, backups and the audit log.',
     0, 0, '2026-07-12T00:00:00Z');

-- The seeded masks, per area. These are opaque numbers in SQL and that is a hazard, so
-- TestSeededRolesGrantWhatTheyClaim decodes them back into capability NAMES and pins the list —
-- a seed that quietly drifts is a role that hands out something nobody chose.
--
-- Operator deliberately holds NONE of containers.exec, system.prune, backups.restore or
-- backups.download. That is the entire point of those bits: being trusted to restart a container
-- is not the same as being trusted with a root shell on the host.
INSERT INTO role_caps (role_id, ns, mask) VALUES
    -- containers.view/edit, images.view/edit, networks.view/edit, volumes.view/edit
    ('role_operator', 'docker',  507),
    -- stacks.view/edit, registries.view, gitcreds.view
    ('role_operator', 'deploy',   23),
    -- backups.view/edit, storage.view
    ('role_operator', 'data',     19),
    -- monitors.view/edit, audit.view
    ('role_operator', 'observe',   7),
    -- hosts.view
    ('role_operator', 'admin',    64),

    -- containers.view, images.view, networks.view, volumes.view
    ('role_viewer',   'docker',  169),
    -- stacks.view
    ('role_viewer',   'deploy',    1),
    -- backups.view, storage.view
    ('role_viewer',   'data',     17),
    -- monitors.view, audit.view
    ('role_viewer',   'observe',   5),
    -- hosts.view
    ('role_viewer',   'admin',    64);

-- Identity providers live in the database, and there may be more than one. The client
-- secret is sealed with the master key, like every other secret.
CREATE TABLE oidc_providers (
    id                TEXT PRIMARY KEY,
    -- slug keys the callback URL, so the redirect is stable and knowable BEFORE the row
    -- exists: https://<host>/api/auth/callback/<slug>. An id would not be.
    slug              TEXT NOT NULL,
    name              TEXT NOT NULL,
    issuer            TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    client_secret_enc TEXT NOT NULL DEFAULT '',
    redirect_url      TEXT NOT NULL,
    scopes            TEXT NOT NULL DEFAULT 'openid profile email',
    roles_claim       TEXT NOT NULL DEFAULT '',
    -- NULL means a user whose claims map to no role is REFUSED at login. Handing them a
    -- session with an empty capability mask would render an empty application and read as
    -- a bug rather than as a decision.
    default_role_id   TEXT REFERENCES roles (id),
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX oidc_providers_slug_uq ON oidc_providers (slug);

-- An identity provider's group maps to a role AT A SCOPE: "sre" → Operator on staging.
-- Without the scope columns, an SSO-only deployment could not use scoping at all — the
-- feature would exist and be unreachable from the only way anybody signs in. The same
-- claim may map to the same role at two different scopes, so the scope is in the key.
CREATE TABLE oidc_role_mappings (
    id          TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES oidc_providers (id) ON DELETE CASCADE,
    claim_value TEXT NOT NULL,
    role_id     TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    scope_kind  TEXT NOT NULL DEFAULT 'global',   -- global | env
    scope_id    TEXT NOT NULL DEFAULT ''          -- env id when scope_kind = 'env'
);
CREATE UNIQUE INDEX oidc_map_uq ON oidc_role_mappings (provider_id, claim_value, role_id, scope_kind, scope_id);
CREATE INDEX oidc_map_provider_idx ON oidc_role_mappings (provider_id);

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,           -- local | oidc
    username      TEXT,                    -- local only
    password_hash TEXT,                    -- local only, argon2id
    sub           TEXT,                    -- oidc only
    -- A sub is only unique WITHIN an issuer: two IdPs can legitimately issue the same
    -- subject, and the loser of a global collision would be logged in as the winner.
    -- Hence the provider joins the unique key below.
    oidc_provider_id TEXT REFERENCES oidc_providers (id),
    email         TEXT NOT NULL DEFAULT '',
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    last_login_at TEXT
);
CREATE UNIQUE INDEX users_username_uq     ON users (username);
CREATE UNIQUE INDEX users_provider_sub_uq ON users (oidc_provider_id, sub);

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,          -- hash of the cookie token, never the token
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    break_glass INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- A grant is a role AT A SCOPE: "Sara is Operator ON staging". See docs/scoping.md.
--
-- The scope is on the BINDING, not the role, so roles stay small and reusable — there is no
-- "Operator (staging)" role sitting next to "Operator (prod)" waiting to drift apart. The
-- same role may be held more than once by the same person (Viewer everywhere, Operator on
-- staging), which is why the scope is part of the primary key.
--
-- source records who granted the membership. On each OIDC login Daffa replaces that
-- user's 'oidc' rows from the claim and leaves 'local' rows alone: the identity provider
-- is authoritative for what it manages, and a role granted inside Daffa survives the next
-- login instead of being silently wiped.
CREATE TABLE role_members (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_id    TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    source     TEXT NOT NULL DEFAULT 'local',    -- local | oidc
    -- 'global' means everywhere; 'env' means one host, named by scope_id.
    --
    -- A role holding any global-only capability (users.*, roles.*, settings.*, hosts.edit,
    -- and the credential-store edits) can only ever be granted 'global' — enforced in the
    -- store, not here, because it depends on the capability registry. That rule is what
    -- keeps "Admin on staging" from existing, and therefore keeps the admin short-circuit
    -- in EffectiveMask from silently promoting a scoped grant to the whole fleet.
    scope_kind TEXT NOT NULL DEFAULT 'global',   -- global | env
    scope_id   TEXT NOT NULL DEFAULT '',         -- env id when scope_kind = 'env'
    PRIMARY KEY (user_id, role_id, scope_kind, scope_id)
);
CREATE INDEX role_members_role_idx  ON role_members (role_id);
CREATE INDEX role_members_scope_idx ON role_members (scope_kind, scope_id);

-- API tokens: automation without a browser. See docs/tokens.md.
--
-- A token is a way to BE a user without a session — no parallel principal type, no second
-- grants table. The row stores only the SHA-256 of the secret (the session-cookie
-- treatment: a dump of this table cannot be replayed as a credential) plus a display
-- prefix so the UI can say WHICH daffa_3fJk… this is. ON DELETE CASCADE: a deleted user's
-- tokens die with them, and a DISABLED user's tokens are refused at resolve time.
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,           -- first chars of the secret, for display only
    hash         TEXT NOT NULL UNIQUE,    -- sha256(secret); the secret itself is never stored
    expires_at   TEXT,                    -- NULL = does not expire (stated, not smuggled)
    created_at   TEXT NOT NULL,
    last_used_at TEXT                     -- throttled to one write per minute per token
);
-- Two tokens with one name under one user would make "revoke the forgejo token" ambiguous.
CREATE UNIQUE INDEX api_tokens_user_name_uq ON api_tokens (user_id, name);

CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,
    at         TEXT NOT NULL,
    user_id    TEXT,
    user_label TEXT NOT NULL DEFAULT '',   -- denormalized: audit outlives the user row
    env_id     TEXT,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    outcome    TEXT NOT NULL,              -- ok | error | denied
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_at_idx ON audit_log (at);

CREATE TABLE settings (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);

-- Break-glass tokens are minted by the CLI (which has shell on the box) and
-- redeemed once, in the browser. Only the hash is stored, so a dump of this table
-- cannot be replayed; rows are deleted on redemption and swept when expired.
CREATE TABLE break_glass_tokens (
    id         TEXT PRIMARY KEY,          -- hash of the token, never the token
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- ── deploy ────────────────────────────────────────────────────────────────────────

-- A registry credential. The password is sealed with the master key; it is written
-- into an ephemeral docker config inside the runner and never touches disk here.
CREATE TABLE registries (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    url           TEXT NOT NULL,          -- e.g. ghcr.io
    username      TEXT NOT NULL DEFAULT '',
    password_enc  TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

-- A git credential is the same shape of thing as a registry or a bucket: configured once,
-- used by several stacks. Pasting an access token into each stack that needs it is how one
-- of them ends up stale, and how a rotation turns into an archaeology exercise.
CREATE TABLE git_credentials (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    kind           TEXT NOT NULL,             -- token | ssh
    username       TEXT NOT NULL DEFAULT '',  -- token: any non-empty value works for most forges
    token_enc      TEXT NOT NULL DEFAULT '',
    ssh_key_enc    TEXT NOT NULL DEFAULT '',  -- PEM private key
    passphrase_enc TEXT NOT NULL DEFAULT '',
    -- Optional: the server's SSH host key (one line of ssh-keyscan output). When set, the
    -- host is pinned and a substituted server is refused. When empty, it is not.
    host_key       TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    created_by     TEXT
);

-- A stack is a set of services deployed together from one compose file, on one host, under
-- one project name. Git is the recommended source: the repo stays the source of truth and
-- Daffa is only the executor.
CREATE TABLE stacks (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    -- Placement: which NODE the containers land on. Engine and placement are different
    -- questions — "how is the file applied" and "where does it run" — and collapsing them
    -- would re-create the implicitness the engine column exists to remove.
    --
    -- Empty means "the environment decides": a standalone environment has exactly one node, and
    -- a swarm stack is placed by the SCHEDULER, which is the entire point of Swarm. It is
    -- required only for a compose stack on a swarm environment with more than one node — that
    -- is, exactly when there is more than one possible answer.
    node_id     TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,            -- the compose project name
    -- Which engine applies the file. It used to be implicit — the entity was called a stack
    -- while only ever running 'docker compose', and you had to read the source to find that
    -- out. It is now stored, shown, and it decides which actions the stack even has.
    engine      TEXT NOT NULL DEFAULT 'compose',  -- compose | swarm
    -- A label, not a hierarchy. The list collapses under it; nothing else reads it.
    group_name  TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL,            -- git | inline
    git_url     TEXT NOT NULL DEFAULT '',
    git_ref     TEXT NOT NULL DEFAULT '',
    git_path    TEXT NOT NULL DEFAULT '', -- path to the compose file within the repo
    git_credential_id TEXT REFERENCES git_credentials (id),
    inline_yaml TEXT NOT NULL DEFAULT '',
    registry_id TEXT,                     -- optional: creds for pulling

    -- Auto-deploy is opt-in per stack. "The compose file changed" and "put this in production
    -- right now" are different statements, and a tool that conflates them eventually deploys
    -- someone's half-finished branch at 2am.
    auto_deploy        INTEGER NOT NULL DEFAULT 0,
    webhook_secret_enc TEXT NOT NULL DEFAULT '',
    -- Newline-separated globs. Empty means "the compose file itself", which is the only
    -- default that cannot surprise anyone.
    watch_paths        TEXT NOT NULL DEFAULT '',

    -- What is LIVE, as of the last SUCCESSFUL deploy. The hash answers "has the source changed
    -- since?" without pretending to reproduce compose's own config-hash; the commit answers
    -- "which commit is running?", which a hash cannot.
    deployed_hash   TEXT NOT NULL DEFAULT '',
    deployed_commit TEXT NOT NULL DEFAULT '',
    deployed_at     TEXT,
    created_at  TEXT NOT NULL,
    created_by  TEXT
);
CREATE UNIQUE INDEX stacks_env_name_uq ON stacks (env_id, name);

CREATE TABLE stack_envs (
    stack_id  TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    k         TEXT NOT NULL,
    v_enc     TEXT NOT NULL,
    is_secret INTEGER NOT NULL DEFAULT 0, -- write-only in the UI once saved
    PRIMARY KEY (stack_id, k)
);

-- A stack secret is the file-shaped twin of a stack env var: sealed material a stack
-- carries, delivered as a file the deploy writes into the bundle (daffa-secrets/<name>)
-- for the compose secrets: primitive to mount at /run/secrets/<name>. See docs/secrets.md.
--
-- content_enc is sealed under the master key, exactly like stack_envs.v_enc — the plaintext
-- exists only in the runner bundle, never in a deployment row. ON DELETE CASCADE, so a
-- deleted stack takes its secrets with it: the stack_envs precedent verbatim.
CREATE TABLE stack_secrets (
    stack_id    TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    content_enc TEXT NOT NULL,
    PRIMARY KEY (stack_id, name)
);

-- One row per deploy ATTEMPT — including the ones that never reach a container. A compose
-- file that will not parse and a container that exits 1 are both just a failed deployment
-- with a log, and they belong in the same list, read the same way.
--
-- status='running' is also the LOCK: a stack with a running deployment refuses a second one,
-- which is why the claim is a conditional insert rather than a mutex in the server (a mutex
-- would not survive a restart, and the runner container does).
CREATE TABLE deployments (
    id            TEXT PRIMARY KEY,
    stack_id      TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    action        TEXT NOT NULL,          -- up | pull | stop | restart | down | down+volumes
    status        TEXT NOT NULL,          -- running | ok | failed | cancelled
    engine        TEXT NOT NULL DEFAULT 'compose',
    -- 'trigger' is a reserved word in Postgres and would need quoting everywhere it appeared.
    trigger_kind  TEXT NOT NULL DEFAULT 'manual', -- manual | webhook | rollback
    started_by    TEXT,                   -- the user; empty for a webhook
    runner_ctr_id TEXT NOT NULL DEFAULT '',
    exit_code     INTEGER,
    log           TEXT NOT NULL DEFAULT '',
    log_truncated INTEGER NOT NULL DEFAULT 0,
    -- Set by a cancel request, read when the runner exits: it is what tells a killed runner
    -- apart from a failed one, so a deploy somebody stopped on purpose is not reported as a
    -- failure and does not page anyone.
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    bundle_hash   TEXT NOT NULL DEFAULT '',
    commit_sha     TEXT NOT NULL DEFAULT '',  -- empty for inline sources
    commit_subject TEXT NOT NULL DEFAULT '',
    -- The RESOLVED compose file. This is what a rollback re-applies, and storing it is what
    -- makes a rollback independent of git: a moved branch, a deleted tag or an unreachable
    -- repo cannot stop you putting back the thing that worked. Secrets are not in here —
    -- they live in stack_envs and in the .env rendered inside the runner.
    compose_yaml  TEXT NOT NULL DEFAULT '',
    rollback_of   TEXT,                   -- the deployment this one re-applied
    started_at    TEXT NOT NULL,
    ended_at      TEXT
);
CREATE INDEX deployments_stack_idx  ON deployments (stack_id, started_at);
-- The cross-stack feed: "something broke and I do not yet know where".
CREATE INDEX deployments_recent_idx ON deployments (started_at);
-- "One running deployment per stack", made true in the database itself rather than by a
-- COUNT-then-INSERT. On Postgres the SQLite write lock that used to serialize the claim is a
-- no-op, and READ COMMITTED lets two concurrent claims both read zero and both insert — two
-- runners applying the same compose project at once. The same trick as idx_env_swarm.
CREATE UNIQUE INDEX idx_deploy_one_running ON deployments (stack_id) WHERE status = 'running';

-- Volume sources: "this volume's contents come from this git subtree" — config-shaped,
-- reproducible, freely overwritten. See docs/volumes.md.
--
-- There is deliberately no volume_type column anywhere: a Docker volume stays a Docker
-- volume, and intent is declared by ATTACHMENTS, never inferred from a name. A volume
-- backup job (engine='volume') says the opposite of a source: the contents are precious.
CREATE TABLE volume_sources (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    volume             TEXT NOT NULL,
    git_url            TEXT NOT NULL,
    git_ref            TEXT NOT NULL DEFAULT '',
    git_path           TEXT NOT NULL DEFAULT '',  -- the subtree; empty = repository root
    git_credential_id  TEXT REFERENCES git_credentials (id),
    uid                INTEGER NOT NULL DEFAULT 0,
    gid                INTEGER NOT NULL DEFAULT 0,
    -- Linked ⇒ that stack's deploys sync this source first and fail loudly if the sync
    -- fails: a stack must not come up against config Daffa knows is stale. ON DELETE SET
    -- NULL, not CASCADE: deleting the stack unlinks the source, it does not delete it —
    -- the volume (and whatever mounts it next) may outlive the stack that introduced it.
    stack_id           TEXT REFERENCES stacks (id) ON DELETE SET NULL,
    restart_targets    TEXT NOT NULL DEFAULT '',  -- space-separated containers bounced after a CHANGED sync
    auto_sync          INTEGER NOT NULL DEFAULT 0,
    webhook_secret_enc TEXT NOT NULL DEFAULT '',
    synced_hash        TEXT NOT NULL DEFAULT '',
    synced_commit      TEXT NOT NULL DEFAULT '',  -- answers "which commit's config is live?"
    synced_at          TEXT,
    status             TEXT NOT NULL DEFAULT 'pending', -- pending | ok | error
    last_error         TEXT NOT NULL DEFAULT '',
    warnings           TEXT NOT NULL DEFAULT '',  -- newline-separated say-so (e.g. key material in the repo)
    created_at         TEXT NOT NULL,
    created_by         TEXT
);
-- Two sources fighting over one volume would take turns mirror-deleting each other's
-- files. A configuration error, refused at create time.
CREATE UNIQUE INDEX volume_sources_env_volume_uq ON volume_sources (env_id, volume);
CREATE INDEX volume_sources_stack_idx ON volume_sources (stack_id);

-- ── backups ───────────────────────────────────────────────────────────────────────

-- Object storage is a thing you configure once and point several backup jobs at, not a
-- pile of fields you retype (and get subtly wrong) for every database you back up.
CREATE TABLE storage_targets (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    endpoint    TEXT NOT NULL,
    region      TEXT NOT NULL DEFAULT 'auto',
    bucket      TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    secret_enc  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    created_by  TEXT
);

-- A backup job dumps a database out of a running container — or archives a volume
-- (engine='volume', where "container" is unused and "volume" names the subject) — and
-- streams it to object storage. Encryption is to named age keys via backup_job_keys; the
-- server holds only public recipients, so the box can write backups it cannot read.
CREATE TABLE backup_jobs (
    id              TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name            TEXT NOT NULL UNIQUE,
    container       TEXT NOT NULL,          -- container name or id on that env
    engine          TEXT NOT NULL,          -- postgres | mysql | mongodb | volume
    databases       TEXT NOT NULL DEFAULT '', -- empty = whole cluster/server
    db_user         TEXT NOT NULL DEFAULT '',
    db_password_enc TEXT NOT NULL DEFAULT '', -- mysql/mongo need it; postgres usually does not
    schedule        TEXT NOT NULL DEFAULT '', -- cron; empty = manual only

    storage_id      TEXT NOT NULL REFERENCES storage_targets (id),
    prefix          TEXT NOT NULL DEFAULT '',   -- where in the bucket this job's snapshots live

    encryption      TEXT NOT NULL DEFAULT 'age', -- age (to public recipients) | none

    -- engine='volume' only: the subject, and which containers to stop for the copy.
    -- stop_containers trades downtime for consistency, per job, in writing — a file-level
    -- snapshot of a live database is torn, and the form says so.
    volume          TEXT NOT NULL DEFAULT '',
    stop_containers TEXT NOT NULL DEFAULT '',

    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    created_by      TEXT
);

CREATE TABLE backup_runs (
    id          TEXT PRIMARY KEY,
    job_id      TEXT NOT NULL REFERENCES backup_jobs (id) ON DELETE CASCADE,
    status      TEXT NOT NULL,              -- running | ok | failed
    trigger     TEXT NOT NULL DEFAULT 'manual', -- manual | schedule
    bytes       INTEGER NOT NULL DEFAULT 0,
    object_key  TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    started_by  TEXT
);
CREATE INDEX backup_runs_job_idx ON backup_runs (job_id, started_at);

-- ── certificates and keys ─────────────────────────────────────────────────────────

-- The certificate manager. See docs/certs.md.
--
-- Three kinds of private key, three deliberately different answers:
--   * leaf keys    — key_enc, sealed: Traefik must present them to serve TLS.
--   * CA keys      — key_enc, sealed: the signer stays online so renewal is automatic.
--   * age keys     — NOT HERE. encryption_keys holds only the public recipient; the private
--                    half is downloaded once at generation and never stored. The box can
--                    encrypt backups it cannot read.
CREATE TABLE cert_authorities (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    subject       TEXT NOT NULL,
    cert_pem      TEXT NOT NULL,              -- public material, plaintext
    key_enc       TEXT NOT NULL DEFAULT '',   -- sealed; empty = trust-only anchor (cannot sign)
    key_algo      TEXT NOT NULL DEFAULT '',
    not_before    TEXT NOT NULL,
    not_after     TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active', -- active | next | retired
    rotates_id    TEXT,                        -- on a NEXT CA: the active CA it will replace
    overlap_until TEXT,                        -- while a rotation is in flight: when the announced overlap window ends
    warn_days     INTEGER NOT NULL DEFAULT 180, -- when "rotate me" notifications start
    created_at    TEXT NOT NULL,
    created_by    TEXT
);

-- No ON DELETE on ca_id: deleting a CA out from under live certificates is refused in the
-- handler (CertAuthorityInUse), the storage-target precedent — not silently cascaded.
CREATE TABLE certificates (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,   -- also the filename deliveries write
    ca_id             TEXT REFERENCES cert_authorities (id), -- NULL = uploaded; tracked, not renewable
    sans              TEXT NOT NULL DEFAULT '', -- space-separated; first is the CN
    key_algo          TEXT NOT NULL DEFAULT '',
    cert_pem          TEXT NOT NULL,
    chain_pem         TEXT NOT NULL DEFAULT '',
    key_enc           TEXT NOT NULL,           -- sealed
    not_before        TEXT NOT NULL,
    not_after         TEXT NOT NULL,
    validity_days     INTEGER NOT NULL DEFAULT 398,
    renew_before_days INTEGER NOT NULL DEFAULT 30,
    status            TEXT NOT NULL DEFAULT 'ok', -- ok | error
    last_error        TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    created_by        TEXT
);

CREATE TABLE encryption_keys (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    recipient  TEXT NOT NULL,                 -- one age PUBLIC key; plaintext is the point
    source     TEXT NOT NULL DEFAULT 'imported', -- generated | imported
    created_at TEXT NOT NULL,
    created_by TEXT
);

-- A delivery keeps cert material current inside a named volume on one host, where a
-- container (Traefik) mounts it read-only. ON DELETE CASCADE from both parents: a delivery
-- of a deleted cert or onto a deleted host delivers nothing and means nothing.
CREATE TABLE cert_deliveries (
    id              TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    cert_id         TEXT REFERENCES certificates (id) ON DELETE CASCADE, -- NULL = trust-bundle-only
    volume          TEXT NOT NULL DEFAULT 'daffa-certs',
    uid             INTEGER NOT NULL DEFAULT 0,
    gid             INTEGER NOT NULL DEFAULT 0,
    traefik         INTEGER NOT NULL DEFAULT 0, -- also render a file-provider tls.yml
    restart_targets TEXT NOT NULL DEFAULT '',   -- space-separated container names; empty = consumer hot-reloads
    synced_hash     TEXT NOT NULL DEFAULT '',
    synced_at       TEXT,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | ok | error
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT
);
CREATE INDEX cert_deliveries_env_idx ON cert_deliveries (env_id);

-- Backup jobs encrypt to NAMED keys, never raw recipient strings.
-- No ON DELETE on key_id: deleting a key that a job still encrypts to is refused in the
-- handler — silently dropping a recipient narrows who can restore, which is exactly the
-- kind of surprise a backup system must not spring.
CREATE TABLE backup_job_keys (
    job_id TEXT NOT NULL REFERENCES backup_jobs (id) ON DELETE CASCADE,
    key_id TEXT NOT NULL REFERENCES encryption_keys (id),
    PRIMARY KEY (job_id, key_id)
);

-- Keyrings: rotatable application encryption keys. See docs/keyrings.md.
--
-- A keyring is a stable name over an append-only set of versions, so "rotate" can mean
-- "new data uses the new key" instead of "all old data is now unreadable". The material is
-- GENERATED, which breaks the usual sealing promise: unlike a registry password, a keyring
-- version has no off-box source of truth to re-enter — the sealed row is the only durable
-- copy in existence. The database and master.key backups are what make keyrings durable.
CREATE TABLE keyrings (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,           -- also the filename prefix deliveries write
    rotate_days INTEGER NOT NULL DEFAULT 0,     -- 0 = manual rotation only
    created_at  TEXT NOT NULL,
    created_by  TEXT
);

-- The version id (krv_…) is the kid applications store beside their ciphertext. Rows are
-- never deleted, only moved through active → decrypt_only → retired: a retired version's
-- row is the audit trail of what existed, and at ~300 bytes there is no pressure to reap.
CREATE TABLE keyring_versions (
    id           TEXT PRIMARY KEY,
    keyring_id   TEXT NOT NULL REFERENCES keyrings (id) ON DELETE CASCADE,
    material_enc TEXT NOT NULL,                 -- sealed 32 bytes; write-only forever
    state        TEXT NOT NULL,                 -- active | decrypt_only | retired
    created_at   TEXT NOT NULL
);
CREATE INDEX keyring_versions_ring_idx ON keyring_versions (keyring_id);

-- No ON DELETE on keyring_id: deleting a keyring out from under a live delivery is refused
-- in the handler (KeyringInUse), the storage-target precedent — retiring versions is the
-- graduated alternative. ON DELETE CASCADE from the environment: a delivery onto a deleted
-- host delivers nothing and means nothing (the cert_deliveries reasoning).
CREATE TABLE keyring_deliveries (
    id              TEXT PRIMARY KEY,
    keyring_id      TEXT NOT NULL REFERENCES keyrings (id),
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    volume          TEXT NOT NULL DEFAULT 'daffa-keys',
    uid             INTEGER NOT NULL DEFAULT 0,
    gid             INTEGER NOT NULL DEFAULT 0,
    restart_targets TEXT NOT NULL DEFAULT '',   -- space-separated container names; empty = consumer re-reads
    synced_hash     TEXT NOT NULL DEFAULT '',
    synced_at       TEXT,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | ok | error
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT
);
CREATE INDEX keyring_deliveries_env_idx ON keyring_deliveries (env_id);

-- ── notifications ─────────────────────────────────────────────────────────────────

-- Email notifications. See docs/notifications.md.
--
-- One SMTP server, one row. The id is fixed so there can only ever be one — a settings
-- table that could hold two rows is a settings table that eventually does, and then which
-- one wins is decided by ORDER BY.
CREATE TABLE smtp_settings (
    id           TEXT PRIMARY KEY,     -- always 'smtp'
    host         TEXT NOT NULL DEFAULT '',
    port         INTEGER NOT NULL DEFAULT 587,
    username     TEXT NOT NULL DEFAULT '',
    -- Sealed with the master key, like every other secret. WRITE-ONLY: no endpoint reads it
    -- back, so there is nothing to leak.
    password_enc TEXT NOT NULL DEFAULT '',
    from_addr    TEXT NOT NULL DEFAULT '',
    from_name    TEXT NOT NULL DEFAULT 'Daffa',
    -- base_url makes the "Open in Daffa" link work. Daffa cannot know its own public URL —
    -- it sits behind a proxy — so somebody has to say. Empty just omits the button.
    base_url     TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL
);

-- Chat channels: a place a notification can go that is not an email. Slack, Discord, or a
-- generic webhook that gets the raw JSON.
--
-- A channel is a URL and nothing else worth a schema. The URL is the whole secret — a Slack
-- incoming-webhook URL is a bearer credential, anyone holding it can post to that channel — so
-- it is SEALED with the master key exactly like an SMTP password, and no endpoint reads it back.
CREATE TABLE notification_channels (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,               -- slack | discord | webhook
    name       TEXT NOT NULL,
    url_enc    TEXT NOT NULL,               -- sealed; write-only, never returned
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);

-- A rule is an event and a recipient — exactly ONE of three (enforced in Go, in
-- CreateNotificationRule): a ROLE (resolved at send time, so the list tracks membership by
-- itself), a literal address (for a shared inbox or a pager that is not a Daffa user), or a
-- channel.
--
-- ON DELETE CASCADE from the channel, the same way deleting a role cascades: a rule pointing
-- at a channel that no longer exists is a rule that silently drops the alert it was written
-- to deliver.
CREATE TABLE notification_rules (
    id         TEXT PRIMARY KEY,
    event      TEXT NOT NULL,
    role_id    TEXT REFERENCES roles (id) ON DELETE CASCADE,
    address    TEXT NOT NULL DEFAULT '',
    channel_id TEXT REFERENCES notification_channels (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL
);
CREATE INDEX notification_rules_event_idx ON notification_rules (event);

-- The outbox. Rows are inserted in the SAME transaction as the thing they are about, and
-- drained by a worker OUTSIDE any transaction.
--
-- Both halves matter. Sending inside a transaction holds a database lock for as long as a
-- slow SMTP server feels like taking, and a transaction that then rolls back has already
-- sent an email about an event that did not happen. Enqueuing outside one loses the
-- notification entirely if the process dies in between — which is precisely the moment you
-- most wanted it.
--
-- kind='email' rows carry to_addr and both bodies. A channel row sets kind and channel_id,
-- leaves to_addr empty, and puts the provider-shaped JSON payload in body_text (body_html
-- stays empty — a webhook has no HTML part). channel_id is a plain column, NOT a foreign
-- key: the outbox is a firehose and holds no FKs, so a message whose channel was deleted
-- before it drained simply fails to a dead letter, which is the honest outcome.
CREATE TABLE notification_outbox (
    id           TEXT PRIMARY KEY,
    event        TEXT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'email',  -- email | channel
    channel_id   TEXT NOT NULL DEFAULT '',
    to_addr      TEXT NOT NULL,
    subject      TEXT NOT NULL,
    body_html    TEXT NOT NULL,
    body_text    TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending | sent | failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_try_at  TEXT NOT NULL,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    sent_at      TEXT
);
-- The worker's query: what is due?
CREATE INDEX notification_outbox_due_idx ON notification_outbox (status, next_try_at);

-- ── monitoring ────────────────────────────────────────────────────────────────────

-- Resource monitors. See docs/monitoring.md.
--
-- NOTE: the table that holds the actual samples, metric_samples, is NOT created here, and
-- you have not missed it. It is partitioned by day, its partitions are created and dropped
-- on a schedule, and its DDL is dialect-specific (Postgres has declarative partitioning;
-- SQLite has nothing and gets day tables unioned at read time). All of that lives together
-- in store/metrics.go, because the thing that rewrites a schema every day should own the
-- whole of it rather than have half in a migration and half in a goroutine.

-- How often we sample and how long we keep it. One row, fixed id, for the same reason
-- smtp_settings has one: a settings table that CAN hold two rows eventually does.
CREATE TABLE monitor_settings (
    id             TEXT PRIMARY KEY,               -- always 'monitoring'
    enabled        INTEGER NOT NULL DEFAULT 1,
    interval_secs  INTEGER NOT NULL DEFAULT 30,
    -- Capped at 90 days by validation, not by a CHECK, so the reason can be a sentence
    -- rather than a constraint violation. See store/monitors.go.
    retention_days INTEGER NOT NULL DEFAULT 7,
    updated_at     TEXT NOT NULL
);

-- A rule: a metric, a comparison, a threshold, and how long it has to hold.
--
-- env_id / stack / container are filters, ANDed. A monitor with no env_id watches the whole
-- fleet — which is why creating one requires monitors.edit GLOBALLY, while a host-scoped
-- holder may only create monitors pinned to their host.
--
-- env_id is NULL for "every host", and not the empty string, for two reasons that both bite.
-- A '' would have to satisfy the foreign key, and there is no environment called '' — so a
-- fleet-wide monitor simply could not be created. And NULL is what makes the scoped list
-- filter correct for free: "env_id IN (staging)" evaluates to NULL — not true — for a
-- fleet-wide row, so a staging-scoped holder is not shown the rule that watches production.
-- The FK still earns its place: deleting a host takes its monitors with it, and leaves the
-- fleet-wide ones alone.
CREATE TABLE resource_monitors (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    metric        TEXT NOT NULL,                   -- cpu_pct | mem_pct | mem_bytes
    op            TEXT NOT NULL,                   -- '>' | '<'
    threshold     REAL NOT NULL,
    duration_secs INTEGER NOT NULL,
    env_id        TEXT REFERENCES environments (id) ON DELETE CASCADE,  -- NULL = every host
    stack         TEXT NOT NULL DEFAULT '',
    container     TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX resource_monitors_enabled_idx ON resource_monitors (enabled);

-- What is firing, and what fired. Keyed on container_NAME, not container_id: a compose name
-- (billing-api-1) survives a redeploy and an id does not, and an alert whose clock resets on
-- every deploy is an alert that never reaches ten minutes.
--
-- Resolved rows are kept: "it was in trouble for an hour last night and recovered" is the
-- thing you most want to find in the morning.
CREATE TABLE monitor_alerts (
    id             TEXT PRIMARY KEY,
    monitor_id     TEXT NOT NULL REFERENCES resource_monitors (id) ON DELETE CASCADE,
    env_id         TEXT NOT NULL,
    container_name TEXT NOT NULL,
    container_id   TEXT NOT NULL DEFAULT '',
    stack          TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL,                  -- firing | resolved
    value          REAL NOT NULL DEFAULT 0,
    started_at     TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL,
    resolved_at    TEXT,
    resolve_reason TEXT NOT NULL DEFAULT ''
);
-- The evaluator's question every round: is this monitor already firing on this container?
CREATE INDEX monitor_alerts_firing_idx ON monitor_alerts (monitor_id, container_name, state);
CREATE INDEX monitor_alerts_state_idx ON monitor_alerts (state, started_at);

-- ── logging defaults ──────────────────────────────────────────────────────────────

-- Default container logging for deployed stacks. Daffa cannot edit a host's daemon.json
-- (agents only proxy the Docker API), so retention here IS Docker's own rotation — a
-- json-file/local driver with max-size/max-file — injected as a logging: block into every
-- deployed service that does not declare its own. See docs/stacks.md.
--
-- Two tables, not one with a magic scope id. The global default is a fixed-id singleton
-- (the monitor_settings pattern), and the per-host override keys on env_id so the FK can
-- cascade — a 'global' pseudo-id could never satisfy that FK, and a nullable-unique env_id
-- is not unique under NULL in either dialect, so the singleton would not stay single.
CREATE TABLE log_settings (
    id         TEXT PRIMARY KEY,           -- always 'logging'
    driver     TEXT NOT NULL,
    opts       TEXT NOT NULL DEFAULT '{}', -- JSON object, string -> string
    updated_at TEXT NOT NULL
);

CREATE TABLE env_log_configs (
    env_id     TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    driver     TEXT NOT NULL,
    opts       TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL
);
`},

	// A CA, certificate or delivery the deployment provisions for its own edge (the console's
	// TLS) is marked protected, so deleting it from the UI is refused — the same posture as a
	// system network or volume. Default 0: every existing row is unprotected, and only the
	// edge-cert bootstrap (`daffa edge init`) ever sets it. Common-subset ALTER: SQLite and
	// Postgres both take ADD COLUMN … NOT NULL DEFAULT.
	{name: "0002_protected_certs", sql: `
ALTER TABLE cert_authorities ADD COLUMN protected INTEGER NOT NULL DEFAULT 0;
ALTER TABLE certificates     ADD COLUMN protected INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cert_deliveries  ADD COLUMN protected INTEGER NOT NULL DEFAULT 0;
`},

	// A volume source can now be inline — a set of files authored in Daffa — as well as
	// git-backed. This is what an inline stack (no repo) needs to manage config it delivers
	// into a volume: Traefik's static config and dynamic middlewares, editable in the UI.
	// source_kind defaults to 'git' so every existing source keeps its meaning. The files
	// are plaintext on purpose — the point is to view and edit them; sealed material belongs
	// in stack secrets. path is unique per source (it is the file's name in the volume).
	{name: "0003_inline_volume_sources", sql: `
ALTER TABLE volume_sources ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'git';

CREATE TABLE volsource_files (
    source_id  TEXT NOT NULL REFERENCES volume_sources (id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    mode       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source_id, path)
);
`},

	// A volume backup job can now drop paths from its snapshot — regenerable junk (caches,
	// logs, session temp) that bloats every backup for no restore value. The value is a
	// newline-separated list of paths relative to the volume root; empty (the default for
	// every existing job) means "snapshot everything", the prior behaviour. Common-subset
	// ALTER, mirroring the volume/stop_containers columns it sits beside.
	{name: "0004_backup_exclude_paths", sql: `
ALTER TABLE backup_jobs ADD COLUMN exclude_paths TEXT NOT NULL DEFAULT '';
`},

	// An SSH key is how Daffa dials OUT to a machine it does not run on — a remote cluster's
	// manager, or a node reached over SSH rather than an agent (docs/clusters.md §6). Same shape
	// of shared, configured-once credential as a registry or a git credential.
	//
	// The split of what is sealed and what is not is deliberate and is the whole point:
	//   public_key   — plaintext, ON PURPOSE. It is meant to be read and pasted into the target's
	//                  authorized_keys; it is not a secret, and hiding it would only make the
	//                  store harder to use for no gain.
	//   fingerprint  — plaintext, shown so a human can tell two keys apart at a glance.
	//   *_enc        — the private half and its passphrase, AES-256-GCM under master.key. WRITE-ONLY
	//                  through the API: sealed at rest and never returned, only opened in memory
	//                  when Daffa actually dials out. This is sealed-not-absent on purpose — unlike
	//                  an age backup key ("the box cannot read its own backups"), Daffa MUST hold
	//                  these to use them, so the posture is sealing, not exile. See clusters.md §6.
	{name: "0005_ssh_keys", sql: `
CREATE TABLE ssh_keys (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    algo            TEXT NOT NULL,             -- ed25519 | rsa
    public_key      TEXT NOT NULL,             -- one authorized_keys line
    fingerprint     TEXT NOT NULL,             -- SHA256:…
    private_key_enc TEXT NOT NULL,             -- sealed OpenSSH private key
    passphrase_enc  TEXT NOT NULL DEFAULT '',  -- sealed; empty for a generated key
    created_at      TEXT NOT NULL,
    created_by      TEXT
);
`},

	// A node reached over SSH is the third transport, after the local socket and the agent tunnel
	// (docs/clusters.md §2). The daemon it points at is managed identically to any other — these
	// columns only say how Daffa DIALS it. They are empty on a local or agent node; kind gains the
	// value 'ssh'.
	//
	// ssh_host_key is the pinned host-key line: empty until the first successful dial records it
	// (trust on first use), and thereafter a changed key is refused, not re-pinned silently
	// (docs/clusters.md §7). ssh_key_id is an FK-by-convention to ssh_keys — no ON DELETE, because
	// the SSH-key store refuses to delete a key a node still references (store.SSHKeyInUse).
	{name: "0006_ssh_nodes", sql: `
ALTER TABLE nodes ADD COLUMN ssh_host     TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ssh_port     INTEGER NOT NULL DEFAULT 22;
ALTER TABLE nodes ADD COLUMN ssh_user     TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ssh_key_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ssh_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ssh_host_key TEXT NOT NULL DEFAULT '';
`},

	// An SSH git credential now REFERENCES a key in the shared SSH-key store rather than carrying
	// its own copy: one place to generate, rotate and audit keys, and a git deploy key that is the
	// same object as a cluster key. ssh_key_id is an FK-by-convention to ssh_keys (the store's
	// SSHKeyInUse refuses to delete a key a credential still points at).
	//
	// The inline key columns are dropped unconditionally — a git credential holds no key material
	// of its own anymore, and openGitCred reads it from the key store instead. DROP COLUMN is
	// common-subset (SQLite ≥ 3.35, Postgres).
	{name: "0007_gitcred_ssh_key_ref", sql: `
ALTER TABLE git_credentials ADD COLUMN ssh_key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE git_credentials DROP COLUMN ssh_key_enc;
ALTER TABLE git_credentials DROP COLUMN passphrase_enc;
`},

	// An agent is enrolled TO a cluster: when it connects, Daffa joins its daemon to that cluster's
	// Swarm over the tunnel — no `docker swarm join` by hand (docs/clusters.md §5, §14.2). An agent
	// adds a NODE, never a cluster (§1), so every new agent names its target; the create handler
	// enforces that. join_env_id is a real foreign key so a removed cluster takes its agents with it
	// (ON DELETE CASCADE, the same rule nodes.env_id follows) rather than leaving one dialling a
	// ghost Swarm. It is nullable only because SQLite cannot add a NOT NULL foreign key by ALTER —
	// pre-existing agents keep a NULL target and stay standalone. join_role is worker or manager;
	// join_advertise_addr is the node's reachable overlay address ('' ⇒ Docker auto-detects).
	{name: "0008_agent_join_target", sql: `
ALTER TABLE agents ADD COLUMN join_env_id         TEXT REFERENCES environments (id) ON DELETE CASCADE;
ALTER TABLE agents ADD COLUMN join_role           TEXT NOT NULL DEFAULT 'worker';
ALTER TABLE agents ADD COLUMN join_advertise_addr TEXT NOT NULL DEFAULT '';
`},

	// A certificate now belongs to an environment — or to none, which means SHARED: visible
	// and deliverable everywhere, the meaning every pre-existing row keeps. Name uniqueness
	// becomes per-env (staging and prod each get their own `cellauth`, same filename in the
	// volume, different key), which is why the change cannot be a plain ADD COLUMN: the
	// inline UNIQUE on name has to go, and SQLite cannot drop a constraint without
	// rebuilding the table. See cert-trust-domains.md for the full reasoning.
	//
	// The rebuild is the delicate part. SQLite runs with foreign_keys(1), and DROP TABLE
	// performs an implicit DELETE FROM first — so dropping `certificates` while
	// cert_deliveries still references it would fire cert_id's ON DELETE CASCADE and
	// silently destroy every delivery row. The fn therefore renames the old parent aside,
	// rebuilds the CHILD first (dropping a child fires no cascades), and only then drops
	// the orphaned old parent, which by that point nothing references. Postgres needs none
	// of that: ADD COLUMN + DROP CONSTRAINT.
	//
	// Uniqueness is an expression index rather than UNIQUE(env_id, name), because both
	// dialects treat NULLs as distinct — two SHARED certs could otherwise share a name.
	{name: "0009_cert_env_scope", fn: migrateCertEnvScope},

	// What a leaf may be used AS: 'server', 'client', or 'server client' — space-separated
	// like sans. Every existing cert keeps 'server', the only thing issuance ever produced.
	// The column, not the issue request, is the source of truth: renewals and rotation
	// re-signs read it, so an mTLS cert cannot silently lose clientAuth at its first
	// hourly renewal. For uploaded certs it is derived from the PEM and display-only.
	{name: "0010_cert_usages", sql: `
ALTER TABLE certificates ADD COLUMN usages TEXT NOT NULL DEFAULT 'server';
`},

	// Which roots a delivery's ca-bundle.crt carries: space-separated CA ids, empty = every
	// managed CA (the prior behaviour, which every existing delivery keeps). Selection is by
	// lineage anchor — a staged successor rides along via rotates_id, activation rewrites
	// selections to the promoted id — so an explicitly-selected bundle survives a CA
	// rotation exactly like the full one. See cert-trust-domains.md.
	{name: "0011_delivery_bundle_cas", sql: `
ALTER TABLE cert_deliveries ADD COLUMN bundle_cas TEXT NOT NULL DEFAULT '';
`},

	// Whether Daffa's OWN outbound TLS (registry reach-out, git clones) trusts this CA
	// beyond the system roots. Default 1 is the prior behaviour — every managed CA was
	// trusted. 0 is for a CA that exists only to be bundled into deliveries (someone
	// else's trust anchor), which should not widen what the console itself accepts.
	{name: "0012_ca_outbound_trust", sql: `
ALTER TABLE cert_authorities ADD COLUMN outbound_trust INTEGER NOT NULL DEFAULT 1;
`},

	// A delivery stops being "this certificate, into this volume" and becomes "the
	// Daffa-managed contents of this volume": many certificates through a join table, and a
	// declared mount_path instead of a compiled-in constant. That reframe is what lets ONE
	// Traefik dynamic directory hold Daffa's certificates and a git-sourced volume source at
	// the same time — Traefik reads exactly one directory, and its file provider ignores
	// anything that is not .toml/.yaml/.yml, so the PEMs and both manifests coexist beside
	// the config fragments. See mixed-config-volumes.md.
	{name: "0013_delivery_multi_cert", fn: migrateDeliveryMultiCert},
}

// migrateCertEnvScope is 0009: certificates gain a nullable env_id and lose global name
// uniqueness in favour of per-env uniqueness. Dialects genuinely diverge here — see the
// migration's comment for why SQLite must rebuild both cert tables child-first.
func migrateCertEnvScope(ctx context.Context, tx *sql.Tx, s *Store) error {
	if s.dialect == Postgres {
		for _, stmt := range []string{
			`ALTER TABLE certificates ADD COLUMN env_id TEXT REFERENCES environments (id) ON DELETE CASCADE`,
			`ALTER TABLE certificates DROP CONSTRAINT certificates_name_key`,
			`CREATE UNIQUE INDEX certificates_env_name ON certificates (COALESCE(env_id,''), name)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}

	for _, stmt := range []string{
		// Rename the parent aside; SQLite rewrites cert_deliveries' FK clause to follow.
		`ALTER TABLE certificates RENAME TO certificates_old`,
		// The new parent: 0001's shape + env_id, minus the inline UNIQUE on name.
		`CREATE TABLE certificates (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    env_id            TEXT REFERENCES environments (id) ON DELETE CASCADE, -- NULL = shared
    ca_id             TEXT REFERENCES cert_authorities (id),
    sans              TEXT NOT NULL DEFAULT '',
    key_algo          TEXT NOT NULL DEFAULT '',
    cert_pem          TEXT NOT NULL,
    chain_pem         TEXT NOT NULL DEFAULT '',
    key_enc           TEXT NOT NULL,
    not_before        TEXT NOT NULL,
    not_after         TEXT NOT NULL,
    validity_days     INTEGER NOT NULL DEFAULT 398,
    renew_before_days INTEGER NOT NULL DEFAULT 30,
    status            TEXT NOT NULL DEFAULT 'ok',
    last_error        TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    created_by        TEXT,
    protected         INTEGER NOT NULL DEFAULT 0
)`,
		`INSERT INTO certificates (id, name, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
    not_before, not_after, validity_days, renew_before_days, status, last_error,
    created_at, created_by, protected)
    SELECT id, name, ca_id, sans, key_algo, cert_pem, chain_pem, key_enc,
    not_before, not_after, validity_days, renew_before_days, status, last_error,
    created_at, created_by, protected FROM certificates_old`,
		// Rebuild the child so its FK points at the new parent, then drop child-first:
		// dropping a child fires no cascades, and the old parent is unreferenced after.
		`ALTER TABLE cert_deliveries RENAME TO cert_deliveries_old`,
		`CREATE TABLE cert_deliveries (
    id              TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    cert_id         TEXT REFERENCES certificates (id) ON DELETE CASCADE,
    volume          TEXT NOT NULL DEFAULT 'daffa-certs',
    uid             INTEGER NOT NULL DEFAULT 0,
    gid             INTEGER NOT NULL DEFAULT 0,
    traefik         INTEGER NOT NULL DEFAULT 0,
    restart_targets TEXT NOT NULL DEFAULT '',
    synced_hash     TEXT NOT NULL DEFAULT '',
    synced_at       TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT,
    protected       INTEGER NOT NULL DEFAULT 0
)`,
		`INSERT INTO cert_deliveries SELECT * FROM cert_deliveries_old`,
		`DROP TABLE cert_deliveries_old`,
		`DROP TABLE certificates_old`,
		`CREATE UNIQUE INDEX certificates_env_name ON certificates (COALESCE(env_id,''), name)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateDeliveryMultiCert is 0013: a delivery carries MANY certificates through a join
// table and declares where its volume is mounted, so the rendered tls.yml can point at a
// directory Daffa does not choose. cert_id goes away rather than staying as a "primary"
// certificate beside the join table — two sources of truth for the same question is the
// thing this project keeps refusing to build.
//
// The SQLite order is the reverse of 0009's trap and just as load-bearing. There, dropping
// a parent fired a child's ON DELETE CASCADE; here the new join table would BE the child,
// so it must not exist while cert_deliveries is rebuilt. Hence: capture the pairs into a
// plain FK-free table, rebuild the parent, create the join table, refill it.
func migrateDeliveryMultiCert(ctx context.Context, tx *sql.Tx, s *Store) error {
	const joinTable = `CREATE TABLE cert_delivery_certs (
    delivery_id TEXT NOT NULL REFERENCES cert_deliveries (id) ON DELETE CASCADE,
    cert_id     TEXT NOT NULL REFERENCES certificates (id) ON DELETE CASCADE,
    is_default  INTEGER NOT NULL DEFAULT 0, -- at most one per delivery: tls.yml's stores.default
    PRIMARY KEY (delivery_id, cert_id)
)`
	// A pre-existing single-cert Traefik delivery meant "this IS the default certificate" —
	// its rendered fragment said so — so the backfill keeps saying it.
	const backfill = `INSERT INTO cert_delivery_certs (delivery_id, cert_id, is_default)
        SELECT delivery_id, cert_id, 1 FROM cert_delivery_pairs_tmp`

	var stmts []string
	if s.dialect == Postgres {
		stmts = []string{
			`ALTER TABLE cert_deliveries ADD COLUMN mount_path TEXT NOT NULL DEFAULT '/etc/traefik/dynamic-certs'`,
			`CREATE TABLE cert_delivery_pairs_tmp AS
                SELECT id AS delivery_id, cert_id FROM cert_deliveries
                WHERE cert_id IS NOT NULL AND cert_id <> ''`,
			`ALTER TABLE cert_deliveries DROP COLUMN cert_id`,
			joinTable,
			backfill,
			`DROP TABLE cert_delivery_pairs_tmp`,
		}
	} else {
		stmts = []string{
			`CREATE TABLE cert_delivery_pairs_tmp AS
                SELECT id AS delivery_id, cert_id FROM cert_deliveries
                WHERE cert_id IS NOT NULL AND cert_id <> ''`,
			`ALTER TABLE cert_deliveries RENAME TO cert_deliveries_old`,
			// The 0009 shape + bundle_cas + mount_path, minus cert_id. Nothing references
			// cert_deliveries at this point, so dropping the old table fires no cascade.
			`CREATE TABLE cert_deliveries (
    id              TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    volume          TEXT NOT NULL DEFAULT 'daffa-certs',
    uid             INTEGER NOT NULL DEFAULT 0,
    gid             INTEGER NOT NULL DEFAULT 0,
    traefik         INTEGER NOT NULL DEFAULT 0,
    restart_targets TEXT NOT NULL DEFAULT '',
    synced_hash     TEXT NOT NULL DEFAULT '',
    synced_at       TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT,
    protected       INTEGER NOT NULL DEFAULT 0,
    bundle_cas      TEXT NOT NULL DEFAULT '',
    mount_path      TEXT NOT NULL DEFAULT '/etc/traefik/dynamic-certs'
)`,
			`INSERT INTO cert_deliveries (id, env_id, volume, uid, gid, traefik, restart_targets,
                synced_hash, synced_at, status, last_error, created_at, created_by, protected, bundle_cas)
             SELECT id, env_id, volume, uid, gid, traefik, restart_targets,
                synced_hash, synced_at, status, last_error, created_at, created_by, protected, bundle_cas
             FROM cert_deliveries_old`,
			`DROP TABLE cert_deliveries_old`,
			`CREATE INDEX cert_deliveries_env_idx ON cert_deliveries (env_id)`,
			joinTable,
			backfill,
			`DROP TABLE cert_delivery_pairs_tmp`,
		}
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	if err := demoteDuplicateTraefikDeliveries(ctx, tx, s); err != nil {
		return err
	}
	// Partial, because several NON-Traefik deliveries into one volume are legitimate: they
	// write disjoint PEMs and no fragment. Keyed on the volume rather than on a stack
	// because the thing being protected is one file in one directory.
	_, err := tx.ExecContext(ctx,
		`CREATE UNIQUE INDEX cert_deliveries_traefik_volume_uq
             ON cert_deliveries (env_id, volume) WHERE traefik = 1`)
	return err
}

// demoteDuplicateTraefikDeliveries clears the way for 0013's unique index without taking
// the box down. Two Traefik deliveries on one volume are possible in existing data, and
// both have been writing tls.yml over each other — each reporting ok forever, because a
// delivery's synced_hash covers only its OWN desired state. Creating the index on that
// data would abort the migration, and a Daffa that will not start is a Daffa the operator
// cannot use to fix the problem.
//
// So the newest row per (env_id, volume) keeps the fragment and the rest are demoted to
// traefik = 0 with a red status naming the winner. The box comes up, the silent overwrite
// stops, and the operator is told what happened — which is more than they were getting.
func demoteDuplicateTraefikDeliveries(ctx context.Context, tx *sql.Tx, s *Store) error {
	// protected first: the delivery that keeps the console's own edge volume current is the
	// one that must not lose its fragment, whatever an operator added later.
	rows, err := tx.QueryContext(ctx, `SELECT id, env_id, volume FROM cert_deliveries
        WHERE traefik = 1 ORDER BY env_id, volume, protected DESC, created_at DESC, id DESC`)
	if err != nil {
		return err
	}
	type row struct{ id, env, volume string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.env, &r.volume); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Grouping in Go rather than in SQL: the window function that would express "all but
	// the first per group" is not in the common subset, and the set is tiny.
	winner := map[string]string{}
	for _, r := range all {
		key := r.env + "\x00" + r.volume
		if _, taken := winner[key]; !taken {
			winner[key] = r.id // first in order = newest
			continue
		}
		msg := fmt.Sprintf("Traefik rendering was turned off here: delivery %s already writes "+
			"tls.yml into volume %q, and the two were overwriting each other. Move these "+
			"certificates onto that delivery, or give this one its own volume.",
			winner[key], r.volume)
		if _, err := tx.ExecContext(ctx, s.rebind(
			`UPDATE cert_deliveries SET traefik = 0, status = 'error', last_error = ? WHERE id = ?`),
			msg, r.id); err != nil {
			return err
		}
	}
	return nil
}

// stopAfter lets a test bring the schema up to a PARTICULAR migration and no further, so
// it can build a database that looks like a real older deployment and then migrate it
// forward for real. Empty means "apply everything", which is what production does.
//
// A migration is only interesting against data that predates it, and a fresh schema
// proves nothing about that. When migration 0002 lands, its test builds the 0001 world
// with this seam, populates it, and migrates forward for real.
var stopAfter string

func (s *Store) migrate(ctx context.Context) error {
	if s.dialect == Postgres {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdent(s.pgSchema))); err != nil {
			return fmt.Errorf("store: creating schema %s (does the role have CREATE on the database?): %w", s.pgSchema, err)
		}
		// Serialize concurrent starts (multiple replicas, or a restart racing itself).
		if _, err := s.db.ExecContext(ctx, "SELECT pg_advisory_lock(4826141)"); err != nil {
			return fmt.Errorf("store: acquiring migration lock: %w", err)
		}
		defer func() { _, _ = s.db.ExecContext(ctx, "SELECT pg_advisory_unlock(4826141)") }()
	}

	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        name       TEXT PRIMARY KEY,
        applied_at TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("store: reading schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: applying migration %s: %w", m.name, err)
		}
		if m.pg != "" && s.dialect == Postgres {
			if _, err := tx.ExecContext(ctx, m.pg); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: applying migration %s (postgres): %w", m.name, err)
			}
		}
		if m.fn != nil {
			if err := m.fn(ctx, tx, s); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: applying migration %s (fn): %w", m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.rebind("INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)"), m.name, ts(now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: recording migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", m.name, err)
		}
		if stopAfter != "" && m.name == stopAfter {
			return nil
		}
	}
	return nil
}

func quoteIdent(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}
