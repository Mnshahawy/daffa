package backups

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Destination is where a snapshot goes.
type Destination struct {
	Endpoint   string
	Region     string
	Bucket     string
	Prefix     string
	KeyID      string
	Secret     string
	Encrypt    bool
	Recipients string // age PUBLIC keys, space separated
}

// Result is what a completed backup produced.
type Result struct {
	ObjectKey string
	Bytes     int64
}

// Run performs one backup, end to end.
//
// The pipeline is assembled with io.Pipe rather than buffers: the dump is produced by the
// database at whatever rate it can manage, compressed, encrypted, and uploaded
// concurrently. If any stage fails, the error propagates through the pipe and tears down
// the rest — which matters, because a backup that half-uploads and reports success is
// worse than one that fails loudly.
func Run(ctx context.Context, node *dockerx.Node, containerRef string, spec Spec, dst Destination, now time.Time) (*Result, error) {
	cmd, err := spec.dumpCommand()
	if err != nil {
		return nil, err
	}

	client, err := newS3(ctx, dst)
	if err != nil {
		return nil, err
	}

	key := objectKey(dst.Prefix, string(spec.Engine), Extension(spec.Engine, dst.Encrypt), now)

	// Stage 1: the dump, straight out of the database container.
	dump, wait, err := execDump(ctx, node, containerRef, cmd)
	if err != nil {
		return nil, err
	}
	defer dump.Close()

	// Stage 2 + 3: gzip, then (optionally) age. Both wrap the writer side of a pipe that
	// the uploader reads from.
	pr, pw := io.Pipe()

	go func() {
		err := compressAndEncrypt(pw, dump, dst)
		// Closing the pipe with the error is what makes the uploader fail rather than
		// cheerfully finish a truncated object.
		_ = pw.CloseWithError(err)
	}()

	n, err := upload(ctx, client, dst.Bucket, key, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}

	// Stage 0, checked last: did the DUMP itself succeed? A pg_dumpall that dies partway
	// still closes its stdout, and the pipeline above would happily upload the truncated
	// prefix and call it a backup. The exit code is the only thing that says otherwise.
	if err := wait(); err != nil {
		// Remove the bad object rather than leave a corrupt snapshot in the bucket
		// looking exactly like a good one.
		_ = client.RemoveObject(context.WithoutCancel(ctx), dst.Bucket, key, removeOpts())
		return nil, err
	}

	if n == 0 {
		_ = client.RemoveObject(context.WithoutCancel(ctx), dst.Bucket, key, removeOpts())
		return nil, fmt.Errorf("backups: the dump produced no data")
	}

	return &Result{ObjectKey: key, Bytes: n}, nil
}

// compressAndEncrypt is the middle of the pipe. Order matters: compress first, then
// encrypt. The other way round produces ciphertext, which does not compress.
func compressAndEncrypt(dst io.Writer, src io.Reader, d Destination) error {
	var sink io.Writer = dst
	var closers []io.Closer

	if d.Encrypt {
		recipients, err := parseRecipients(d.Recipients)
		if err != nil {
			return err
		}
		encWriter, err := age.Encrypt(dst, recipients...)
		if err != nil {
			return fmt.Errorf("backups: starting encryption: %w", err)
		}
		sink = encWriter
		closers = append(closers, encWriter)
	}

	gz, err := gzip.NewWriterLevel(sink, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("backups: starting compression: %w", err)
	}

	if _, err := io.Copy(gz, src); err != nil {
		return fmt.Errorf("backups: streaming the dump: %w", err)
	}

	// Close in order: gzip's trailer must be written before age's, or the object is
	// unreadable in a way that only shows up when you desperately need it.
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backups: finishing compression: %w", err)
	}
	for _, c := range closers {
		if err := c.Close(); err != nil {
			return fmt.Errorf("backups: finishing encryption: %w", err)
		}
	}
	return nil
}

// parseRecipients reads age PUBLIC keys. Only public keys: the whole point of the
// asymmetric scheme is that the machine taking backups cannot read them back. If it held
// a private key, an attacker who owned the box would own the backups too — including the
// ones from before they arrived.
func parseRecipients(s string) ([]age.Recipient, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, fmt.Errorf("backups: encryption is on but no age recipients are configured")
	}

	out := make([]age.Recipient, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "AGE-SECRET-KEY-") {
			return nil, fmt.Errorf("backups: that is an age PRIVATE key — configure the public key (age1…) instead. Daffa must not be able to read its own backups")
		}
		r, err := age.ParseX25519Recipient(f)
		if err != nil {
			return nil, fmt.Errorf("backups: %q is not a valid age public key: %w", f, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// execDump starts the dump inside the database container and returns its stdout, plus a
// function that reports how it exited.
//
// Docker multiplexes stdout and stderr over one connection when there is no TTY, so the
// two must be demultiplexed — otherwise the dump would be interleaved with progress
// messages and eight-byte frame headers, and the resulting "backup" would be garbage
// that only reveals itself on restore.
func execDump(ctx context.Context, node *dockerx.Node, containerRef string, cmd []string) (io.ReadCloser, func() error, error) {
	created, err := node.Client.ContainerExecCreate(ctx, containerRef, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("backups: preparing the dump in %s (does the container exist?): %w", containerRef, err)
	}

	att, err := node.Client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("backups: starting the dump: %w", err)
	}

	pr, pw := io.Pipe()
	var stderr bytes.Buffer

	go func() {
		// StdCopy splits the multiplexed stream: real data to the pipe, diagnostics to
		// a buffer we can quote if it fails.
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		att.Close()
		_ = pw.CloseWithError(err)
	}()

	wait := func() error {
		inspect, err := node.Client.ContainerExecInspect(context.WithoutCancel(ctx), created.ID)
		if err != nil {
			return fmt.Errorf("backups: could not confirm the dump succeeded: %w", err)
		}
		if inspect.ExitCode != 0 {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 500 {
				msg = msg[:500] + "…"
			}
			if msg == "" {
				msg = "no output"
			}
			return fmt.Errorf("backups: the dump failed (exit %d): %s", inspect.ExitCode, msg)
		}
		return nil
	}

	return pr, wait, nil
}

// objectKey lays snapshots out so a bucket listing is readable and a lifecycle rule can
// act on it: <prefix>/<YYYY-MM-DD>/<base>-<timestamp><ext>, where base is the engine
// name, or volume-<name> for the volume engine.
func objectKey(prefix, base, ext string, now time.Time) string {
	t := now.UTC()
	day := t.Format("2006-01-02")
	stamp := t.Format("20060102T150405Z")

	key := fmt.Sprintf("%s/%s-%s%s", day, base, stamp, ext)
	if p := strings.Trim(prefix, "/"); p != "" {
		key = p + "/" + key
	}
	return key
}
