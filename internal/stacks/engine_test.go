package stacks

import (
	"strings"
	"testing"
)

// The engine decides what a stack can DO. This is the seam swarm will land on, and the property
// that matters is that the action set is the engine's, not a global constant — because compose
// and swarm genuinely disagree about it (swarm has no pull and no stop).
func TestComposeEngineActions(t *testing.T) {
	eng := ComposeEngine

	for _, a := range []Action{ActionUp, ActionPull, ActionStop, ActionDown, ActionRestart} {
		if !Supports(eng, a) {
			t.Errorf("compose does not support %q, but it always has", a)
		}
	}
	if Supports(eng, Action("deploy-to-mars")) {
		t.Error("compose claims to support an action that does not exist")
	}

	// Deploy is what people press, so it is the one that must be first.
	if eng.Actions()[0] != ActionUp {
		t.Errorf("the first action offered is %q; the UI renders its buttons in this order and "+
			"Deploy belongs at the front", eng.Actions()[0])
	}
}

// `down` must not remove volumes unless it is asked to. Removing a stack's containers is
// recoverable; removing its volumes is someone's database.
func TestTeardownDoesNotTouchVolumesUnlessAsked(t *testing.T) {
	cmd, err := ComposeEngine.Teardown("web", false)
	if err != nil {
		t.Fatalf("compose refused a plain teardown: %v", err)
	}
	plain := strings.Join(cmd, " ")
	if strings.Contains(plain, "--volumes") {
		t.Fatalf("a plain teardown passes --volumes: %q.\n\n"+
			"That is somebody's database, and nothing about the word \"down\" says so.", plain)
	}

	cmd, err = ComposeEngine.Teardown("web", true)
	if err != nil {
		t.Fatalf("compose refused an explicit volume teardown: %v", err)
	}
	if withVolumes := strings.Join(cmd, " "); !strings.Contains(withVolumes, "--volumes") {
		t.Errorf("an explicit volume teardown did not pass --volumes: %q", withVolumes)
	}
}

// The teardown identifies the project by NAME and reads no compose file, which is what makes
// removing a stack independent of its source still being good.
func TestTeardownNeedsNoComposeFile(t *testing.T) {
	argv, err := ComposeEngine.Teardown("web", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(argv, " ")
	if strings.Contains(cmd, "-f ") || strings.Contains(cmd, "/stack/") {
		t.Errorf("the teardown reads a compose file: %q.\n\n"+
			"A deleted repo or an unparseable file would then leave you unable to clean up the "+
			"containers it once produced.", cmd)
	}
	if !strings.Contains(cmd, "-p web") {
		t.Errorf("the teardown does not name the project: %q", cmd)
	}
}

// An engine Daffa cannot run is refused at the door, not stored and discovered at deploy time.
func TestEngineForResolvesOnlyRealEngines(t *testing.T) {
	if _, err := EngineFor("kubernetes"); err == nil {
		t.Error("an unknown engine was accepted")
	}

	// Empty means compose: every stack that predates the column is one.
	eng, err := EngineFor("")
	if err != nil || eng.Name() != "compose" {
		t.Errorf("an unset engine did not default to compose: %v / %v", eng, err)
	}

	// Swarm is real now. It was deliberately refused until it existed, because a stack that says
	// "swarm" and quietly gets `docker compose up` is the exact confusion the engine field exists
	// to end — and that refusal is only allowed to lift when the thing actually runs.
	eng, err = EngineFor("swarm")
	if err != nil || eng.Name() != "swarm" {
		t.Fatalf("EngineFor(\"swarm\") did not return the swarm engine: %v / %v", eng, err)
	}
}

// Swarm's action list is SHORTER than compose's, and every absence is a decision.
//
// The UI renders its buttons from this list, so a wrong entry here does not produce a broken
// button — it produces a button that lies about what the engine can do.
func TestSwarmActionsAreHonest(t *testing.T) {
	eng := SwarmEngine

	if !Supports(eng, ActionUp) || !Supports(eng, ActionDown) {
		t.Fatal("swarm cannot deploy or remove a stack, which is all it is for")
	}

	// No STOP. Swarm has no stop: scaling every service to zero is a different statement with
	// different consequences, and shipping it under the word "Stop" is the lie the engine exists
	// to end. Dokploy ships a "stop" for swarm stacks that runs `docker stack rm` and DESTROYS the
	// stack; that is the failure this assertion prevents.
	if Supports(eng, ActionStop) {
		t.Error("swarm claims it can Stop a stack. It cannot — and a Stop button that removes the " +
			"stack instead is how somebody deletes production while trying to pause it.")
	}

	// No PULL: `docker stack deploy` defaults to --resolve-image=always, so `up` already re-resolves
	// every tag against the registry. A pull button would be the deploy button twice.
	if Supports(eng, ActionPull) {
		t.Error("swarm offers Pull, but its up already re-resolves images against the registry")
	}
	if Supports(eng, ActionRestart) {
		t.Error("swarm offers Restart, which it has no command for")
	}

	// The volumes checkbox is drawn from this list. Swarm cannot remove a stack's volumes, so the
	// box must not be offered.
	if Supports(eng, ActionDownVolumes) {
		t.Error("swarm claims it can remove a stack's volumes. They are node-local; it cannot.")
	}
	if !Supports(ComposeEngine, ActionDownVolumes) {
		t.Error("compose can remove a stack's volumes, and the UI needs to know that to draw the box")
	}
}

// Swarm REFUSES a volume teardown rather than silently not doing it.
//
// Accepting the flag and dropping it would mean somebody ticks "also delete its volumes", Daffa
// says ok, and the data is still there. That is the worst of both answers.
func TestSwarmRefusesToPretendItRemovedVolumes(t *testing.T) {
	if _, err := SwarmEngine.Teardown("web", true); err == nil {
		t.Fatal("swarm accepted a volume teardown it cannot perform.\n\n" +
			"Not deleting what somebody ticked a box to delete is worse than saying no: they will " +
			"believe the data is gone when it is not.")
	}

	cmd, err := SwarmEngine.Teardown("web", false)
	if err != nil {
		t.Fatalf("swarm refused an ordinary teardown: %v", err)
	}
	if got := strings.Join(cmd, " "); got != "docker stack rm web" {
		t.Errorf("swarm teardown = %q; want `docker stack rm web`", got)
	}
}

// The flag that makes a deployment record mean anything.
//
// Without --detach=false the CLI returns the instant the manager ACCEPTS the spec, so every deploy
// reports success — including the one whose tasks never schedule. Dokploy omits it and does exactly
// that. This assertion is the whole difference between "the command worked" and "the thing runs".
func TestSwarmDeployWaitsForConvergence(t *testing.T) {
	cmd := strings.Join(SwarmEngine.Command(ActionUp, "web"), " ")

	if !strings.Contains(cmd, "--detach=false") {
		t.Fatalf("swarm deploy does not pass --detach=false: %q.\n\n"+
			"It would then return as soon as the manager accepted the spec, and a stack whose tasks "+
			"never schedule would be recorded as a successful deploy.", cmd)
	}
	if !strings.Contains(cmd, "--with-registry-auth") {
		t.Errorf("swarm deploy does not ship registry credentials to the swarm agents: %q", cmd)
	}
	if !strings.Contains(cmd, "--prune") {
		t.Errorf("swarm deploy does not prune removed services: %q", cmd)
	}
}

// docker stack deploy has NO --env-file. It interpolates ${VAR} from the process environment and
// nowhere else, so the engine hands the variables to the runner container instead.
//
// Compose gets --env-file and must NOT also receive them: the same values in two places is how they
// come to disagree.
func TestSwarmInterpolatesFromTheRunnersEnvironment(t *testing.T) {
	vars := []EnvVar{{Key: "TAG", Value: "v1.2.3"}, {Key: "DB_PASSWORD", Value: "hunter2"}}

	got := SwarmEngine.RunnerEnv(vars)
	want := map[string]bool{"TAG=v1.2.3": true, "DB_PASSWORD=hunter2": true}
	if len(got) != len(want) {
		t.Fatalf("RunnerEnv returned %v; want the stack's variables as KEY=VALUE", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("RunnerEnv produced %q, which is not one of the stack's variables", kv)
		}
	}

	if env := ComposeEngine.RunnerEnv(vars); len(env) != 0 {
		t.Errorf("compose put %v in the runner's environment, but it is given --env-file", env)
	}

	// And the swarm deploy must NOT reach for an --env-file that the CLI would reject.
	if cmd := strings.Join(SwarmEngine.Command(ActionUp, "web"), " "); strings.Contains(cmd, "--env-file") {
		t.Errorf("swarm deploy passes --env-file, which `docker stack deploy` does not have: %q", cmd)
	}
}

// Truncation keeps the END. It used to keep the beginning, so a chatty deploy persisted a
// megabyte of "Pulling fs layer" and dropped the lines that said why it failed — which are the
// only lines anybody opens a deploy log to read.
func TestLogTruncationKeepsTheEnd(t *testing.T) {
	var b strings.Builder
	for i := range 5000 {
		b.WriteString("Pulling fs layer ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteByte('\n')
		_ = i
	}
	b.WriteString("ERROR: port is already allocated\n")

	out, truncated := tailBytes([]byte(b.String()), 4096)
	if !truncated {
		t.Fatal("a log well over the limit was not reported as truncated")
	}
	if !strings.Contains(out, "ERROR: port is already allocated") {
		t.Fatal("truncation dropped the LAST line — the one that says why the deploy failed.\n\n" +
			"That is the whole reason anyone opens the log.")
	}
	if len(out) > 4096+200 { // the elision notice is allowed on top of the limit
		t.Errorf("the kept log is %d bytes, well over the 4096 asked for", len(out))
	}
	if !strings.Contains(out, "earlier output dropped") {
		t.Error("a truncated log does not say it was truncated, so it just appears to begin " +
			"mid-sentence")
	}

	// Under the limit: untouched, and not falsely flagged.
	short, truncated := tailBytes([]byte("all good\n"), 4096)
	if truncated || short != "all good\n" {
		t.Errorf("a short log was altered: %q / truncated=%v", short, truncated)
	}
}
