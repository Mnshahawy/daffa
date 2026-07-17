// Package volumes moves file content into and out of named Docker volumes.
//
// The mechanism is the stack runner's trick, first proven by cert delivery: everything is
// a Docker API call (volume create, container create, CopyToContainer/CopyFromContainer),
// so it works identically against the local socket and through an agent tunnel. No exec
// into any user container, no long-lived agent, nothing new listening — and no host
// filesystem path anywhere, which keeps this surface free of traversal and shell-injection
// bugs by construction rather than by defense.
package volumes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/stacks"
)

// mountPath is where the helper mounts the volume. Internal to the helper's lifetime;
// nothing outside this package ever sees it.
const mountPath = "/data"

// The helper's sleep is a backstop, not a schedule: it is force-removed as soon as the
// operation finishes. Writes are one tar copy; snapshots stream an entire volume through
// gzip, age and an S3 upload, and must not have the floor pulled out mid-transfer.
const (
	writeTTL    = 2 * time.Minute
	snapshotTTL = 24 * time.Hour
)

// File is one file to place in a volume. A zero Mode means 0644 — mode policy belongs to
// callers (cert delivery makes keys 0600; git sources map the executable bit), not here.
type File struct {
	Name string
	Data []byte
	Mode int64
}

// ErrNotExist reports that a path is not present in the volume.
var ErrNotExist = errors.New("volumes: no such file in the volume")

// ErrNoVolume reports that the named volume itself does not exist on the node. Read
// paths return it instead of conjuring an empty volume into being.
var ErrNoVolume = errors.New("volumes: no such volume")

// Write puts the files into a named volume on one node, creating the volume if needed,
// via a throwaway helper container. The helper runs `sleep` for the duration of one copy:
// CopyToContainer into a RUNNING container writes through live mounts into the volume,
// whereas a copy into a stopped one lands in the container layer and is shadowed the
// moment the volume mounts over it — the quiet failure mode this three-step dance exists
// to avoid.
func Write(ctx context.Context, node *dockerx.Node, volumeName string, files []File, uid, gid int) error {
	ctx, cancel := context.WithTimeout(ctx, writeTTL)
	defer cancel()

	if _, err := node.Client.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName}); err != nil {
		return fmt.Errorf("creating volume %s: %w", volumeName, err)
	}

	id, cleanup, err := startHelper(ctx, node, volumeName, false, writeTTL)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := node.Client.CopyToContainer(ctx, id, mountPath,
		tarFiles(files, uid, gid), container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("writing into %s: %w", volumeName, err)
	}
	return nil
}

// ReadFile returns one file's content from a named volume, or ErrNotExist. The volume is
// not created if absent — reading is never allowed to conjure an empty volume into being.
func ReadFile(ctx context.Context, node *dockerx.Node, volumeName, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTTL)
	defer cancel()

	if err := mustExist(ctx, node, volumeName); err != nil {
		return nil, err
	}

	id, cleanup, err := startHelper(ctx, node, volumeName, true, writeTTL)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rc, _, err := node.Client.CopyFromContainer(ctx, id, path.Join(mountPath, name))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("reading %s from %s: %w", name, volumeName, err)
	}
	defer rc.Close()

	// The daemon answers with a tar of the requested path; the file is its first (and for
	// a regular file, only) regular entry.
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNotExist
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w", name, volumeName, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		const limit = 8 << 20 // nothing this package reads back is measured in tens of megabytes
		b, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w", name, volumeName, err)
		}
		if len(b) > limit {
			return nil, fmt.Errorf("reading %s from %s: the file exceeds %d bytes", name, volumeName, limit)
		}
		return b, nil
	}
}

// Snapshot streams the volume's entire contents as a tar archive, exactly as the daemon
// builds it — no tar binary in the helper, no attach-stream demultiplexing. The volume is
// mounted read-only; a volume that does not exist is an error, not an empty archive
// pretending to be a backup. The helper container stays alive until the returned stream
// is closed, so Close it even on error paths.
//
// Entries come back rooted at "./" (the trailing "/." asks the daemon for the directory's
// contents rather than the directory), which is exactly the shape CopyToContainer accepts
// on restore — the backup format is the delivery format.
func Snapshot(ctx context.Context, node *dockerx.Node, volumeName string) (io.ReadCloser, error) {
	if err := mustExist(ctx, node, volumeName); err != nil {
		return nil, err
	}

	id, cleanup, err := startHelper(ctx, node, volumeName, true, snapshotTTL)
	if err != nil {
		return nil, err
	}

	rc, _, err := node.Client.CopyFromContainer(ctx, id, mountPath+"/.")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("reading volume %s: %w", volumeName, err)
	}
	return &snapshotStream{rc: rc, cleanup: cleanup}, nil
}

type snapshotStream struct {
	rc      io.ReadCloser
	cleanup func()
}

func (s *snapshotStream) Read(p []byte) (int, error) { return s.rc.Read(p) }

func (s *snapshotStream) Close() error {
	err := s.rc.Close()
	s.cleanup()
	return err
}

// mustExist guards the read paths. Mounting a nonexistent named volume would auto-create
// it, and a snapshot of a mistyped volume name would then succeed with zero files —
// a "backup" that only reveals itself the day it is needed.
func mustExist(ctx context.Context, node *dockerx.Node, volumeName string) error {
	if _, err := node.Client.VolumeInspect(ctx, volumeName); err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("volume %s does not exist on %s: %w", volumeName, node.Name, ErrNoVolume)
		}
		return fmt.Errorf("inspecting volume %s: %w", volumeName, err)
	}
	return nil
}

// RemoveFiles deletes the named files from a volume. The helper's command IS the explicit
// file list — computed by Daffa from its own manifests, never from user input — and it
// exits when done; the exit code is checked, because a removal that silently failed would
// leave stale config in place looking current. Names that do not stay inside the volume
// are refused: they cannot come from a manifest Daffa wrote, so their presence means the
// manifest is not ours to trust.
func RemoveFiles(ctx context.Context, node *dockerx.Node, volumeName string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeTTL)
	defer cancel()

	if err := mustExist(ctx, node, volumeName); err != nil {
		return err
	}

	cmd := []string{"rm", "-f", "--"}
	for _, n := range names {
		clean := path.Clean(n)
		if clean == "" || clean == "." || clean == ".." ||
			strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("refusing to remove %q from %s — it does not stay inside the volume", n, volumeName)
		}
		cmd = append(cmd, path.Join(mountPath, clean))
	}
	return runHelper(ctx, node, volumeName, cmd, fmt.Sprintf("removing stale files from %s", volumeName))
}

// runHelper runs one command in a throwaway container with the volume mounted, waits for
// it, and checks the exit code — a helper that silently failed would leave the volume in
// a state that looks intended. The command is always a fixed argument list computed by
// this package's callers; nothing user-controlled ever joins it.
func runHelper(ctx context.Context, node *dockerx.Node, volumeName string, cmd []string, doing string) error {
	if err := ensureRunnerImage(ctx, node); err != nil {
		return err
	}
	created, err := node.Client.ContainerCreate(ctx,
		&container.Config{
			Image: stacks.RunnerImage,
			// Entrypoint, not Cmd — same reason as startHelper: the image's entrypoint
			// turns `rm` into `docker rm`.
			Entrypoint: cmd,
			Labels:     map[string]string{"daffa.volsync": volumeName},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: mountPath,
			}},
		}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("%s: creating the helper: %w", doing, err)
	}
	defer func() {
		_ = node.Client.ContainerRemove(context.WithoutCancel(ctx), created.ID,
			container.RemoveOptions{Force: true})
	}()

	if err := node.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("%s: starting the helper: %w", doing, err)
	}
	waitC, errC := node.Client.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case res := <-waitC:
		if res.StatusCode != 0 {
			return fmt.Errorf("%s: %s exited %d", doing, cmd[0], res.StatusCode)
		}
	case err := <-errC:
		return fmt.Errorf("%s: %w", doing, err)
	}
	return nil
}

// startHelper creates and starts the throwaway container with the volume mounted, and
// returns its id plus a cleanup that force-removes it. Cleanup survives a canceled ctx —
// a helper that outlives its operation is a leak on someone else's box.
func startHelper(ctx context.Context, node *dockerx.Node, volumeName string, readOnly bool, ttl time.Duration) (string, func(), error) {
	if err := ensureRunnerImage(ctx, node); err != nil {
		return "", nil, err
	}

	created, err := node.Client.ContainerCreate(ctx,
		&container.Config{
			Image: stacks.RunnerImage,
			// Entrypoint, not Cmd: the runner image's own entrypoint rewrites any
			// command that happens to be a docker subcommand into `docker <cmd>` —
			// `rm` is one, and a helper that runs `docker rm` against a missing socket
			// exits 1 having deleted nothing. Found against a live daemon; do not
			// "simplify" this back to Cmd.
			Entrypoint: []string{"sleep", strconv.Itoa(int(ttl / time.Second))},
			Labels: map[string]string{
				"daffa.volsync": volumeName,
			},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{{
				Type:     mount.TypeVolume,
				Source:   volumeName,
				Target:   mountPath,
				ReadOnly: readOnly,
			}},
		}, nil, nil, "")
	if err != nil {
		return "", nil, fmt.Errorf("creating the sync helper: %w", err)
	}

	cleanup := func() {
		_ = node.Client.ContainerRemove(context.WithoutCancel(ctx), created.ID,
			container.RemoveOptions{Force: true})
	}

	if err := node.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("starting the sync helper: %w", err)
	}
	return created.ID, cleanup, nil
}

// tarFiles builds the archive CopyToContainer wants. Ownership comes from the caller, so
// a consumer that drops privileges can still read its own files; order is sorted so the
// same file set always produces the same archive.
func tarFiles(files []File, uid, gid int) io.Reader {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, f := range sorted {
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		_ = tw.WriteHeader(&tar.Header{
			Name: f.Name, Mode: mode, Size: int64(len(f.Data)),
			Uid: uid, Gid: gid, ModTime: now,
		})
		_, _ = tw.Write(f.Data)
	}
	_ = tw.Close()
	return &buf
}

func ensureRunnerImage(ctx context.Context, node *dockerx.Node) error {
	if _, _, err := node.Client.ImageInspectWithRaw(ctx, stacks.RunnerImage); err == nil {
		return nil
	}
	rc, err := node.Client.ImagePull(ctx, stacks.RunnerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling the helper image: %w", err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("pulling the helper image: %w", err)
	}
	return nil
}
