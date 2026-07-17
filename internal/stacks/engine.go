package stacks

import (
	"context"
	"fmt"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Action is what to do to a stack.
type Action string

const (
	ActionUp      Action = "up"      // create/update and start
	ActionPull    Action = "pull"    // pull new images, then up
	ActionStop    Action = "stop"    // stop, keep everything
	ActionDown    Action = "down"    // remove containers and networks (NOT volumes)
	ActionRestart Action = "restart" // restart in place

	// ActionDownVolumes is not a button anyone presses; it is a CAPABILITY, and it is in this list
	// so that the UI can ask the engine whether the "also delete its volumes" checkbox should even
	// be drawn. Compose can do it. Swarm cannot — a stack's volumes live on each node and the
	// manager has no authority over them — and offering a checkbox that silently does nothing is
	// worse than not offering it.
	ActionDownVolumes Action = "down+volumes"
)

// Engine is the thing that actually applies a stack.
//
// It exists because "stack" was a lie by omission. Daffa called the entity a stack and only ever
// ran `docker compose` — never `docker stack` — and nothing in the UI, the API or the database
// said so. You had to read this package to find out what would happen when you pressed Deploy.
//
// So the engine is stored on the stack, shown wherever the stack is, and — the part that matters —
// it OWNS the things that differ between engines. They differ more than one might expect, and the
// difference that bites hardest is not the commands: it is HOW YOU FIND OUT WHAT IS RUNNING.
// Compose asks a daemon for containers. Swarm asks a manager for services, because on a multi-node
// cluster the manager cannot see another machine's containers at all — so reading a swarm stack
// through compose's eyes reports a perfectly healthy stack as broken.
//
// The API reports Actions() with the stack and the UI renders its buttons from that list, so an
// engine that cannot stop a stack does not ship a Stop button that returns an error.
type Engine interface {
	// Name is the stored value: "compose" | "swarm".
	Name() string
	// Label is what a person should read. The whole point of the exercise.
	Label() string

	// ProjectLabel identifies this stack's containers, and ServiceLabel the service within it.
	// These are still how the CONTAINERS view groups things — but they are no longer how a stack's
	// status is found. See Describe, and see why that had to move.
	ProjectLabel() string
	ServiceLabel() string

	// Actions are the ones this engine supports, in the order they should be offered.
	Actions() []Action
	// Command builds the runner's argv for an action.
	Command(a Action, project string) []string

	// Teardown removes the stack by project NAME alone — no compose file, no source, so a deleted
	// repo cannot leave you unable to clean up the containers it once produced.
	//
	// It returns an ERROR rather than silently ignoring `volumes`, because swarm cannot remove
	// them: they live on each node, and `docker stack rm` has no authority over them. Not deleting
	// what somebody ticked a box to delete is worse than saying no.
	Teardown(project string, volumes bool) ([]string, error)

	// RunnerEnv is the environment the RUNNER CONTAINER needs, beyond what it always gets.
	//
	// This exists for one reason: `docker stack deploy` has no --env-file. It interpolates ${VAR}
	// from the process environment and nowhere else. Compose returns nil here, because it is given
	// --env-file and does not need it.
	//
	// The alternative — resolving ${VAR} into the YAML before shipping it — was rejected: the
	// resolved YAML is what gets stored on the deployment for rollback, so secrets would land in
	// the database, which docs/stacks.md §2 forbids outright.
	RunnerEnv(vars []EnvVar) []string

	// Describe reports what is ACTUALLY running, by whatever means this engine can know it.
	//
	// THIS IS THE METHOD THAT FORCED THE INTERFACE TO GROW. Status used to be found by listing
	// containers filtered on ProjectLabel — which quietly assumes a stack is a set of containers on
	// one daemon. On a swarm it is not: the containers are spread across machines the control node
	// cannot see, so that lookup returns almost nothing and reports a healthy stack as
	// `not_deployed`. A confident, wrong answer — precisely the failure this whole abstraction
	// exists to prevent.
	Describe(ctx context.Context, node *dockerx.Node, project string,
		declared []Service, deployedHash, currentHash string) (*Status, error)
}

// EngineFor resolves a stored engine name.
func EngineFor(name string) (Engine, error) {
	switch name {
	case "", ComposeEngine.Name():
		// Empty means compose: it is the only engine that ran before the column existed, so every
		// stack that predates it is one.
		return ComposeEngine, nil
	case SwarmEngine.Name():
		return SwarmEngine, nil
	default:
		return nil, fmt.Errorf("stacks: unknown engine %q", name)
	}
}

// Supports reports whether an engine can do an action at all.
func Supports(e Engine, a Action) bool {
	for _, x := range e.Actions() {
		if x == a {
			return true
		}
	}
	return false
}

// ── compose ─────────────────────────────────────────────────────────────────────

// ComposeEngine runs `docker compose` against one daemon, from inside the runner.
var ComposeEngine Engine = composeEngine{}

type composeEngine struct{}

func (composeEngine) Name() string         { return "compose" }
func (composeEngine) Label() string        { return "Docker Compose" }
func (composeEngine) ProjectLabel() string { return "com.docker.compose.project" }
func (composeEngine) ServiceLabel() string { return "com.docker.compose.service" }

func (composeEngine) Actions() []Action {
	return []Action{ActionUp, ActionPull, ActionRestart, ActionStop, ActionDown, ActionDownVolumes}
}

// Compose is handed --env-file, so it needs nothing in its process environment.
func (composeEngine) RunnerEnv([]EnvVar) []string { return nil }

func (composeEngine) Command(a Action, project string) []string {
	base := []string{"docker", "compose", "-p", project, "-f", "/stack/" + composePath, "--env-file", "/stack/" + envPath}

	switch a {
	case ActionUp:
		return append(base, "up", "-d", "--remove-orphans")
	case ActionPull:
		return append(base, "up", "-d", "--pull", "always", "--remove-orphans")
	case ActionStop:
		return append(base, "stop")
	case ActionDown:
		return append(base, "down", "--remove-orphans")
	case ActionRestart:
		return append(base, "restart")
	default:
		return append(base, "ps")
	}
}

// Teardown does NOT pass --volumes unless asked. Removing a stack's containers is recoverable;
// removing its volumes is someone's database. If a person genuinely wants that, they tick a
// separate box, having been told what it destroys.
func (composeEngine) Teardown(project string, volumes bool) ([]string, error) {
	cmd := []string{"docker", "compose", "-p", project, "down", "--remove-orphans"}
	if volumes {
		cmd = append(cmd, "--volumes")
	}
	return cmd, nil
}

// Describe asks the daemon for containers, which is exactly right for compose: a compose stack IS
// a set of containers on one machine.
func (composeEngine) Describe(ctx context.Context, node *dockerx.Node, project string,
	declared []Service, deployedHash, currentHash string) (*Status, error) {
	return describeContainers(ctx, node, ComposeEngine, project, declared, deployedHash, currentHash)
}

// ── swarm ───────────────────────────────────────────────────────────────────────

// SwarmEngine runs `docker stack deploy` against a MANAGER, from inside the runner.
var SwarmEngine Engine = swarmEngine{}

type swarmEngine struct{}

func (swarmEngine) Name() string         { return "swarm" }
func (swarmEngine) Label() string        { return "Docker Swarm" }
func (swarmEngine) ProjectLabel() string { return "com.docker.stack.namespace" }
func (swarmEngine) ServiceLabel() string { return "com.docker.swarm.service.name" }

// Actions: up and down. That is the honest list, and each absence is deliberate.
//
// No PULL. `docker stack deploy` defaults to --resolve-image=always, which re-queries the registry
// and re-resolves every tag to a digest on EVERY deploy. So on swarm, `up` already IS `pull`, and a
// second button would be the first button twice. (Compose needs `pull` because its `up` reuses
// whatever image is already on the host.)
//
// No RESTART, and no STOP. Swarm has no stop. Scaling every service to zero is a DIFFERENT
// STATEMENT with different consequences, and shipping it under the word "Stop" is exactly the class
// of lie this engine exists to end. If we want it later it arrives called scale-to-zero — which is
// what Dokploy in fact calls it, while ALSO offering a "stop" for stack-type composes that runs
// `docker stack rm` and destroys the stack.
//
// No DOWN+VOLUMES: see Teardown.
func (swarmEngine) Actions() []Action {
	return []Action{ActionUp, ActionDown}
}

// RunnerEnv is the whole reason this method exists on the interface.
//
// `docker stack deploy` has no --env-file. It interpolates ${VAR} from the process environment and
// nowhere else, so the stack's variables become the runner container's own environment and the
// Docker CLI does the substitution natively — no reimplementation of ${VAR:-default} or ${VAR:?err}.
//
// Two honest notes. The values DO sit in the runner's environment, visible to `docker inspect` on
// that node for the life of the deploy — which is exactly as exposed as the .env file already in
// the bundle, on the same daemon, for the same duration. And the runner's environment is otherwise
// EMPTY, because a container does not inherit the host's: the only names visible to interpolation
// are the two the runner sets and the stack's own. Dokploy works for that property with `env -i`;
// Portainer works for it by whitelisting PORTAINER_* into its loader. We get it structurally, and
// it is worth keeping.
func (swarmEngine) RunnerEnv(vars []EnvVar) []string {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Key+"="+v.Value)
	}
	return out
}

func (swarmEngine) Command(a Action, project string) []string {
	switch a {
	case ActionUp:
		return []string{
			"docker", "stack", "deploy",
			"-c", "/stack/" + composePath,
			// Ship the registry credentials to the swarm agents that will do the pulling. The
			// bundle already writes config.json, and the runner already points DOCKER_CONFIG at it.
			"--with-registry-auth",
			// Swarm's --remove-orphans: services no longer in the file are removed.
			"--prune",
			// THE FLAG THAT MAKES A DEPLOYMENT RECORD MEAN ANYTHING.
			//
			// Without it the CLI returns the instant the manager ACCEPTS the spec, so every deploy
			// is `ok` — including the one whose tasks never schedule. With it the runner waits for
			// convergence and exits non-zero when it does not come, which is what watchDeployment
			// already knows how to turn into a failed deployment with a log.
			//
			// Dokploy omits this and reports success for stacks that never ran.
			"--detach=false",
			project,
		}
	case ActionDown:
		// Kept for the interface's honesty even though the deploy path routes down
		// through Teardown (no bundle needed): Actions() advertises down, so Command
		// must answer it with the removal and not with something politer.
		return []string{"docker", "stack", "rm", project}
	default:
		// The old default here was `docker stack services` — a LISTING, which exits 0.
		// When down first shipped it fell through to this arm, so pressing Down listed
		// the services, recorded "ok", and removed nothing. An unsupported action must
		// fail in a way a deployment log states plainly.
		return []string{"sh", "-c", "echo 'the swarm engine does not support this action' >&2; exit 1"}
	}
}

// Teardown REFUSES to remove volumes rather than quietly not removing them.
//
// `docker stack rm` cannot: a swarm stack's volumes are node-local, created on whichever machine
// each task landed on, and the manager has no authority over them. Accepting the flag and dropping
// it would mean somebody ticks "also delete its volumes", Daffa says "ok", and the data is still
// there — which is the worst of both answers.
//
// The UI never shows the box, because it renders from Actions() and swarm does not list
// ActionDownVolumes. This is the backstop for anything that asks anyway.
func (swarmEngine) Teardown(project string, volumes bool) ([]string, error) {
	if volumes {
		return nil, fmt.Errorf(
			"stacks: Swarm cannot remove a stack's volumes — they live on each node, and the manager " +
				"has no authority over them. Remove them per node, under Volumes")
	}
	return []string{"docker", "stack", "rm", project}, nil
}

// Describe asks the CONTROL NODE for services and tasks, never for containers.
//
// This is the method the interface grew for. A swarm stack's containers are spread across machines
// the manager cannot see, so listing containers by label finds only the ones that happen to be on
// the manager itself — and a healthy three-node stack reads as `not_deployed`.
func (swarmEngine) Describe(ctx context.Context, node *dockerx.Node, project string,
	declared []Service, deployedHash, currentHash string) (*Status, error) {
	services, err := node.ListServices(ctx)
	if err != nil {
		return nil, err
	}

	// Swarm names a stack's services `<project>_<service>`, and stamps the namespace as a label.
	// Trust the label: a project name with an underscore in it would defeat the name split.
	running := map[string]dockerx.Service{}
	for _, s := range services {
		if s.Stack != project {
			continue
		}
		name := s.Name
		if len(name) > len(project)+1 && name[:len(project)+1] == project+"_" {
			name = name[len(project)+1:]
		}
		running[name] = s
	}

	st := &Status{
		Services: make([]ServiceStatus, 0, len(declared)),
		Changed:  deployedHash != "" && currentHash != "" && deployedHash != currentHash,
	}

	// Same rule as the compose path: a hook is declared, never deployed, and must not
	// read as a missing service or drag the stack to "partial".
	up, total := 0, 0
	for _, d := range declared {
		if d.Hook {
			st.Services = append(st.Services, ServiceStatus{Name: d.Name, Declared: d.Image, State: "hook"})
			continue
		}
		total++
		s := ServiceStatus{Name: d.Name, Declared: d.Image, State: "missing"}

		if svc, ok := running[d.Name]; ok {
			// The image swarm reports is DIGEST-PINNED — `nginx:1.25@sha256:…` — because
			// `stack deploy` resolves it against the registry. Report the tag, or every healthy
			// service reads as drifted, forever. See dockerx.UntagDigest.
			s.Running = svc.Tag
			s.ContainerID = svc.ID

			switch {
			case svc.Desired == 0:
				s.State = "stopped" // scaled to zero: deliberate, not broken
			case svc.Running == 0:
				s.State = "missing" // asked for, and not running. The TASK says why.
			case svc.Running < svc.Desired:
				s.State = "partial"
				up++
			default:
				s.State = "running"
				up++
			}
		}
		st.Services = append(st.Services, s)
	}

	switch {
	case len(running) == 0:
		st.State = "not_deployed"
	case up == 0:
		st.State = "stopped"
	case up == total:
		st.State = "running"
	default:
		st.State = "partial"
	}
	return st, nil
}
