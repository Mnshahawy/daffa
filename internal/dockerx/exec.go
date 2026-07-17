package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"

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

// detectShell finds a shell the container actually has. `docker exec` fails at start
// if the binary is missing, so we ask the container to check rather than guessing and
// handing the user an opaque OCI error.
func (e *Node) detectShell(ctx context.Context, id string) (string, error) {
	for _, shell := range shellCandidates {
		created, err := e.Client.ContainerExecCreate(ctx, id, container.ExecOptions{
			Cmd:          []string{shell, "-c", "exit 0"},
			AttachStdout: true,
		})
		if err != nil {
			continue
		}
		if err := e.Client.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{}); err != nil {
			continue
		}

		inspect, err := e.Client.ContainerExecInspect(ctx, created.ID)
		if err == nil && inspect.ExitCode == 0 {
			return shell, nil
		}
	}
	return "", fmt.Errorf("%w: tried %v — a distroless or scratch image has no shell to attach to", ErrNoShell, shellCandidates)
}
