package volumes

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Restore streams a tar archive into a named volume — the exact inverse of Snapshot,
// because the backup format IS the delivery format. The volume is created if absent:
// disaster recovery targets a box that no longer has it, and that is the one write path
// allowed to conjure it.
//
// The REFUSALS (volume in use, volume not empty) belong to the caller, which can name
// the containers and demand the explicit wipe; this function just does the work, like
// backups.Load.
func Restore(ctx context.Context, node *dockerx.Node, volumeName string, archive io.Reader) error {
	if _, err := node.Client.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName}); err != nil {
		return fmt.Errorf("creating volume %s: %w", volumeName, err)
	}

	// No write timeout here, deliberately: this is someone's data coming back, and it
	// takes as long as it takes. The caller's context bounds it.
	id, cleanup, err := startHelper(ctx, node, volumeName, false, snapshotTTL)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := node.Client.CopyToContainer(ctx, id, mountPath, archive,
		container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("restoring into %s: %w", volumeName, err)
	}
	return nil
}

// cleanEntryName strips the shapes a daemon-built tar roots its entries with ("./", the
// bare directory) down to the file's own name; empty means the entry IS the root.
func cleanEntryName(name string) string {
	name = path.Clean(name)
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return name
}

// IsEmpty reports whether the volume holds nothing. It reads the snapshot stream only as
// far as the first real entry — the daemon's tar of an empty directory still carries the
// directory itself, which does not count.
func IsEmpty(ctx context.Context, node *dockerx.Node, volumeName string) (bool, error) {
	rc, err := Snapshot(ctx, node, volumeName)
	if err != nil {
		return false, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspecting %s: %w", volumeName, err)
		}
		if name := cleanEntryName(h.Name); name != "" {
			return false, nil
		}
	}
}

// Wipe empties a volume. Destructive by definition, so callers gate it behind an explicit
// flag and audit it — this function just does the work. busybox find, not a shell: the
// command is a fixed argument list, and nothing user-controlled ever joins it.
func Wipe(ctx context.Context, node *dockerx.Node, volumeName string) error {
	if err := mustExist(ctx, node, volumeName); err != nil {
		return err
	}
	return runHelper(ctx, node, volumeName,
		[]string{"find", mountPath, "-mindepth", "1", "-delete"},
		fmt.Sprintf("emptying %s", volumeName))
}
