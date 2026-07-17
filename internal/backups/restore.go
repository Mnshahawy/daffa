package backups

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Restore is deliberately split in two, and where the split falls is the whole security
// design:
//
//   - Decrypt (below) runs on the OPERATOR'S machine, in the CLI. The age private key
//     never travels, is never typed into a web page, and is never held by the server.
//     The server cannot read its own backups, which is the property that makes an
//     attacker who owns the box unable to also own its backup history.
//
//   - Load (below) runs on the server, because the server is the thing holding the
//     Docker socket. It receives an already-decrypted stream and pipes it into the
//     database.
//
// The plaintext dump does pass through the server on its way back into the database.
// That is unavoidable — the server is what can reach the container — and it is a much
// weaker exposure than handing over the key, which would compromise every snapshot ever
// taken, including the ones from before the attacker arrived.

// Decrypt turns a stored snapshot back into a plain dump. Runs client-side.
//
// identity may be nil for an unencrypted snapshot; passing one for a snapshot that was
// never encrypted (or omitting one for a snapshot that was) is an error worth saying out
// loud rather than failing with a mysterious gzip complaint.
func Decrypt(src io.Reader, encrypted bool, identity age.Identity) (io.Reader, error) {
	r := src

	if encrypted {
		if identity == nil {
			return nil, fmt.Errorf("backups: this snapshot is encrypted — an age private key is required to read it")
		}
		dec, err := age.Decrypt(src, identity)
		if err != nil {
			return nil, fmt.Errorf("backups: could not decrypt (is this the right key?): %w", err)
		}
		r = dec
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		if !encrypted {
			return nil, fmt.Errorf("backups: this does not look like a gzip stream — is the snapshot encrypted? (its name would end in .age): %w", err)
		}
		return nil, fmt.Errorf("backups: the decrypted data is not valid gzip: %w", err)
	}
	return gz, nil
}

// ParseIdentity reads an age private key, as written by `age-keygen`. Accepts the key
// itself or the contents of a key file (which has comment lines).
func ParseIdentity(raw string) (age.Identity, error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("backups: that is not a valid age private key (it should start with AGE-SECRET-KEY-)")
		}
		return id, nil
	}
	return nil, fmt.Errorf("backups: no key found")
}

// Load pipes a plain dump into the database container. Runs SERVER-side.
//
// This is the single most destructive thing Daffa can do — it overwrites a live database
// — so the caller is expected to have demanded confirmation and to audit it. This
// function just does the work.
func Load(ctx context.Context, node *dockerx.Node, containerRef string, spec Spec, dump io.Reader) (string, error) {
	cmd, err := spec.restoreCommand()
	if err != nil {
		return "", err
	}

	created, err := node.Client.ContainerExecCreate(ctx, containerRef, container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	})
	if err != nil {
		return "", fmt.Errorf("backups: preparing the restore in %s: %w", containerRef, err)
	}

	att, err := node.Client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", fmt.Errorf("backups: starting the restore: %w", err)
	}
	defer att.Close()

	// Feed the dump in, then close stdin — psql and mysql keep waiting for more input
	// otherwise, and the restore hangs forever looking like it is still working.
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(att.Conn, dump)
		_ = att.CloseWrite()
		copyErr <- err
	}()

	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil && err != io.EOF {
		return out.String(), fmt.Errorf("backups: reading the restore output: %w", err)
	}
	if err := <-copyErr; err != nil {
		return out.String(), fmt.Errorf("backups: sending the dump: %w", err)
	}

	inspect, err := node.Client.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return out.String(), fmt.Errorf("backups: could not confirm the restore finished: %w", err)
	}

	output := strings.TrimSpace(out.String())
	if inspect.ExitCode != 0 {
		return output, fmt.Errorf("backups: the restore failed (exit %d)", inspect.ExitCode)
	}
	return output, nil
}
