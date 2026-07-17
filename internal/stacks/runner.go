package stacks

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// ErrRunnerGone reports that the container a viewer tried to follow no longer exists —
// already collected by Wait. In a hooked pipeline this is the normal gap between phases.
var ErrRunnerGone = errors.New("stacks: the runner container is gone")

// RunnerImage is pinned. Two reasons, and the second is the important one:
//
//  1. No host needs a compose binary installed, or the right version of one.
//  2. The compose version is a property of DAFFA, not of whatever happened to be on
//     each machine — so a deploy behaves the same on every host in the fleet.
const RunnerImage = "docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c"

// Labels let the server find its runners again after a restart.
//
// LabelHookRun is DELIBERATELY not LabelRun. FindRunner reattaches to whatever carries
// LabelRun and records its exit as the deployment's — and a pre-deploy hook that
// succeeded is not a deployment that succeeded. A hook runner found after a restart is
// an orphan of a pipeline whose orchestration died with the process; the deployment is
// failed honestly and the orphan is swept (see RemoveHookRunners), never adopted.
const (
	LabelRun     = "daffa.run"
	LabelStack   = "daffa.stack"
	LabelHookRun = "daffa.hookrun"
)

// Start launches a runner container on the target daemon and returns its ID.
//
// The deploy runs in a CONTAINER rather than in the Daffa process, and that is not an
// implementation detail — it is the reason Daffa can manage the stack it is part of.
// If the server ran `compose up` itself, recreating its own container would kill the
// deploy halfway through, leaving the stack in an unknown state and no one to report it.
// A detached container is supervised by dockerd, so Daffa can be recreated by its own
// deploy and simply reattach to the runner's logs when it comes back.
func Start(ctx context.Context, node *dockerx.Node, eng Engine, runID, stackID, project string, action Action, bundle *Bundle) (string, error) {
	if !Supports(eng, action) {
		return "", fmt.Errorf("stacks: %s cannot %s a stack", eng.Label(), action)
	}
	if err := ensureImage(ctx, node); err != nil {
		return "", err
	}

	// The runner's environment is EMPTY except for what we put in it — a container does not
	// inherit the host's — so these two names, plus whatever the engine asks for, are the only
	// things a compose file's ${VAR} can possibly interpolate against. Dokploy works for that
	// property with `env -i`; Portainer works for it by whitelisting PORTAINER_* into its loader.
	// Here it is structural, and it is worth not giving away.
	env := []string{
		"DOCKER_HOST=unix:///var/run/docker.sock",
		// The registry credential, if any, is written into the bundle as config.json; point the
		// CLI at it.
		"DOCKER_CONFIG=/stack",
	}
	// `docker stack deploy` has no --env-file: it interpolates ${VAR} from the process environment
	// and nowhere else. So the engine says what it needs, and compose — which gets --env-file —
	// says nothing.
	env = append(env, eng.RunnerEnv(bundle.Env)...)

	cfg := &container.Config{
		Image:      RunnerImage,
		Cmd:        eng.Command(action, project),
		WorkingDir: "/stack",
		Env:        env,
		Labels: map[string]string{
			LabelRun:   runID,
			LabelStack: stackID,
		},
	}

	host := &container.HostConfig{
		// The runner drives the HOST's daemon. This is the same socket Daffa itself
		// holds — the runner is not more privileged than the thing that started it.
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: "/var/run/docker.sock",
			Target: "/var/run/docker.sock",
		}},
		AutoRemove: false, // we need the exit code and the logs after it stops
	}

	created, err := node.Client.ContainerCreate(ctx, cfg, host, nil, nil, "daffa-run-"+runID)
	if err != nil {
		return "", fmt.Errorf("stacks: creating runner: %w", err)
	}

	// Copy the bundle in BEFORE starting: the container's filesystem is writable at
	// create time, and this way there is no volume to create, mount, or clean up — and
	// it works identically on a remote host, because CopyToContainer is just another
	// Docker API call through the tunnel.
	if err := node.Client.CopyToContainer(ctx, created.ID, "/stack", bytes.NewReader(bundle.Tar),
		container.CopyToContainerOptions{}); err != nil {
		_ = node.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("stacks: copying the bundle into the runner: %w", err)
	}

	if err := node.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = node.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("stacks: starting the runner: %w", err)
	}
	return created.ID, nil
}

// StartHook launches a runner that executes ONE lifecycle hook: `compose run --rm` of a
// hook service from the bundle's hooks.yml. Same shape as a deploy runner — pinned image,
// bundle copied in, detached, exit code and log collected by Wait — because a hook IS a
// phase of a deployment and deserves the identical treatment: it survives a Daffa
// restart, it lands in the deployment log, and killing the deployment kills it.
func StartHook(ctx context.Context, node *dockerx.Node, runID, stackID, project, service string, bundle *Bundle) (string, error) {
	if err := ensureImage(ctx, node); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:      RunnerImage,
		Cmd:        HookCommand(project, service),
		WorkingDir: "/stack",
		Env: []string{
			"DOCKER_HOST=unix:///var/run/docker.sock",
			"DOCKER_CONFIG=/stack",
			// hooks.yml deliberately holds only the hook services, so compose sees the
			// stack's own containers as "orphans" of the project and says so in every
			// hook log — a warning that reads like something went wrong when the split
			// is working exactly as designed.
			"COMPOSE_IGNORE_ORPHANS=true",
		},
		Labels: map[string]string{
			LabelHookRun: runID,
			LabelStack:   stackID,
		},
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: "/var/run/docker.sock",
			Target: "/var/run/docker.sock",
		}},
		AutoRemove: false, // the exit code and the log are the point
	}

	created, err := node.Client.ContainerCreate(ctx, cfg, host, nil, nil, "daffa-hook-"+runID+"-"+service)
	if err != nil {
		return "", fmt.Errorf("stacks: creating the hook runner: %w", err)
	}
	if err := node.Client.CopyToContainer(ctx, created.ID, "/stack", bytes.NewReader(bundle.Tar),
		container.CopyToContainerOptions{}); err != nil {
		_ = node.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("stacks: copying the bundle into the hook runner: %w", err)
	}
	if err := node.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = node.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("stacks: starting the hook runner: %w", err)
	}
	return created.ID, nil
}

// RemoveHookRunners sweeps the hook runners of one deployment. Best-effort, for the
// restart path: a hook runner whose orchestrating process died is an orphan nothing will
// ever Wait on, and `compose run --rm` cleans the hook's own container but nothing
// cleans the runner that launched it.
func RemoveHookRunners(ctx context.Context, node *dockerx.Node, runID string) {
	f := filters.NewArgs()
	f.Add("label", LabelHookRun+"="+runID)
	list, err := node.Client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return
	}
	for _, c := range list {
		_ = node.Client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

// StartTeardown launches a runner that removes a stack, by PROJECT NAME and nothing else.
//
// It takes no bundle, and that is the whole point. Compose identifies a project by the labels
// it stamped on the containers, so `docker compose -p <project> down` finds and removes
// everything it made without ever reading a compose file — verified against a real daemon.
//
// Which means removing a stack does not depend on its SOURCE still being good. A git repo that
// has been deleted, a branch that was force-pushed, a compose file that no longer parses: none
// of them can leave you unable to clean up the containers those things once produced. Building
// the bundle first — the obvious implementation — would have made the ability to delete a stack
// contingent on the health of the thing you are trying to get rid of.
//
// volumes removes the stack's named volumes. That destroys data and cannot be undone, which is
// why it is a parameter here and an explicit, separate tick in the UI rather than part of what
// "delete" quietly means.
func StartTeardown(ctx context.Context, node *dockerx.Node, eng Engine, runID, stackID, project string, volumes bool) (string, error) {
	// Asked BEFORE the runner image is pulled. An engine that cannot honour `volumes` says so now,
	// as a plain refusal, rather than after a pull and a container start have made it look like the
	// request was accepted.
	cmd, err := eng.Teardown(project, volumes)
	if err != nil {
		return "", err
	}

	if err := ensureImage(ctx, node); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:  RunnerImage,
		Cmd:    cmd,
		Env:    []string{"DOCKER_HOST=unix:///var/run/docker.sock"},
		Labels: map[string]string{LabelRun: runID, LabelStack: stackID},
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: "/var/run/docker.sock",
			Target: "/var/run/docker.sock",
		}},
		AutoRemove: false, // the exit code and the log are the point
	}

	created, err := node.Client.ContainerCreate(ctx, cfg, host, nil, nil, "daffa-run-"+runID)
	if err != nil {
		return "", fmt.Errorf("stacks: creating the teardown runner: %w", err)
	}
	if err := node.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = node.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("stacks: starting the teardown runner: %w", err)
	}
	return created.ID, nil
}

// ensureImage pulls the runner image if the host does not have it. A host that has never
// deployed anything will not, and the first deploy should not fail with "no such image".
func ensureImage(ctx context.Context, node *dockerx.Node) error {
	_, _, err := node.Client.ImageInspectWithRaw(ctx, RunnerImage)
	if err == nil {
		return nil
	}

	rc, err := node.Client.ImagePull(ctx, RunnerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("stacks: pulling the runner image: %w", err)
	}
	defer rc.Close()

	// Drain: the pull is only complete when the body is consumed.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("stacks: pulling the runner image: %w", err)
	}
	return nil
}

// Result is how a run ended.
type Result struct {
	ExitCode int
	Log      string
	// Truncated says the log is only the end of what the runner printed.
	Truncated bool
}

// Wait blocks until the runner finishes, then collects its exit code and output and
// removes it. Safe to call from a fresh process against a runner started by a previous
// one — which is exactly what happens when Daffa redeploys itself.
func Wait(ctx context.Context, node *dockerx.Node, ctrID string) (*Result, error) {
	statusCh, errCh := node.Client.ContainerWait(ctx, ctrID, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case err := <-errCh:
		if err != nil {
			return nil, fmt.Errorf("stacks: waiting for the runner: %w", err)
		}
	case st := <-statusCh:
		exitCode = int(st.StatusCode)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	log, truncated, err := runnerLogs(ctx, node, ctrID)
	if err != nil {
		// The run happened; not being able to read its log is a lesser problem than
		// losing the result.
		log = fmt.Sprintf("(could not read the runner's output: %v)", err)
	}

	_ = node.Client.ContainerRemove(context.WithoutCancel(ctx), ctrID, container.RemoveOptions{Force: true})

	return &Result{ExitCode: exitCode, Log: log, Truncated: truncated}, nil
}

// LogLimit is how much of a runner's output is kept. It is a lot of compose output; a deploy
// that produces more than this is one nobody is going to read in full anyway.
const LogLimit = 1 << 20 // 1 MiB

// readCeiling bounds what we are willing to pull off the daemon before giving up on being
// exhaustive. Well above LogLimit, so the tail we keep is the real tail in every plausible
// case, but not unbounded: a runner in a print loop must not be able to make the server
// allocate until it dies.
const readCeiling = 16 << 20

// runnerLogs reads what the engine printed.
//
// Two things here are easy to get wrong, and both were:
//
// Compose writes its progress to STDERR, so a reader that only took stdout would show an empty
// log for a perfectly good deploy — and an empty log for a failed one, which is worse.
//
// And the cap keeps the END. This used to read through io.LimitReader, which keeps the
// BEGINNING — so a chatty deploy persisted a megabyte of "Pulling fs layer" and threw away the
// lines that said why it failed. The last lines are the only reason anyone opens a deploy log;
// they are the last thing that should be dropped.
func runnerLogs(ctx context.Context, node *dockerx.Node, ctrID string) (log string, truncated bool, err error) {
	rc, err := node.Client.ContainerLogs(ctx, ctrID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", false, err
	}
	defer rc.Close()

	var out bytes.Buffer
	// The runner has no TTY, so its stream is multiplexed and must be demultiplexed;
	// otherwise every line carries eight bytes of binary header.
	_, copyErr := stdcopy.StdCopy(&out, &out, io.LimitReader(rc, readCeiling))

	text, truncated := tailBytes(out.Bytes(), LogLimit)
	return strings.TrimSpace(text), truncated, copyErr
}

// tailBytes keeps the last limit bytes, cut at a line boundary so the log does not begin
// mid-word, and says so in the text rather than leaving a reader to wonder why it starts in the
// middle of a sentence.
func tailBytes(b []byte, limit int) (string, bool) {
	if len(b) <= limit {
		return string(b), false
	}

	cut := b[len(b)-limit:]
	if i := bytes.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "[… earlier output dropped: this log was too long to keep in full …]\n" + string(cut), true
}

// FindRunner locates a runner container by run id — used after a restart, when the
// server has a run marked `running` in its database but no goroutine watching it.
func FindRunner(ctx context.Context, node *dockerx.Node, runID string) (string, bool) {
	f := filters.NewArgs()
	f.Add("label", LabelRun+"="+runID)

	list, err := node.Client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil || len(list) == 0 {
		return "", false
	}
	return list[0].ID, true
}

// Kill stops a runner mid-flight.
//
// It does NOT undo anything. Compose may already have recreated half the services, and killing
// the process that was doing it leaves the stack exactly as far along as it got. That is a real
// state and the UI says so; what the caller gets back is the ability to deploy again, instead of
// a stack locked for the full deploy timeout by a runner that is never going to finish.
//
// The runner is left in place: Wait is still watching it, and it is Wait that records the exit
// code, collects the log and removes the container. Removing it here would race that.
func Kill(ctx context.Context, node *dockerx.Node, ctrID string) error {
	if err := node.Client.ContainerKill(ctx, ctrID, "KILL"); err != nil {
		return fmt.Errorf("stacks: killing the runner: %w", err)
	}
	return nil
}

// Reap force-removes a runner that Wait could NOT collect: a deploy that blew the timeout, or a
// wait that errored. On both of those paths Wait returns without removing the container — its
// removal only happens after ContainerWait reports an exit. So the runner, which mounts the
// Docker socket and may still be recreating services unsupervised, would linger indefinitely,
// contradicting the "failed" verdict the operator was just handed, and ReapOrphanedRuns will not
// reclaim it because the deployment is no longer unfinished. Best-effort and idempotent:
// force-remove kills it first, and "already gone" is the good outcome, not an error.
func Reap(ctx context.Context, node *dockerx.Node, ctrID string) {
	if ctrID == "" {
		return
	}
	_ = node.Client.ContainerRemove(context.WithoutCancel(ctx), ctrID, container.RemoveOptions{Force: true})
}

// StreamLogs follows a running runner's output so the UI can watch a deploy happen
// rather than waiting for a verdict.
//
// It emits whole LINES. It used to emit whatever 4 KB happened to arrive, which meant a chunk
// boundary could land in the middle of a line — so the live view could show a half-written
// error and then complete it a second later, and the log a viewer assembled from those chunks
// was not always the log that got stored. Docker makes no promise that a read ends on a line
// boundary; container logs already know this (see dockerx's lineWriter), and this is the same
// problem.
func StreamLogs(ctx context.Context, node *dockerx.Node, ctrID string, emit func(string) error) error {
	rc, err := node.Client.ContainerLogs(ctx, ctrID, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if errdefs.IsNotFound(err) {
		// The runner was already collected and removed — in a hooked pipeline that is a
		// PHASE BOUNDARY, not a failure: Wait force-removes each phase's container the
		// moment it ends, and a viewer can attach in the gap. Named so the caller can
		// move on to the next phase instead of painting a red banner over a good deploy.
		return ErrRunnerGone
	}
	if err != nil {
		return fmt.Errorf("stacks: streaming runner logs: %w", err)
	}
	defer rc.Close()

	// The watchdog closes rc if ctx is cancelled mid-stream, to unblock the scanner. Tie it to a
	// context this function cancels on return, or when the stream ends on its own (runner exits,
	// EOF) while the request ctx is still live, the goroutine would park on <-ctx.Done() forever —
	// harmless per call, but they accumulate for the lifetime of a busy deployments page.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()

	pr, pw := io.Pipe()
	go func() {
		// The runner has no TTY, so the stream is multiplexed; demultiplex both channels
		// into one pipe, because compose's progress is on stderr and its results on stdout
		// and a reader wants them interleaved the way they happened.
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = pw.CloseWithError(err)
	}()

	sc := bufio.NewScanner(pr)
	// A compose line can be long (a pull progress bar, a stack trace out of an entrypoint),
	// so give the scanner room before it gives up on one.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if err := emit(strings.TrimRight(sc.Text(), "\r") + "\n"); err != nil {
			return err
		}
	}
	return nil // the scanner stops when the runner exits; that is not an error
}

// deployTimeout bounds a run. A compose up that has not finished in this long is stuck
// (a pull from a dead registry, a healthcheck that never passes), and leaving the run
// "in progress" forever would block every future deploy of that stack.
const deployTimeout = 20 * time.Minute

func DeployContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), deployTimeout)
}
