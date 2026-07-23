package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// ExecSession is a live shell inside a container: a duplex byte stream plus a way to
// tell the far end the window changed size.
type ExecSession struct {
	ID     string
	Conn   io.ReadWriteCloser
	resize func(ctx context.Context, h, w uint) error
}

func (s *ExecSession) Resize(ctx context.Context, rows, cols uint) error {
	if rows == 0 || cols == 0 {
		return nil // a zero-sized terminal is a browser glitch, not an instruction
	}
	return s.resize(ctx, rows, cols)
}

func (s *ExecSession) Close() error { return s.Conn.Close() }

// Shells to try, in order of how pleasant they are to actually use. Distroless and
// scratch images have none of them, and there is nothing to be done about that — we
// say so plainly rather than dropping the user into a dead terminal.
var shellCandidates = []string{"/bin/bash", "/bin/sh"}

// Exec starts an interactive shell in a container. If cmd is empty it probes for a
// usable shell; probing is one exec each, which is cheap next to the round trip the
// human is about to spend typing.
func (e *Node) Exec(ctx context.Context, id string, cmd []string, rows, cols uint) (*ExecSession, error) {
	if len(cmd) == 0 {
		shell, err := e.detectShell(ctx, id)
		if err != nil {
			return nil, err
		}
		cmd = []string{shell}
	}

	created, err := e.Client.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		// A TTY exec merges stderr into stdout, which is what a terminal expects.
	})
	if err != nil {
		return nil, fmt.Errorf("dockerx: creating exec in %s: %w", id, err)
	}

	att, err := e.Client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("dockerx: attaching exec in %s: %w", id, err)
	}

	sess := &ExecSession{
		ID:   created.ID,
		Conn: att.Conn,
		resize: func(ctx context.Context, h, w uint) error {
			return e.Client.ContainerExecResize(ctx, created.ID, container.ResizeOptions{Height: h, Width: w})
		},
	}

	// Set the initial size before the shell draws its first prompt, or it wraps at 80
	// columns until the user's first keystroke.
	if err := sess.Resize(ctx, rows, cols); err != nil {
		// Not fatal — the shell works, it is just the wrong shape.
		_ = err
	}
	return sess, nil
}

var ErrNoShell = errors.New("no shell in container")

// detectShell finds a shell the container actually has. A candidate says "no" two ways,
// depending on daemon version: older daemons fail exec START when the binary is missing,
// newer ones (Docker 28+) start the exec fine and it exits 127. Every OTHER failure —
// container not running, gone, daemon unreachable — is about the container, not the
// shell, and must surface as itself: reporting it as "distroless image" sends the
// operator debugging the wrong thing entirely.
func (e *Node) detectShell(ctx context.Context, id string) (string, error) {
	var lastErr error
	for _, shell := range shellCandidates {
		created, err := e.Client.ContainerExecCreate(ctx, id, container.ExecOptions{
			Cmd:          []string{shell, "-c", "exit 0"},
			AttachStdout: true,
		})
		if err != nil {
			// Create never sees the binary; it fails over the container's state.
			// The next candidate cannot fare better — stop and say what happened.
			return "", fmt.Errorf("dockerx: probing for a shell in %s: %w", id, err)
		}
		if err := e.Client.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{}); err != nil {
			if missingBinary(err) {
				continue // this candidate's honest "no", pre-Docker-28 spelling
			}
			lastErr = err
			continue
		}

		inspect, err := e.Client.ContainerExecInspect(ctx, created.ID)
		if err == nil && inspect.ExitCode == 0 {
			return shell, nil
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("dockerx: probing for a shell in %s: %w", id, lastErr)
	}
	return "", fmt.Errorf("%w: tried %v — a distroless or scratch image has no shell to attach to", ErrNoShell, shellCandidates)
}

// missingBinary recognizes the daemon's ways of saying the exec'd path does not exist.
// String matching is what there is: the error crosses the HTTP API as text.
func missingBinary(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "no such file or directory")
}
