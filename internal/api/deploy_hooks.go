package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// runHookedDeployment is the pipeline a stack with x-daffa.hooks gets: pre-deploy hooks,
// the engine, post-deploy hooks — one deployment record, one log, one claim held from
// first hook to last verdict. See docs/hooks.md.
//
// The ordering IS the failure semantics. A pre-deploy failure aborts before the engine
// has touched a single running service, so the stack is exactly as it was — that is the
// entire reason migrations belong in pre hooks. A post-deploy failure means the new
// version is live and demonstrably wrong, which is the one situation where an automatic
// step backwards is safer than waiting for a human.
func (s *Server) runHookedDeployment(node *dockerx.Node, dep *store.Deployment, stack *store.Stack,
	eng stacks.Engine, action stacks.Action, bundle *stacks.Bundle, plan *stacks.HookPlan, warnings string) {

	hooks := plan.Hooks

	ctx, cancel := stacks.DeployContext(context.Background())
	defer cancel()

	var log strings.Builder
	log.WriteString(warnings)

	// ── first deploy: provision what the hooks need ─────────────────────────────
	// The stack's networks and volumes do not exist before the first engine run, and a
	// hook is a plain container that must attach to them. On swarm Daffa creates them
	// here, namespace-labelled so `stack deploy` adopts them as its own; on compose the
	// hooks file carries the original definitions and the first hook's `compose run`
	// creates them natively. Either way the engine inherits, and the log says which —
	// "my migration did not run" must keep a findable answer, and so must "where did
	// this network come from".
	if stack.DeployedHash == "" {
		if plan.Provision != nil {
			log.WriteString(phaseHeader("first deploy", "creating the networks and volumes the hooks need; the engine adopts them"))
			if err := provisionHookResources(ctx, node, plan.Provision, &log); err != nil {
				fmt.Fprintf(&log, "%v\nThe pipeline stops here — nothing running has been touched.\n", err)
				s.finishDeploy(ctx, dep, stack, 1, log.String(), false)
				return
			}
		} else if len(hooks.PreDeploy) > 0 {
			log.WriteString(phaseHeader("first deploy", "the hooks create the stack's networks and volumes; the engine adopts them"))
		}
	}

	// ── pre-deploy ──────────────────────────────────────────────────────────────
	for _, hook := range hooks.PreDeploy {
		if s.runHookPhase(ctx, node, dep, stack, "pre-deploy", hook.Service, bundle, hooks, &log) {
			continue
		}
		if !hook.Blocking() {
			fmt.Fprintf(&log, "hook %q is marked on_failure: continue — proceeding despite the failure.\n", hook.Service)
			continue
		}
		log.WriteString("The pipeline stops here — nothing running has been touched.\n")
		s.finishDeploy(ctx, dep, stack, 1, log.String(), false)
		return
	}

	// ── the engine ──────────────────────────────────────────────────────────────
	log.WriteString(phaseHeader("deploy", string(action)))
	ctrID, err := stacks.Start(ctx, node, eng, dep.ID, stack.ID, stack.Name, action, bundle)
	if err != nil {
		log.WriteString("The deploy runner could not be started.\n\n" + err.Error() + "\n")
		s.finishDeploy(ctx, dep, stack, 1, log.String(), false)
		return
	}
	if err := s.store.SetDeploymentContainer(ctx, dep.ID, ctrID); err != nil {
		slog.Error("recording the runner container", "deployment", dep.ID, "err", err)
	}
	dep.RunnerCtrID = ctrID

	result, err := stacks.Wait(ctx, node, ctrID)
	if err != nil {
		log.WriteString("the runner could not be waited on: " + err.Error() + "\n")
		s.finishDeploy(ctx, dep, stack, 1, log.String(), false)
		return
	}
	log.WriteString(result.Log)
	log.WriteString("\n")
	if result.ExitCode != 0 {
		// Failed or cancelled — finishDeploy tells them apart. Either way the post hooks
		// have nothing to verify.
		s.finishDeploy(ctx, dep, stack, result.ExitCode, log.String(), result.Truncated)
		return
	}

	// ── post-deploy ─────────────────────────────────────────────────────────────
	for _, hook := range hooks.PostDeploy {
		if s.runHookPhase(ctx, node, dep, stack, "post-deploy", hook.Service, bundle, hooks, &log) {
			continue
		}
		if !hook.Blocking() {
			// Declared best-effort: worth a line in the log, not worth failing a release
			// over — and never a reason to roll one back.
			fmt.Fprintf(&log, "hook %q is marked on_failure: continue — the deploy stands despite the failure.\n", hook.Service)
			continue
		}
		// The engine succeeded and the verification did not: the deployment is FAILED —
		// the stack's live hash stays on the previous version, which is the truth the
		// operator needs — and, if asked, the previous version comes back by itself.
		log.WriteString("The pipeline stops here — the new version is live but failed its verification.\n")
		status := s.finishDeploy(ctx, dep, stack, 1, log.String(), false)
		if status == store.DeployFailed && hooks.RollbackOnFailure {
			s.autoRollback(ctx, stack, dep)
		}
		return
	}

	s.finishDeploy(ctx, dep, stack, 0, log.String(), false)
}

// provisionHookResources creates the swarm first-deploy resources from the plan. Create,
// not converge: a resource that already exists is left exactly as found — it is either
// the operator's (adopt it, the way the engine would) or debris from a failed earlier
// first deploy (which, carrying the namespace label, is indistinguishable from ours and
// equally usable). Each creation is logged, so the resources' origin stays findable.
func provisionHookResources(ctx context.Context, node *dockerx.Node, prov *stacks.HookProvision, log *strings.Builder) error {
	for _, n := range prov.Networks {
		if _, err := node.Client.NetworkInspect(ctx, n.Name, network.InspectOptions{}); err == nil {
			fmt.Fprintf(log, "network %s already exists — using it as is\n", n.Name)
			continue
		}
		opts := network.CreateOptions{
			Driver:     n.Driver,
			Attachable: n.Attachable,
			Internal:   n.Internal,
			Options:    n.Options,
			Labels:     n.Labels,
		}
		if opts.Driver == "" {
			opts.Driver = "overlay" // stack deploy's own default
		}
		if n.IpamDriver != "" || len(n.Subnets) > 0 {
			ipam := &network.IPAM{Driver: n.IpamDriver}
			for _, sn := range n.Subnets {
				ipam.Config = append(ipam.Config, network.IPAMConfig{
					Subnet: sn.Subnet, Gateway: sn.Gateway, IPRange: sn.IPRange,
				})
			}
			opts.IPAM = ipam
		}
		if _, err := node.Client.NetworkCreate(ctx, n.Name, opts); err != nil {
			return fmt.Errorf("creating network %s for the pre-deploy hooks: %w", n.Name, err)
		}
		fmt.Fprintf(log, "created network %s (%s, attachable)\n", n.Name, opts.Driver)
	}

	for _, v := range prov.Volumes {
		// VolumeCreate is create-or-adopt by name; no inspect dance needed.
		if _, err := node.Client.VolumeCreate(ctx, volume.CreateOptions{
			Name: v.Name, Driver: v.Driver, DriverOpts: v.Options, Labels: v.Labels,
		}); err != nil {
			return fmt.Errorf("creating volume %s for the pre-deploy hooks: %w", v.Name, err)
		}
		fmt.Fprintf(log, "created volume %s\n", v.Name)
	}
	return nil
}

// runHookPhase runs ONE hook to its verdict and appends its log. Returns whether the
// pipeline may continue.
func (s *Server) runHookPhase(ctx context.Context, node *dockerx.Node, dep *store.Deployment,
	stack *store.Stack, phase, service string, bundle *stacks.Bundle, hooks *stacks.Hooks,
	log *strings.Builder) bool {

	log.WriteString(phaseHeader(phase+" hook", service))

	// Each hook gets its own deadline. A migration that has run for this long is not
	// late, it is wedged — and without the bound it would hold the stack's deploy claim
	// for the whole deploy timeout.
	hctx, cancel := context.WithTimeout(ctx, hooks.Timeout)
	defer cancel()

	ctrID, err := stacks.StartHook(hctx, node, dep.ID, stack.ID, stack.Name, service, bundle)
	if err != nil {
		fmt.Fprintf(log, "the hook runner could not be started: %v\n", err)
		return false
	}
	// The live log view follows RunnerCtrID, so pointing it at each phase in turn is what
	// lets somebody watch their migration scroll by before the deploy output takes over.
	if err := s.store.SetDeploymentContainer(ctx, dep.ID, ctrID); err != nil {
		slog.Error("recording the hook runner", "deployment", dep.ID, "err", err)
	}
	dep.RunnerCtrID = ctrID

	result, err := stacks.Wait(hctx, node, ctrID)
	if err != nil {
		// Most likely the timeout. The runner is still going, unobserved — kill it rather
		// than leave a migration running against a database nobody is watching.
		stacks.RemoveHookRunners(context.WithoutCancel(ctx), node, dep.ID)
		fmt.Fprintf(log, "hook %q did not finish within %s and was stopped: %v\n", service, hooks.Timeout, err)
		return false
	}
	log.WriteString(result.Log)
	log.WriteString("\n")
	if result.ExitCode != 0 {
		// Just the fact. What it MEANS — stop, or proceed — is the caller's line to
		// write, because only the caller knows the hook's on_failure policy.
		fmt.Fprintf(log, "hook %q failed (exit %d).\n", service, result.ExitCode)
		return false
	}
	return true
}

// autoRollback puts back the last version that passed its own pipeline. One step, never
// recursive: the rollback deploy runs with RollbackOnFailure disabled (see deploy()), so
// a rollback whose own post hooks fail stops and waits for a person — automation gets one
// attempt at self-repair, not a loop.
func (s *Server) autoRollback(ctx context.Context, stack *store.Stack, failed *store.Deployment) {
	prev, err := s.lastGoodDeployment(ctx, stack.ID, failed.ID)
	if err != nil || prev == nil {
		slog.Warn("rollback_on_failure: no successful deployment to roll back to",
			"stack", stack.Name, "failed", failed.ID)
		return
	}

	_ = s.store.Audit(ctx, store.AuditEntry{
		EnvID: stack.EnvID, Action: "stack.rollback", Target: stack.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{
			"automatic": true, "failed_deployment": failed.ID, "restoring": prev.ID,
		}),
	})
	slog.Info("post-deploy hook failed; rolling back automatically",
		"stack", stack.Name, "failed", failed.ID, "restoring", prev.ID)

	// StartedBy stays empty: nobody pressed this. The audit entry above carries the chain
	// of causation from the deployment that did have a person (or a webhook) behind it.
	if _, err := s.deploy(ctx, stack, stacks.ActionUp, store.TriggerRollback, "", prev); err != nil {
		slog.Error("automatic rollback could not start", "stack", stack.Name, "err", err)
	}
}

// lastGoodDeployment is the most recent deployment that shipped something and succeeded —
// what an automatic rollback restores.
func (s *Server) lastGoodDeployment(ctx context.Context, stackID, excludeID string) (*store.Deployment, error) {
	deps, err := s.store.ListDeployments(ctx, stackID, 50)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if d.ID == excludeID || d.Status != store.DeployOK || !isDeploying(stacks.Action(d.Action)) {
			continue
		}
		if d.ComposeYAML == "" {
			continue // predates stored bundles; nothing to restore from
		}
		return d, nil
	}
	return nil, nil
}

// phaseHeader marks a phase boundary in the one log a deployment keeps. Distinct enough
// to scan for, plain enough to read in a terminal.
func phaseHeader(phase, detail string) string {
	return fmt.Sprintf("── %s: %s ──\n", phase, detail)
}
