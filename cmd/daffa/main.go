// Command daffa is a lean container operations and deployment console.
//
//	daffa serve                      run the server
//	daffa user add -u NAME --role R  create a local account
//	daffa user list                  list accounts and their roles
//	daffa user role -u NAME --role R grant a role to an existing account
//	daffa user passwd -u NAME        change a password
//	daffa admin-token                print a one-time break-glass sign-in URL
//	daffa openapi                    print the API's OpenAPI 3.1 description
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Mnshahawy/daffa/internal/agent"
	"github.com/Mnshahawy/daffa/internal/api"
	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/cli"
	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
	"golang.org/x/term"
)

// version is stamped at build time (-ldflags "-X main.version=…"). The agent reports it
// so an operator can spot a fleet member that never got upgraded.
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "agent":
		err = agentCmd()
	case "user":
		err = userCmd(os.Args[2:])
	case "restore":
		err = restoreCmd()
	case "admin-token":
		err = adminToken()
	case "edge":
		err = edgeCmd(os.Args[2:])
	case "stack":
		err = stackCmd(os.Args[2:])
	case "openapi":
		// The embedded spec, pipeable into external tooling without authentication —
		// `daffa openapi | npx @redocly/cli lint -` and the like.
		_, err = os.Stdout.Write(api.OpenAPISpec())
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "daffa: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `daffa — container operations and deployment console

  daffa serve                          run the server (default)
  daffa agent --server URL --token T   run the agent on a managed host
  daffa restore --job J --snapshot S   restore a database backup (see below)
  daffa user add -u NAME --role ROLE [--on HOST]   create a local account
  daffa user list                      list accounts and the roles they hold
  daffa user role -u NAME --role ROLE [--on HOST]  grant a role, optionally on one host
  daffa user passwd -u NAME            change an account's password
  daffa admin-token                    print a one-time break-glass sign-in URL
  daffa edge init --domain D --volume V   issue an internal-CA edge certificate and print its trust bundle
  daffa stack adopt [--name N]         record the running compose deployment as an editable Daffa stack
  daffa openapi                        print the API's OpenAPI 3.1 description

Restore runs here, not in the browser: an encrypted snapshot needs your age PRIVATE
key, and that key must never reach the server — otherwise the machine taking the
backups could also read them.

Configuration is entirely by environment; see README.md.
`)
}

// open brings up the store and config, shared by every subcommand.
func open() (*config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(context.Background(), cfg.DBURL)
	if err != nil {
		return nil, nil, err
	}
	return cfg, st, nil
}

func serve() error {
	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The local Docker socket is one NODE, in a standalone environment of its own. Agents add
	// more nodes; a Swarm gathers them into one environment. Nothing else in the system knows
	// the difference — which is the whole point of dockerx.
	pool := dockerx.NewPool()
	defer pool.Close()

	// "Local" rather than "local": it is an environment in a list of environments, and it should
	// read like one. An admin can rename it.
	localEnv, localNode, err := st.UpsertLocalEnvironment(ctx, "Local", cfg.DockerHost)
	if err != nil {
		return err
	}
	if err := pool.Register(localEnv, localNode); err != nil {
		return err
	}
	if env, err := pool.Get(localEnv.ID); err == nil {
		if node, err := env.Node(localNode.ID); err == nil {
			if err := node.Ping(ctx); err != nil {
				// Not fatal: the console should still come up and say the daemon is down,
				// which is more useful than refusing to start.
				slog.Warn("local Docker daemon is not reachable", "host", cfg.DockerHost, "err", err)
			} else {
				slog.Info("connected to Docker", "host", cfg.DockerHost)
			}
		}
	}

	// Identity providers are rows now, built lazily on first use — so a provider whose
	// issuer is temporarily unreachable no longer stops the whole console from starting.
	providers, err := st.ListOIDCProviders(ctx)
	if err != nil {
		return err
	}
	enabled := 0
	for _, p := range providers {
		if p.Enabled {
			enabled++
		}
	}

	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	switch {
	case n == 0 && cfg.LocalAuth:
		slog.Warn("no accounts exist yet — create one with: daffa user add -u <name> --role Admin")
	case n == 0:
		// No users, no password login, and providers can only be added from inside: the
		// only way in is break-glass. Say so now rather than at 3am.
		slog.Warn("no accounts exist and password sign-in is off — " +
			"the only way in is: daffa admin-token")
	case enabled == 0 && !cfg.LocalAuth:
		slog.Warn("password sign-in is off and no identity provider is enabled — " +
			"nobody can sign in; recover with: daffa admin-token")
	}
	slog.Info("auth", "local", cfg.LocalAuth, "identity_providers", enabled)

	sealer, err := config.NewSealer(cfg.MasterKey)
	if err != nil {
		return err
	}

	apiServer := api.NewServer(cfg, st, pool, sealer)

	// Reattach to deploys that outlived the last process (possibly the very deploy that
	// recreated it), and load the backup schedule.
	apiServer.Start(ctx)
	defer apiServer.Stop()

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: apiServer.Handler(),
		// No WriteTimeout: SSE streams are long-lived by design and a write deadline
		// would sever them mid-follow. Idle and read deadlines still bound the rest.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Expired sessions accumulate otherwise; nothing else reaps them.
	go reapSessions(ctx, st)

	errc := make(chan error, 1)
	go func() {
		slog.Info("daffa listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ── edge ──────────────────────────────────────────────────────────────────────

// edgeCmd issues the console's own edge TLS from an internal CA, for a domain that a
// public ACME challenge cannot reach (a private hostname, split-horizon DNS). It runs
// in-process against the same database and Docker socket the server uses — like
// `user add`, shell access is the authorization — and prints the CA trust bundle to
// install on client machines. Idempotent: safe to re-run.
func edgeCmd(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: daffa edge init --domain <name> [--volume <name>] [--out <file>]")
	}
	fs := flag.NewFlagSet("edge init", flag.ExitOnError)
	domain := fs.String("domain", os.Getenv("DAFFA_EDGE_DOMAIN"), "hostname the edge certificate serves")
	volume := fs.String("volume", env("DAFFA_EDGE_VOLUME", "daffa-edge-certs"), "volume Traefik reads dynamic certs from")
	out := fs.String("out", "", "write the CA trust bundle here (default: stdout)")
	caDays := fs.Int("ca-days", 3650, "CA validity in days")
	certDays := fs.Int("cert-days", 397, "certificate validity in days")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*domain) == "" {
		return errors.New("edge init: --domain is required")
	}

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The delivery is written into a volume on the local host, so we need the same Docker
	// wiring serve builds: the pool, and the local environment registered on it.
	pool := dockerx.NewPool()
	defer pool.Close()
	localEnv, localNode, err := st.UpsertLocalEnvironment(ctx, "Local", cfg.DockerHost)
	if err != nil {
		return err
	}
	if err := pool.Register(localEnv, localNode); err != nil {
		return err
	}
	sealer, err := config.NewSealer(cfg.MasterKey)
	if err != nil {
		return err
	}
	srv := api.NewServer(cfg, st, pool, sealer)

	res, err := srv.BootstrapEdgeCert(ctx, api.EdgeCertOptions{
		Domain: *domain, EnvID: localEnv.ID, Volume: *volume,
		CADays: *caDays, CertDays: *certDays,
	})
	if err != nil {
		return err
	}

	if *out != "" {
		// The trust bundle is public material — a CA certificate, no key — so 0644 is right.
		if err := os.WriteFile(*out, []byte(res.TrustBundlePEM), 0o644); err != nil {
			return fmt.Errorf("edge init: writing trust bundle: %w", err)
		}
		fmt.Fprintf(os.Stderr, "edge certificate ready for %s (volume %s); trust bundle written to %s\n",
			res.Domain, res.Volume, *out)
		return nil
	}
	fmt.Print(res.TrustBundlePEM)
	return nil
}

// ── stack ─────────────────────────────────────────────────────────────────────

func stackCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: daffa stack <adopt|config> ...")
	}
	switch args[0] {
	case "adopt":
		return stackAdoptCmd(args[1:])
	case "config":
		return stackConfigCmd(args[1:])
	default:
		return fmt.Errorf("unknown stack subcommand %q (want adopt or config)", args[0])
	}
}

// stackAdoptCmd adopts the running compose deployment as an editable Daffa stack — the
// installer brings the stack up with `docker compose up`, and this records it so the
// console's stack features (drift, redeploy, env editing) apply. It reads the compose file
// and env from the deployment directory, mounted read-only into this container.
func stackAdoptCmd(args []string) error {
	fs := flag.NewFlagSet("stack adopt", flag.ExitOnError)
	name := fs.String("name", env("DAFFA_STACK_NAME", "daffa"), "stack (compose project) name — must match what is running")
	composePath := fs.String("compose", env("DAFFA_STACK_COMPOSE", "/etc/daffa/docker-compose.yml"), "compose file to adopt")
	envFile := fs.String("env-file", env("DAFFA_STACK_ENVFILE", "/etc/daffa/.env"), "env file whose values become the stack's env")
	if err := fs.Parse(args); err != nil {
		return err
	}

	yaml, err := os.ReadFile(*composePath)
	if err != nil {
		return fmt.Errorf("stack adopt: reading compose: %w", err)
	}
	kv, err := parseEnvFile(*envFile)
	if err != nil {
		return err
	}

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	// The stack runs on the local host. UpsertLocalEnvironment gives us its id (and is what
	// serve calls too), without needing a live Docker connection — adopt touches no daemon.
	localEnv, _, err := st.UpsertLocalEnvironment(ctx, "Local", cfg.DockerHost)
	if err != nil {
		return err
	}
	sealer, err := config.NewSealer(cfg.MasterKey)
	if err != nil {
		return err
	}
	srv := api.NewServer(cfg, st, dockerx.NewPool(), sealer)

	res, err := srv.AdoptStack(ctx, api.AdoptStackOptions{
		Name: *name, EnvID: localEnv.ID, ComposeYAML: string(yaml), Env: kv,
	})
	if err != nil {
		return err
	}
	verb := "re-adopted"
	if res.Created {
		verb = "adopted"
	}
	fmt.Fprintf(os.Stderr, "%s stack %q (%d env vars)\n", verb, *name, len(kv))
	return nil
}

// stackConfigCmd puts the deployment's Traefik configuration under Daffa's management: it
// creates inline volume sources so traefik.yml and the dynamic middlewares directory become
// editable in the console and re-delivered on every deploy. Idempotent. This is what makes
// "change a middleware and redeploy" possible for a stack that has no git repo.
func stackConfigCmd(args []string) error {
	fs := flag.NewFlagSet("stack config", flag.ExitOnError)
	stackName := fs.String("stack", env("DAFFA_STACK_NAME", "daffa"), "stack to link the config sources to")
	traefikFile := fs.String("traefik", env("DAFFA_TRAEFIK_CONFIG", "/etc/daffa/traefik.yml"), "the static traefik.yml to seed under management")
	configVol := fs.String("config-volume", env("DAFFA_TRAEFIK_CONFIG_VOLUME", "daffa-traefik-config"), "volume holding traefik.yml (mounted at /etc/traefik)")
	dynamicVol := fs.String("dynamic-volume", env("DAFFA_TRAEFIK_DYNAMIC_VOLUME", "daffa-edge-certs"), "volume the file provider watches (dynamic config + certs)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	traefikYAML, err := os.ReadFile(*traefikFile)
	if err != nil {
		return fmt.Errorf("stack config: reading traefik.yml: %w", err)
	}

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	pool := dockerx.NewPool()
	defer pool.Close()
	localEnv, localNode, err := st.UpsertLocalEnvironment(ctx, "Local", cfg.DockerHost)
	if err != nil {
		return err
	}
	if err := pool.Register(localEnv, localNode); err != nil {
		return err
	}
	sealer, err := config.NewSealer(cfg.MasterKey)
	if err != nil {
		return err
	}
	srv := api.NewServer(cfg, st, pool, sealer)

	// Link the sources to the stack so a deploy delivers them before the runner starts —
	// a static traefik.yml change then takes effect on the same redeploy that restarts it.
	var stackID string
	if stacks, err := st.ListStacks(ctx, false, []string{localEnv.ID}); err == nil {
		for _, s := range stacks {
			if s.Name == *stackName {
				stackID = s.ID
				break
			}
		}
	}

	// The static config: traefik.yml in its own volume, mounted at /etc/traefik.
	if err := srv.EnsureInlineVolumeSource(ctx, api.InlineVolumeSourceOptions{
		EnvID: localEnv.ID, Volume: *configVol, StackID: stackID,
		Files: []store.VolSourceFile{{Path: "traefik.yml", Content: string(traefikYAML)}},
	}); err != nil {
		return err
	}

	// A place for dynamic config (middlewares, routers) in the volume the file provider
	// watches — seeded with an example so there is somewhere obvious to edit. It shares the
	// volume with delivered certificates; each writer only ever touches its own files.
	if err := srv.EnsureInlineVolumeSource(ctx, api.InlineVolumeSourceOptions{
		EnvID: localEnv.ID, Volume: *dynamicVol, StackID: stackID,
		Files: []store.VolSourceFile{{Path: "middlewares.yml", Content: dynamicConfigExample}},
	}); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "traefik config under management (volumes %s, %s)\n", *configVol, *dynamicVol)
	return nil
}

// dynamicConfigExample is a valid-but-inert dynamic config file: a comment plus an empty
// http block, so Traefik loads it without complaint and the operator has a template.
const dynamicConfigExample = `# Traefik dynamic configuration — edit in Daffa (Volume sources).
# Add middlewares, routers or TLS options here; changes hot-reload, no restart.
# Example:
#   http:
#     middlewares:
#       secure-headers:
#         headers:
#           stsSeconds: 31536000
http: {}
`

// parseEnvFile reads KEY=VALUE lines (skipping blanks and # comments) into env vars,
// flagging the ones whose name looks like a secret so the console hides their values.
func parseEnvFile(path string) ([]api.EnvKV, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("stack adopt: reading env file: %w", err)
	}
	defer f.Close()

	var out []api.EnvKV
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out = append(out, api.EnvKV{Key: k, Value: strings.TrimSpace(v), Secret: isSecretKey(k)})
	}
	return out, sc.Err()
}

func isSecretKey(k string) bool {
	u := strings.ToUpper(k)
	return strings.Contains(u, "PASSWORD") || strings.Contains(u, "SECRET") || strings.Contains(u, "TOKEN")
}

func reapSessions(ctx context.Context, st *store.Store) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.DeleteExpiredSessions(ctx); err != nil {
				slog.Error("reaping expired sessions", "err", err)
			}
			if err := st.DeleteExpiredBreakGlassTokens(ctx); err != nil {
				slog.Error("reaping expired break-glass tokens", "err", err)
			}
			if err := st.DeleteExpiredJoinTokens(ctx); err != nil {
				slog.Error("reaping expired join tokens", "err", err)
			}
		}
	}
}

// ── agent ───────────────────────────────────────────────────────────────────────

// agentCmd runs Daffa on a managed host. Note what it does NOT need: a database, a
// listening port, a certificate, or an open firewall. It dials out, and that is the
// whole security story — the host exposes nothing.
func agentCmd() error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	server := fs.String("server", os.Getenv("DAFFA_SERVER"), "Daffa server base URL, e.g. https://ops.example.com")
	token := fs.String("token", os.Getenv("DAFFA_JOIN_TOKEN"), "one-time join token (first run only)")
	dockerHost := fs.String("docker-host", env("DAFFA_DOCKER_HOST", "unix:///var/run/docker.sock"), "local Docker endpoint to proxy")
	stateFile := fs.String("state", env("DAFFA_AGENT_STATE", "/var/lib/daffa/agent.json"), "where to persist this agent's identity")
	insecure := fs.Bool("insecure", envBool("DAFFA_INSECURE"), "accept the server's certificate without a trusted CA (it is pinned on first use)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return agent.Run(ctx, agent.Config{
		Server:     *server,
		JoinToken:  *token,
		DockerHost: *dockerHost,
		StateFile:  *stateFile,
		Version:    version,
		Insecure:   *insecure,
	})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string) bool {
	v, err := strconv.ParseBool(os.Getenv(k))
	return err == nil && v
}

// ── restore ─────────────────────────────────────────────────────────────────────

// restoreCmd exists as a CLI command, and not as a button in the web UI, on purpose.
//
// Restoring an encrypted snapshot needs the age PRIVATE key. If the server held that
// key — or if a web form asked you to paste it in — then the machine taking the backups
// could also read them, and anyone who compromised that machine would inherit every
// snapshot ever taken. Keeping the key on the operator's laptop is the entire point of
// encrypting to a public key in the first place, so the decryption happens here, and the
// server only ever sees ciphertext going out and plaintext coming back.
func restoreCmd() error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	server := fs.String("server", os.Getenv("DAFFA_SERVER"), "Daffa server URL, e.g. https://ops.example.com")
	job := fs.String("job", "", "backup job id")
	snapshot := fs.String("snapshot", "", "snapshot key, as listed in the console")
	identity := fs.String("identity", os.Getenv("DAFFA_AGE_IDENTITY"), "path to your age private key file (for an encrypted snapshot)")
	user := fs.String("user", os.Getenv("DAFFA_USER"), "Daffa username")
	password := fs.String("password", os.Getenv("DAFFA_PASSWORD"), "Daffa password (prompted if omitted)")
	token := fs.String("token", os.Getenv("DAFFA_TOKEN"), "Daffa API token (used instead of user/password)")
	insecure := fs.Bool("insecure", false, "skip TLS verification (for an internal CA)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	wipe := fs.Bool("wipe", false, "volume restore only: empty the volume first (the server refuses a non-empty volume otherwise)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Restore(ctx, cli.RestoreOptions{
		Server:   *server,
		Job:      *job,
		Snapshot: *snapshot,
		Identity: *identity,
		Username: *user,
		Password: *password,
		Token:    *token,
		Insecure: *insecure,
		Yes:      *yes,
		Wipe:     *wipe,
	})
}

// ── user ────────────────────────────────────────────────────────────────────────

func userCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("user: want a subcommand (add | list | role | passwd)")
	}

	fs := flag.NewFlagSet("user", flag.ExitOnError)
	username := fs.String("u", "", "username")
	roleName := fs.String("role", "", "role name, e.g. Admin, Operator, Viewer")
	onHost := fs.String("on", "", "limit the role to one host, by name (default: everywhere)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	_, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	// findRole resolves a role by name and says what exists when it cannot. Roles are rows
	// now, so there is no fixed list to put in a usage string.
	findRole := func(name string) (*store.Role, error) {
		r, err := st.RoleByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			all, lerr := st.ListRoles(ctx)
			if lerr != nil {
				return nil, lerr
			}
			names := make([]string, 0, len(all))
			for _, x := range all {
				names = append(names, x.Name)
			}
			return nil, fmt.Errorf("no role named %q — existing roles: %s", name, strings.Join(names, ", "))
		}
		return r, err
	}

	// scopeFor turns --on into a scope. Empty means everywhere; a name has to resolve to a
	// host that actually exists, or the grant would silently do nothing.
	scopeFor := func() (store.Scope, error) {
		if *onHost == "" {
			return store.Global(), nil
		}
		env, err := st.EnvironmentByName(ctx, *onHost)
		if err != nil {
			return store.Scope{}, fmt.Errorf("no host named %q", *onHost)
		}
		return store.OnEnv(env.ID), nil
	}

	switch args[0] {
	case "add":
		if *username == "" {
			return errors.New("user add: -u is required")
		}
		if *roleName == "" {
			return errors.New("user add: --role is required (a user with no role can sign in and see nothing)")
		}
		role, err := findRole(*roleName)
		if err != nil {
			return fmt.Errorf("user add: %w", err)
		}

		pw, err := readPassword("Password: ")
		if err != nil {
			return err
		}
		again, err := readPassword("Confirm: ")
		if err != nil {
			return err
		}
		if pw != again {
			return errors.New("user add: passwords do not match")
		}
		if len(pw) < 12 {
			// Short enough to brute force offline if the store ever leaks; the cost of
			// a longer one is a few keystrokes, once.
			return errors.New("user add: password must be at least 12 characters")
		}

		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		sc, err := scopeFor()
		if err != nil {
			return fmt.Errorf("user add: %w", err)
		}

		u := &store.User{Kind: "local", Username: *username, PasswordHash: hash}
		if err := st.CreateUser(ctx, u); err != nil {
			return err
		}
		if err := st.GrantRole(ctx, u.ID, role.ID, store.SourceLocal, sc); err != nil {
			return fmt.Errorf("user add: %w", err)
		}
		fmt.Printf("created %s (%s)\n", *username, describeGrant(role.Name, *onHost))
		return nil

	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("no accounts yet")
			return nil
		}
		for _, u := range users {
			ms, err := st.UserRoles(ctx, u.ID)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(ms))
			for _, m := range ms {
				n := describeGrant(m.Name, m.EnvName)
				if m.Source == store.SourceOIDC {
					n += " (idp)"
				}
				names = append(names, n)
			}
			roles := strings.Join(names, ", ")
			if roles == "" {
				roles = "— no roles —"
			}
			status := ""
			if u.Disabled {
				status = " [disabled]"
			}
			fmt.Printf("%-20s %-8s %s%s\n", u.Label(), u.Kind, roles, status)
		}
		return nil

	// role is the escape hatch the old CLI did not have: before this, changing an
	// existing user's role meant editing the database by hand.
	case "role":
		if *username == "" || *roleName == "" {
			return errors.New("user role: -u and --role are required")
		}
		u, err := st.UserByUsername(ctx, *username)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user role: no such account %q", *username)
		}
		if err != nil {
			return err
		}
		role, err := findRole(*roleName)
		if err != nil {
			return fmt.Errorf("user role: %w", err)
		}
		sc, err := scopeFor()
		if err != nil {
			return fmt.Errorf("user role: %w", err)
		}
		if err := st.GrantRole(ctx, u.ID, role.ID, store.SourceLocal, sc); err != nil {
			return fmt.Errorf("user role: %w", err)
		}
		fmt.Printf("granted %s to %s\n", describeGrant(role.Name, *onHost), u.Label())
		return nil

	case "passwd":
		if *username == "" {
			return errors.New("user passwd: -u is required")
		}
		u, err := st.UserByUsername(ctx, *username)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user passwd: no such account %q", *username)
		}
		if err != nil {
			return err
		}

		pw, err := readPassword("New password: ")
		if err != nil {
			return err
		}
		if len(pw) < 12 {
			return errors.New("user passwd: password must be at least 12 characters")
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.SetUserPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		fmt.Printf("password updated for %s\n", *username)
		return nil

	default:
		return fmt.Errorf("user: unknown subcommand %q (add | list | role | passwd)", args[0])
	}
}

// stdin is buffered once: a fresh bufio.Reader per read would discard whatever it had
// already pulled in, so a script piping two lines would lose the second.
var stdin = bufio.NewReader(os.Stdin)

// readPassword prompts on a terminal with echo off, and otherwise reads a line from
// stdin — so `daffa user add` works both for a human at a shell and for the
// provisioning script that pipes a generated password in.
func readPassword(prompt string) (string, error) {
	if !term.IsTerminal(int(syscall.Stdin)) {
		line, err := stdin.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(b), nil
}

// ── admin-token ─────────────────────────────────────────────────────────────────

// adminToken is the break-glass path: when the IdP is down — quite possibly because
// the stack Daffa manages is the stack the IdP runs in — someone with shell on the
// box mints a single-use admin session and gets back in.
//
// Requiring shell is the authorization: that shell already reaches the Docker socket,
// which is root. The token is stored hashed, expires in ten minutes, and is consumed
// on first use.
func adminToken() error {
	fs := flag.NewFlagSet("admin-token", flag.ExitOnError)
	base := fs.String("url", "", "base URL of this Daffa (e.g. https://ops.example.com); prefixed to the printed link")
	ttl := fs.Duration("ttl", 10*time.Minute, "how long the token stays valid")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	_, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	tok, err := api.MintBreakGlassToken(ctx, st, *ttl)
	if err != nil {
		return err
	}

	fmt.Printf("Single-use admin sign-in link (valid %s):\n\n  %s/api/auth/break-glass?token=%s\n\n",
		*ttl, *base, tok)
	fmt.Fprintln(os.Stderr, "This link grants an admin session once and is then dead. It is not stored anywhere you can read it back.")
	return nil
}

// describeGrant renders "Operator" or "Operator on staging" — the scope is part of what a
// grant IS, so printing the role alone would be a half-truth.
func describeGrant(role, host string) string {
	if host == "" {
		return role
	}
	return role + " on " + host
}
