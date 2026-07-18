package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// deploy orchestrates one deployment: resolve the source, build the bundle, claim the
// deployment, start a runner, and watch it.
//
// The claim comes BEFORE the runner starts and is released only when the runner's exit code has
// been recorded, so two people hitting Deploy at the same moment cannot race a stack into an
// unknown state.
//
// from is non-nil only for a rollback, and it changes where the compose file comes from: the
// stored one on that deployment, not the stack's source. See docs/stacks.md §5.
func (s *Server) deploy(ctx context.Context, stack *store.Stack, action stacks.Action, trigger, userID string, from *store.Deployment) (*store.Deployment, error) {
	eng, err := stacks.EngineFor(stack.Engine)
	if err != nil {
		return nil, err
	}

	// The deployment is claimed FIRST, before anything that can fail.
	//
	// It used to be claimed after the bundle was built, which meant that a deploy failing during
	// PREPARATION — a compose file that will not parse, a git repo that will not clone, a
	// credential that no longer works — produced no record at all. The error came back on the
	// POST, the page showed it as a red line, and a refresh wiped it: there was nothing in the
	// history to click on and nothing to read afterwards.
	//
	// Worse, it never notified. An auto-deploy that a webhook started at 2am, on a compose file
	// somebody had just broken, failed completely silently — which is precisely the deploy you
	// most needed to hear about.
	//
	// Every attempt is now a deployment. Every deployment has a log. Every failure is emitted
	// from one place.
	dep := &store.Deployment{
		StackID:     stack.ID,
		Action:      string(action),
		Engine:      eng.Name(),
		TriggerKind: trigger,
		StartedBy:   userID,
	}
	if from != nil {
		dep.RollbackOf = from.ID
	}
	if err := s.store.ClaimDeployment(ctx, dep); err != nil {
		return nil, err // ErrRunInProgress reaches the handler and becomes a 409
	}

	node, err := s.runnerNode(ctx, stack)
	if err != nil {
		return nil, s.failDeployment(ctx, stack, dep,
			"The environment this stack runs on is not connected, so nothing could be deployed to it.")
	}

	// Down needs no bundle: both engines remove by project name alone (`compose down -p`,
	// `stack rm`), so a stack whose source no longer resolves — or no longer validates —
	// can always be brought down. Building the bundle here once held a swarm stack
	// hostage to its own compose file; worse, the swarm engine's Command() had no down
	// arm at all, so the runner LISTED the services, exited 0, and the deployment
	// recorded "ok" with everything still running.
	if action == stacks.ActionDown || action == stacks.ActionDownVolumes {
		ctrID, err := stacks.StartTeardown(ctx, node, eng, dep.ID, stack.ID, stack.Name,
			action == stacks.ActionDownVolumes)
		if err != nil {
			return nil, s.failDeployment(ctx, stack, dep, "The runner could not be started.\n\n"+err.Error())
		}
		if err := s.store.SetDeploymentContainer(ctx, dep.ID, ctrID); err != nil {
			slog.Error("recording the runner container", "deployment", dep.ID, "err", err)
		}
		dep.RunnerCtrID = ctrID
		go s.watchDeployment(node, dep, stack, "")
		return dep, nil
	}

	bundle, plan, resolved, err := s.bundleForDeploy(ctx, stack, from)
	if err != nil {
		// This is where a bad compose file, an unreachable repo or a dead credential lands.
		// It is the most common way a deploy fails and it used to be the least visible.
		return nil, s.failDeployment(ctx, stack, dep, err.Error())
	}

	// A rollback never rolls back. The hooks it runs are the OLD deployment's — they came
	// with the stored file — but a failing post hook on a rollback triggering another
	// rollback would be a loop with no floor. One automatic step back, then a person.
	if plan.Hooks != nil && from != nil {
		plan.Hooks.RollbackOnFailure = false
	}

	// Linked volume sources sync BEFORE the runner starts: a stack must not come up
	// against config Daffa knows is stale, and the sync's VolumeCreate is what lets an
	// `external: true` volume exist before compose goes looking for it on a fresh node.
	// A failed sync fails the deploy, loudly, as a recorded deployment.
	if isDeploying(action) {
		if err := s.syncStackVolumeSources(ctx, stack); err != nil {
			return nil, s.failDeployment(ctx, stack, dep,
				"A volume source this stack depends on could not be synced, so nothing was deployed.\n\n"+err.Error())
		}
	}

	// What this compose file quietly MEANS on a swarm — chiefly the volume trap: a named volume is
	// node-local, so a rescheduled task finds a fresh empty one and the database is gone.
	//
	// It leads the deploy log rather than trailing it, because a log's last lines are the ones
	// people read when something FAILED, and this is a warning about a deploy that will succeed.
	var warnings string
	if stack.Engine == stacks.SwarmEngine.Name() {
		warnings = stacks.WarningLog(s.swarmWarnings(ctx, stack, bundle))
	}

	// What this deployment is actually shipping — recorded now, so it is on the row even if the
	// runner never comes back.
	if err := s.store.SetDeploymentBundle(ctx, dep.ID, bundle.Hash,
		resolved.CommitSHA, resolved.CommitSubject, resolved.YAML); err != nil {
		slog.Error("recording what the deployment shipped", "deployment", dep.ID, "err", err)
	}
	dep.BundleHash = bundle.Hash
	dep.CommitSHA, dep.CommitSubject = resolved.CommitSHA, resolved.CommitSubject

	// A deploy with hooks is a PIPELINE — pre hooks, the engine, post hooks — and the whole
	// pipeline runs in the background, because its first phase can take as long as a deploy.
	// The plain path below is untouched: a stack without hooks behaves exactly as it always
	// has, including runner-start errors surfacing on the POST.
	if plan.Hooks != nil && isDeploying(action) {
		go s.runHookedDeployment(node, dep, stack, eng, action, bundle, plan, warnings)
		return dep, nil
	}

	ctrID, err := stacks.Start(ctx, node, eng, dep.ID, stack.ID, stack.Name, action, bundle)
	if err != nil {
		// Release the claim: a stack whose runner never started must not be locked out of
		// deploying forever.
		return nil, s.failDeployment(ctx, stack, dep, "The deploy runner could not be started.\n\n"+err.Error())
	}
	if err := s.store.SetDeploymentContainer(ctx, dep.ID, ctrID); err != nil {
		slog.Error("recording the runner container", "deployment", dep.ID, "err", err)
	}
	dep.RunnerCtrID = ctrID

	// Watch it in the background. The HTTP request returns as soon as the runner is launched — a
	// deploy takes minutes, and holding a request open for it would mean a browser timeout could
	// orphan a deploy that is running perfectly well.
	go s.watchDeployment(node, dep, stack, warnings)

	return dep, nil
}

// runnerNode is the daemon a stack's runner container is started on — and therefore the daemon its
// containers land on.
//
// Two cases, one function, no branches in the handlers:
//
//   - a PINNED stack (NodeID set) runs on that node, always. Its containers land there, and a
//     change of Swarm leadership is irrelevant to it. That independence is the entire reason
//     placement is a stored column rather than something worked out at deploy time.
//   - anything else runs on the environment's control node: the one node of a standalone
//     environment, or a manager of a swarm — which is what `docker stack deploy` requires anyway.
//
// Resolving it anywhere else would mean a compose deploy landing on whichever manager we happened
// to be talking to, so that a later change of leadership would silently put the next deploy on a
// different machine from the containers it is meant to be updating.
func (s *Server) runnerNode(ctx context.Context, stack *store.Stack) (*dockerx.Node, error) {
	env, err := s.pool.Get(stack.EnvID)
	if err != nil {
		return nil, err
	}

	// A swarm stack needs a Swarm — and it is checked HERE, not only at create time, because an
	// environment's kind can change underneath a stack. Somebody runs `docker swarm leave` on the
	// box, and the next deploy of a stack that says "swarm" would otherwise be handed the one
	// remaining daemon and told to run `docker stack deploy` against it, which fails deep inside
	// the runner with an error about no swarm manager — true, but three layers from the fact.
	if stack.Engine == stacks.SwarmEngine.Name() && !env.IsSwarm() {
		return nil, fmt.Errorf("this stack deploys with Swarm, but its environment is no longer a Swarm cluster")
	}

	// A pinned stack runs on ITS node, always. That independence is the whole reason placement is
	// stored: a change of Swarm leadership must not move somebody's containers.
	if stack.NodeID != "" {
		return env.Node(stack.NodeID)
	}
	return env.Control()
}

// watchDeployment collects a runner's verdict. It deliberately does not hold the request's
// context: the deployment outlives the request that started it, and cancelling on disconnect
// would abandon a deploy the moment someone closed a tab.
func (s *Server) watchDeployment(node *dockerx.Node, dep *store.Deployment, stack *store.Stack, warnings string) {
	ctx, cancel := stacks.DeployContext(context.Background())
	defer cancel()

	result, err := stacks.Wait(ctx, node, dep.RunnerCtrID)
	if err != nil {
		// A timed-out or un-waitable runner was NOT removed by Wait, and nothing else is
		// watching it. Reap it before we declare the deploy failed, or a root-capable container
		// keeps mutating the stack behind a "failed" verdict.
		stacks.Reap(ctx, node, dep.RunnerCtrID)
		slog.Error("waiting for the runner", "deployment", dep.ID, "stack", stack.Name, "err", err)
		_, _ = s.store.FinishDeployment(ctx, dep.ID, 1, "the runner could not be waited on: "+err.Error(), false)
		return
	}

	s.finishDeploy(ctx, dep, stack, result.ExitCode, warnings+result.Log, result.Truncated)
}

// finishDeploy records a deployment's outcome and does everything that outcome implies —
// the notification, the stack's live hash, the audit entry. It is the ONE funnel: the
// plain path and the hook pipeline both end here, so a new way to finish a deployment
// cannot forget to notify. It returns the status so a caller with a next step (an
// automatic rollback) can decide on it.
func (s *Server) finishDeploy(ctx context.Context, dep *store.Deployment, stack *store.Stack, exitCode int, log string, truncated bool) string {
	// FinishDeployment decides the status, because it is the only place that can: a killed
	// runner and a broken one both exit non-zero, and only the cancel flag in the database tells
	// them apart.
	status, err := s.store.FinishDeployment(ctx, dep.ID, exitCode, log, truncated)
	if err != nil {
		slog.Error("recording the deployment result", "deployment", dep.ID, "err", err)
		return ""
	}

	// Every deploy outcome funnels through here, including the ones a webhook started at 2am
	// with nobody watching.
	//
	// Except a cancellation. Somebody stopped that on purpose, and paging the team about a
	// decision they just made is how a channel gets muted.
	if status != store.DeployCancelled {
		s.notifyDeploy(ctx, stack, dep, exitCode, log)
	}

	// Only a SUCCESSFUL deploy updates what is considered live. A failed or cancelled one leaves
	// the previous hash and commit in place, so the UI keeps saying "the source has changed" —
	// which is true, and is what the operator needs to see.
	if status == store.DeployOK && isDeploying(stacks.Action(dep.Action)) {
		if err := s.store.MarkStackDeployed(ctx, stack.ID, dep.BundleHash, dep.CommitSHA); err != nil {
			slog.Error("marking the stack deployed", "stack", stack.ID, "err", err)
		}
	}

	outcome := map[string]string{
		store.DeployOK:        "ok",
		store.DeployFailed:    "error",
		store.DeployCancelled: "cancelled",
	}[status]
	_ = s.store.Audit(ctx, store.AuditEntry{
		UserID: dep.StartedBy, EnvID: stack.EnvID,
		Action: "stack." + dep.Action, Target: stack.Name, Outcome: outcome,
		Detail: store.AuditDetail(map[string]any{
			"deployment": dep.ID, "exit_code": exitCode, "trigger": dep.TriggerKind,
		}),
	})
	slog.Info("deployment finished", "stack", stack.Name, "action", dep.Action,
		"status", status, "exit_code", exitCode)
	return status
}

// isDeploying reports whether an action changes what is running from the source — i.e. whether
// it makes the current bundle the live one. `stop` and `restart` do not: they act on what is
// already deployed.
func isDeploying(a stacks.Action) bool {
	return a == stacks.ActionUp || a == stacks.ActionPull
}

// bundleForDeploy resolves what a deployment will actually ship.
//
// For a rollback it does NOT go back to git. The whole value of storing the resolved compose
// file on the deployment is that putting back a known-good state cannot be blocked by a moved
// branch, a deleted tag, or a repo that is down — which are, in practice, the exact conditions
// under which somebody wants to roll back.
//
// The env vars and registry credentials are the CURRENT ones either way. A rollback restores a
// compose file, not a whole world; re-injecting an old secret would be a security decision
// nobody asked for.
func (s *Server) bundleForDeploy(ctx context.Context, stack *store.Stack, from *store.Deployment) (*stacks.Bundle, *stacks.HookPlan, *stacks.Resolved, error) {
	if from != nil {
		resolved := &stacks.Resolved{
			YAML:          from.ComposeYAML,
			CommitSHA:     from.CommitSHA,
			CommitSubject: from.CommitSubject,
		}
		bundle, plan, err := s.buildBundle(ctx, stack, resolved)
		return bundle, plan, resolved, err
	}

	resolved, _, err := s.resolveSource(ctx, stack)
	if err != nil {
		return nil, nil, nil, err
	}
	bundle, plan, err := s.buildBundle(ctx, stack, resolved)
	return bundle, plan, resolved, err
}

// bundleFor resolves a stack's CURRENT source to a deployable bundle plus its declared services.
// The detail page uses it to show what would be deployed, and to detect drift.
func (s *Server) bundleFor(ctx context.Context, stack *store.Stack) (*stacks.Bundle, []stacks.Service, error) {
	resolved, services, err := s.resolveSource(ctx, stack)
	if err != nil {
		return nil, nil, err
	}
	bundle, _, err := s.buildBundle(ctx, stack, resolved)
	if err != nil {
		return nil, nil, err
	}
	return bundle, services, nil
}

// resolveSource fetches and validates the stack's compose file.
func (s *Server) resolveSource(ctx context.Context, stack *store.Stack) (*stacks.Resolved, []stacks.Service, error) {
	gitAuth, err := s.gitAuth(ctx, stack.GitCredentialID)
	if err != nil {
		return nil, nil, err
	}

	resolved, err := stacks.Resolve(ctx, stacks.Source{
		Kind:     stack.SourceKind,
		URL:      stack.GitURL,
		Ref:      stack.GitRef,
		Path:     stack.GitPath,
		Auth:     gitAuth,
		YAML:     stack.InlineYAML,
		CABundle: s.managedCABundle(ctx),
	})
	if err != nil {
		return nil, nil, err
	}

	env, err := s.stackEnvPlain(ctx, stack.ID)
	if err != nil {
		return nil, nil, err
	}

	// Validate before shipping: a typo should be an error in the browser, not a failed container
	// on a remote host whose logs someone has to go and read.
	services, err := stacks.Parse(ctx, resolved.YAML, stack.Name, env)
	if err != nil {
		return nil, nil, err
	}
	return resolved, services, nil
}

// buildBundle turns a resolved compose file into the tar the runner receives, deriving
// the hook plan on the way — hooks are re-derived from the file on EVERY deploy, so a
// rollback runs the hooks of the version it is restoring, not of the version that failed.
func (s *Server) buildBundle(ctx context.Context, stack *store.Stack, resolved *stacks.Resolved) (*stacks.Bundle, *stacks.HookPlan, error) {
	env, err := s.stackEnvPlain(ctx, stack.ID)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := s.stackSecretsPlain(ctx, stack.ID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkSecretRefs(ctx, resolved.YAML, stack.Name, env, secrets); err != nil {
		return nil, nil, err
	}
	auths, err := s.registryAuths(ctx, resolved.YAML, stack.Name, env)
	if err != nil {
		return nil, nil, err
	}
	// First deploy (no live hash) switches the hooks file into the mode where the
	// stack's resources do not exist yet. It never feeds the bundle hash — that is
	// computed over the original file — so drift detection cannot tell the difference.
	plan, err := stacks.PlanHooks(ctx, resolved.YAML, stack.Name, env,
		stack.Engine == stacks.SwarmEngine.Name(), stack.DeployedHash == "")
	if err != nil {
		return nil, nil, err
	}
	// The host's default logging is merged into the DEPLOY file, after the hook split and
	// before the bundle. Like the split, it never feeds the hash: a changed host default
	// applies at the next deploy, it does not make every stack on the host read as
	// "source changed". A rollback re-derives from here too, so it picks up the CURRENT
	// host config — same philosophy as env vars on rollback.
	logCfg, err := s.store.EffectiveLogConfig(ctx, stack.EnvID)
	if err != nil {
		return nil, nil, err
	}
	if logCfg != nil {
		plan.DeployYAML, err = stacks.InjectLogging(ctx, plan.DeployYAML, stack.Name, env,
			&stacks.LogConfig{Driver: logCfg.Driver, Opts: logCfg.Opts})
		if err != nil {
			return nil, nil, err
		}
	}
	bundle, err := stacks.BuildPlanned(resolved.YAML, plan, env, secrets, auths)
	return bundle, plan, err
}

// checkSecretRefs refuses a deploy whose compose file points a daffa-secrets/ file at a
// secret that has not been defined — in the browser, naming the fix, rather than letting the
// runner fail thirty seconds later with a bare "file not found" on a remote host. The reverse
// (a stored secret nothing references) is harmless and is surfaced on the Secrets tab, not
// here. See docs/secrets.md §4.
func checkSecretRefs(ctx context.Context, yaml, projectName string, env []stacks.EnvVar, secrets []stacks.Secret) error {
	declared, err := stacks.SecretsFromCompose(ctx, yaml, projectName, env)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(secrets))
	for _, sec := range secrets {
		have[sec.Name] = true
	}
	var missing []string
	for _, d := range declared {
		name, ok := stacks.DaffaSecretRef(d.File)
		if ok && !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("stacks: the compose file references secret(s) %s under daffa-secrets/, "+
			"but they are not defined — add them on the stack's Secrets tab", strings.Join(missing, ", "))
	}
	return nil
}

// stackSecretsPlain unseals a stack's secrets. The plaintext exists only here and in the
// bundle that goes straight into the runner — never in a deployment row. See docs/secrets.md.
func (s *Server) stackSecretsPlain(ctx context.Context, stackID string) ([]stacks.Secret, error) {
	rows, err := s.store.StackSecrets(ctx, stackID)
	if err != nil {
		return nil, err
	}

	out := make([]stacks.Secret, 0, len(rows))
	for _, r := range rows {
		v, err := s.sealer.Open(r.ContentEnc)
		if err != nil {
			return nil, fmt.Errorf("could not decrypt secret %s (was the master key replaced?)", r.Name)
		}
		out = append(out, stacks.Secret{Name: r.Name, Content: v})
	}
	return out, nil
}

// stackEnvPlain unseals a stack's variables. The plaintext exists only here and in the bundle
// that goes straight into the runner.
func (s *Server) stackEnvPlain(ctx context.Context, stackID string) ([]stacks.EnvVar, error) {
	rows, err := s.store.StackEnv(ctx, stackID)
	if err != nil {
		return nil, err
	}

	out := make([]stacks.EnvVar, 0, len(rows))
	for _, r := range rows {
		v, err := s.sealer.Open(r.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("could not decrypt %s (was the master key replaced?)", r.Key)
		}
		out = append(out, stacks.EnvVar{Key: r.Key, Value: v})
	}
	return out, nil
}

// registryAuths returns credentials for exactly the registries whose host appears among the
// compose file's images — the deploy analog of registryAuthForHost, but for every image at once.
// A stack pulling from several private registries authenticates to all of them; one pulling only
// public images gets no entries (and so no config.json), which is correct — anonymous pulls need
// no credential. A stored credential the compose never references is never handed to the runner.
func (s *Server) registryAuths(ctx context.Context, yaml, project string, env []stacks.EnvVar) ([]*stacks.RegistryAuth, error) {
	services, err := stacks.Parse(ctx, yaml, project, env)
	if err != nil {
		return nil, err
	}

	// The distinct registry hosts this deploy actually pulls from. An image whose ref cannot be
	// parsed (an unresolved ${VAR}, or a build-only service) authenticates nothing, so skip it.
	want := map[string]bool{}
	for _, svc := range services {
		ref, err := dockerx.ParseImageRef(svc.Image)
		if err != nil {
			continue
		}
		want[dockerx.RegistryHost(ref.Host)] = true
	}
	if len(want) == 0 {
		return nil, nil
	}

	regs, err := s.store.ListRegistries(ctx)
	if err != nil {
		return nil, err
	}
	var out []*stacks.RegistryAuth
	for _, reg := range regs {
		if !want[dockerx.RegistryHost(reg.URL)] {
			continue
		}
		pw, err := s.sealer.Open(reg.PasswordEnc)
		if err != nil {
			return nil, fmt.Errorf("could not decrypt the credential for %s", reg.Name)
		}
		out = append(out, &stacks.RegistryAuth{
			URL: registryConfigKey(reg.URL), Username: reg.Username, Password: pw,
		})
	}
	return out, nil
}

// registryConfigKey is the key a credential is stored under in the runner's config.json, so the
// docker CLI finds it when it pulls. It is the registry HOST, not the raw stored URL (scheme and
// path stripped, all Docker Hub spellings collapsed) — with one exception: Hub credentials must
// live under the legacy `https://index.docker.io/v1/` key the engine looks them up by, not
// `docker.io`.
func registryConfigKey(url string) string {
	host := dockerx.RegistryHost(url)
	if host == "docker.io" {
		return "https://index.docker.io/v1/"
	}
	return host
}

// ReapOrphanedRuns reattaches to runners that were still going when the server stopped.
//
// This is the payoff of running deploys in detached containers: Daffa can be recreated BY ONE OF
// ITS OWN DEPLOYS, come back up, find the runner that recreated it still running, and report how
// it turned out. Without this, redeploying Daffa's own stack would always look like a deployment
// that vanished.
func (s *Server) ReapOrphanedRuns(ctx context.Context) {
	deps, err := s.store.UnfinishedDeployments(ctx)
	if err != nil {
		slog.Error("looking for unfinished deployments", "err", err)
		return
	}

	for _, dep := range deps {
		stack, err := s.store.StackByID(ctx, dep.StackID)
		if err != nil {
			continue
		}
		node, err := s.runnerNode(ctx, stack)
		if err != nil {
			continue // the environment is not connected yet; a later restart can reap it
		}

		// Hook runners are labelled apart from deploy runners ON PURPOSE, and are never
		// adopted: a pre-deploy hook that exits 0 is not a deployment that succeeded, and
		// the pipeline that knew what came next died with the process. Sweep them, and let
		// the deployment be judged by its MAIN runner — or failed for the lack of one.
		stacks.RemoveHookRunners(ctx, node, dep.ID)

		if dep.RunnerCtrID == "" {
			// The server died between claiming the deployment and recording the container.
			// Find it by label instead.
			id, ok := stacks.FindRunner(ctx, node, dep.ID)
			if !ok {
				_, _ = s.store.FinishDeployment(ctx, dep.ID, 1,
					"the runner could not be found after a restart (if this deployment was mid-hook, the pipeline died with the server — deploy again)", false)
				continue
			}
			dep.RunnerCtrID = id
		}

		slog.Info("reattaching to a runner that outlived a restart", "deployment", dep.ID, "stack", stack.Name)
		// No warnings on a reattach: this deploy was already started, and the file was already read
		// once. Re-deriving them here would prepend a second copy to a log that is half written.
		go s.watchDeployment(node, dep, stack, "")
	}
}

// Deployment retention. Every deployment carries its whole log, and nothing used to delete any
// of them, so the database grew without bound — worst on the busiest stack, which is the one you
// can least afford to have slow. See docs/stacks.md §7.
//
// The two rules are a union: keep the last N of every stack, AND keep everything recent. A stack
// deployed fifty times this morning keeps this morning; a stack deployed twice last year still
// has both.
const (
	deploymentsKeptPerStack = 50
	deploymentsMaxAge       = 90 * 24 * time.Hour
	deploymentPruneInterval = 6 * time.Hour
)

// pruneDeployments sweeps at startup and every few hours after that — the same shape as
// monitor.Retention, and for the same reason: a sweep that has to pick a time of day is a sweep
// a nightly reboot can skip forever.
func (s *Server) pruneDeployments(ctx context.Context) {
	sweep := func() {
		n, err := s.store.PruneDeployments(ctx, deploymentsKeptPerStack, deploymentsMaxAge)
		if err != nil {
			slog.Error("pruning deployments", "err", err)
			return
		}
		if n > 0 {
			slog.Info("pruned old deployments", "count", n)
		}
	}
	sweep()

	t := time.NewTicker(deploymentPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// notifyDeploy tells whoever asked to be told. It never fails the deploy: a notification that
// could not be queued is a logged problem, not an outage.
func (s *Server) notifyDeploy(ctx context.Context, stack *store.Stack, dep *store.Deployment, exit int, log string) {
	failed := exit != 0

	event := notify.DeploySucceeded
	if failed {
		event = notify.DeployFailed
	}

	host := s.envName(ctx, stack.EnvID)
	verb := map[bool]string{true: "failed", false: "succeeded"}[failed]

	d := notify.Data{
		Event:    event,
		Subject:  fmt.Sprintf("Deploy %s: %s on %s", verb, stack.Name, host),
		Title:    fmt.Sprintf("Deploy %s: %s", verb, stack.Name),
		HostName: host,
		Target:   stack.Name,
		// The deployment, not the stack. A failure email whose link lands on the stack page
		// makes the reader go and find the deploy it is telling them about.
		Link:   "/deployments/" + dep.ID,
		Failed: failed,
	}

	what := fmt.Sprintf("The %s of stack %q on %s", dep.Action, stack.Name, host)
	if dep.CommitSHA != "" {
		what += fmt.Sprintf(" (%s %s)", shortSHA(dep.CommitSHA), dep.CommitSubject)
	}

	if failed {
		d.Summary = fmt.Sprintf("%s exited with code %d.", what, exit)
		// The TAIL of the log, not the head: the last lines say why it failed, and a
		// head-truncated compose log is a wall of "Pulling" and nothing else.
		d.Detail = notify.Tail(log, 25, 4000)
	} else {
		d.Summary = what + " completed."
	}

	s.notify.Send(ctx, stack.EnvID, d)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// envName resolves a host's name for display. A notification that says the host id is a
// notification nobody can act on.
func (s *Server) envName(ctx context.Context, envID string) string {
	if e, err := s.store.EnvironmentByID(ctx, envID); err == nil {
		return e.Name
	}
	return envID
}

// ErrHostUnreachable is returned when a stack cannot be torn down because its host is not
// connected. It is not a failure of the teardown — it is the absence of anywhere to run it.
var ErrHostUnreachable = errors.New("the host this stack runs on is not connected")

// tearDown removes a stack's containers and networks, and WAITS for the verdict.
//
// Unlike a deploy, the caller cannot be told "started, check back later". Deleting a stack is
// two things that must not come apart: remove what it deployed, then forget it. If the second
// happens without the first, the containers become orphans that nothing is managing and nothing
// can name — which is exactly the state a stack delete used to leave behind.
//
// So this blocks, and a failure here means the stack row STAYS. A stack you could not clean up
// is a stack you must not forget.
func (s *Server) tearDown(ctx context.Context, stack *store.Stack, volumes bool, userID string) error {
	eng, err := stacks.EngineFor(stack.Engine)
	if err != nil {
		return err
	}

	node, err := s.runnerNode(ctx, stack)
	if err != nil {
		return ErrHostUnreachable
	}

	// ASK the daemon, do not merely look it up. The pool hands back a client for the local host
	// whether or not anything is listening on the other end of the socket, so a dead daemon looks
	// exactly like a live one until you try to use it — and then it surfaces as some deep error
	// about pulling the runner image, which tells an operator nothing and, worse, hides the fact
	// that the one thing they CAN do is forget the stack.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := node.Ping(pingCtx); err != nil {
		return ErrHostUnreachable
	}

	// No bundle. `compose down` works from the project name alone, so removing a stack does not
	// depend on its source still being resolvable — see stacks.StartTeardown.
	action := "down"
	if volumes {
		action = "down+volumes"
	}
	dep := &store.Deployment{
		StackID: stack.ID, Action: action, Engine: eng.Name(),
		TriggerKind: store.TriggerManual, StartedBy: userID,
	}
	if err := s.store.ClaimDeployment(ctx, dep); err != nil {
		return err // ErrRunInProgress becomes a 409: do not delete a stack mid-deploy
	}

	ctrID, err := stacks.StartTeardown(ctx, node, eng, dep.ID, stack.ID, stack.Name, volumes)
	if err != nil {
		_, _ = s.store.FinishDeployment(ctx, dep.ID, 1, "could not start the runner: "+err.Error(), false)
		return err
	}
	if err := s.store.SetDeploymentContainer(ctx, dep.ID, ctrID); err != nil {
		slog.Error("recording the teardown runner", "deployment", dep.ID, "err", err)
	}

	// Not the request's context: a browser that gives up must not abandon a teardown halfway,
	// which would leave some containers gone and some not, and the stack still on the books.
	waitCtx, cancel := stacks.DeployContext(context.Background())
	defer cancel()

	result, err := stacks.Wait(waitCtx, node, ctrID)
	if err != nil {
		// See watchDeployment: Wait leaves the runner in place on this path, so reap it.
		stacks.Reap(waitCtx, node, ctrID)
		_, _ = s.store.FinishDeployment(waitCtx, dep.ID, 1, "the runner could not be waited on: "+err.Error(), false)
		return err
	}
	if _, err := s.store.FinishDeployment(waitCtx, dep.ID, result.ExitCode, result.Log, result.Truncated); err != nil {
		slog.Error("recording the teardown result", "deployment", dep.ID, "err", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("compose down failed (exit %d): %s", result.ExitCode, notify.Tail(result.Log, 6, 400))
	}
	return nil
}

// failDeployment closes a deployment that never reached the runner, and makes it look exactly
// like one that did: a failed row in the history, with the reason as its log, an audit entry, and
// a notification.
//
// That sameness is the point. An operator should not have to know WHERE a deploy fell over in
// order to find out WHY — "the compose file will not parse" and "the container exited 1" are both
// just a failed deploy with a log, and they belong in the same list, read the same way.
//
// It returns the error so the caller can `return nil, s.failDeployment(...)` and be sure the
// deployment was closed on every path out.
func (s *Server) failDeployment(ctx context.Context, stack *store.Stack, dep *store.Deployment, reason string) error {
	if _, err := s.store.FinishDeployment(ctx, dep.ID, 1, reason, false); err != nil {
		slog.Error("recording a failed deployment", "deployment", dep.ID, "err", err)
	}

	s.notifyDeploy(ctx, stack, dep, 1, reason)

	_ = s.store.Audit(ctx, store.AuditEntry{
		UserID: dep.StartedBy, EnvID: stack.EnvID,
		Action: "stack." + dep.Action, Target: stack.Name, Outcome: "error",
		Detail: store.AuditDetail(map[string]any{"deployment": dep.ID, "error": reason}),
	})

	slog.Warn("deployment failed before it started",
		"stack", stack.Name, "action", dep.Action, "reason", reason)

	return errors.New(reason)
}
