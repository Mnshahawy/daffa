package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecResult is a finished, non-interactive exec: its exit code and the tail of what it
// printed. The TAIL, because the useful line of a heap-dump or thread-dump command is the last
// one ("Heap dump file created", or the error), and a command that prints megabytes must not
// be able to put megabytes into an alert row.
type ExecResult struct {
	ExitCode int
	Output   string
}

// maxExecOutput bounds what ExecCapture keeps of a command's output.
const maxExecOutput = 4 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
		t.cut = true
	}
	return n, nil
}

// ExecCapture runs cmd in a container without a TTY and waits for it, bounded by ctx. It is
// the automation twin of Exec: no shell is probed and no terminal is attached, because nobody
// is on the other end — the command is run exactly as written (wrap it in sh -c for pipes).
func (e *Node) ExecCapture(ctx context.Context, id string, cmd []string) (*ExecResult, error) {
	created, err := e.Client.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("dockerx: creating exec in %s: %w", id, err)
	}
	att, err := e.Client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerx: attaching exec in %s: %w", id, err)
	}
	defer att.Close()

	out := &tailBuffer{max: maxExecOutput}
	done := make(chan error, 1)
	go func() {
		// Without a TTY the stream is multiplexed; both halves go to one tail, in order.
		_, err := stdcopy.StdCopy(out, out, att.Reader)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("dockerx: reading exec output in %s: %w", id, err)
		}
	case <-ctx.Done():
		// The process keeps running in the container — Docker has no exec kill — but we stop
		// waiting, and say so, rather than hold an alert's actions hostage to it.
		return nil, fmt.Errorf("dockerx: exec in %s did not finish in time: %w", id, ctx.Err())
	}

	insp, err := e.Client.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return nil, fmt.Errorf("dockerx: inspecting exec in %s: %w", id, err)
	}
	text := out.buf.String()
	if out.cut {
		text = "…" + text
	}
	return &ExecResult{ExitCode: insp.ExitCode, Output: text}, nil
}
