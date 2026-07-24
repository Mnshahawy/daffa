package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
	"github.com/Mnshahawy/daffa/internal/volumes"
)

// The volume-source reconciler: make every node's copy of the volume hold exactly what
// the repo's subtree holds, and nothing else Daffa ever wrote. Same shape as the cert
// delivery reconciler — desired state, content hash, fan-out — plus the one thing certs
// never needed: mirrored deletion, scoped by the manifest to files Daffa itself delivered.

// reportVolumeSourceSync syncs one source and records the outcome on it.
func (s *Server) reportVolumeSourceSync(ctx context.Context, v *store.VolumeSource) error {
	hash, commit, warnings, err := s.syncVolumeSource(ctx, v)
	if err == nil && hash == v.SyncedHash && commit == v.SyncedCommit && v.Status == "ok" {
		return nil // nothing changed, nothing written, nothing to record
	}
	_ = s.store.MarkVolumeSourceSynced(ctx, v.ID, hash, commit, warnings, err)
	if err == nil {
		v.SyncedHash, v.SyncedCommit, v.Warnings = hash, commit, warnings
		v.Status, v.LastError = "ok", ""
	} else {
		v.Status, v.LastError = "error", err.Error()
	}
	return err
}

// syncVolumeSource makes the volume on every node of the source's environment hold the
// resolved subtree. Returns the content hash and commit it delivered (or should have).
func (s *Server) syncVolumeSource(ctx context.Context, v *store.VolumeSource) (hash, commit, warnings string, err error) {
	// Resolve the desired files, from git or from the inline set stored in Daffa. Everything
	// after this point — hashing, the manifest, the ordered per-node write — is identical:
	// the whole reason inline is a source KIND and not a parallel feature.
	var rt *stacks.ResolvedTree
	if v.SourceKind == "inline" {
		rt, err = s.inlineTree(ctx, v)
	} else {
		var auth *stacks.GitAuth
		if auth, err = s.gitAuth(ctx, v.GitCredentialID); err == nil {
			rt, err = stacks.ResolveTree(ctx, stacks.Source{
				Kind: "git", URL: v.GitURL, Ref: v.GitRef, Path: v.GitPath, Auth: auth,
				CABundle: s.managedCABundle(ctx),
			})
		}
	}
	if err != nil {
		return "", "", "", err
	}
	hash = volumeSourceHash(rt, v.UID, v.GID)
	commit = rt.CommitSHA
	warnings = strings.Join(rt.Warnings, "\n")

	if hash == v.SyncedHash && v.Status == "ok" {
		return hash, commit, warnings, nil // the volume already holds this
	}

	// A volume may have a second writer: a Traefik certificate delivery, sharing the one
	// dynamic directory Traefik is able to read. That works because each side mirrors only
	// its own manifest — but only as long as their file NAMES stay disjoint. A repo that
	// carries its own tls.yml would fight the delivery forever, each rewriting the other at
	// its own cadence, and both reporting ok. Refuse, and name the file.
	if err := s.refuseDeliveryFileClash(ctx, v, rt); err != nil {
		return hash, commit, warnings, err
	}

	env, err := s.pool.Get(v.EnvID)
	if err != nil {
		return hash, commit, warnings, fmt.Errorf("the environment is not connected")
	}

	files := make([]volumes.File, 0, len(rt.Files))
	names := make([]string, 0, len(rt.Files))
	current := make(map[string]bool, len(rt.Files))
	for _, f := range rt.Files {
		files = append(files, volumes.File{Name: f.Name, Data: f.Data, Mode: f.Mode})
		names = append(names, f.Name)
		current[f.Name] = true
	}
	// The manifest is written LAST, alone, as the commit point — see the node loop.
	manifest := []volumes.File{{Name: volumes.ManifestName, Data: volumes.Manifest(commit, hash, names)}}

	// Every node, like cert deliveries: a local volume exists per machine, and the
	// consumer may be on any of them (or move).
	var errs []string
	for _, node := range env.Nodes() {
		// The previous manifest bounds what a mirror may delete: exactly the files Daffa
		// wrote that the repo no longer contains. Files the CONSUMER wrote beside them
		// (acme.json, a plugin's cache) are never listed, read, or touched. No manifest —
		// first sync, or a hand-made volume — means a plain overlay: deletes nothing.
		var stale []string
		prev, err := volumes.ReadFile(ctx, node, v.Volume, volumes.ManifestName)
		switch {
		case err == nil:
			for _, name := range volumes.ParseManifest(prev) {
				if !current[name] {
					stale = append(stale, name)
				}
			}
		case errors.Is(err, volumes.ErrNotExist), errors.Is(err, volumes.ErrNoVolume):
			// overlay
		default:
			// A node that cannot answer must fail the sync, not silently skip its
			// deletions — a stale Traefik fragment that keeps routing is the exact
			// config drift this feature exists to end.
			errs = append(errs, fmt.Sprintf("%s: reading the previous manifest: %v", node.Name, err))
			continue
		}

		// Order is load-bearing, proven against a live daemon:
		//  1. content first (a brief moment where old and new both exist beats one
		//     where neither does),
		//  2. stale removal second,
		//  3. the manifest LAST, alone, as the commit point.
		// The first cut wrote the new manifest alongside the content, before the
		// removal — so a failed removal lost the stale list forever: the next sync read
		// the new manifest, computed nothing stale, and reported ok over an orphaned
		// file. Exactly the drift this feature exists to end, reported as success.
		if err := volumes.Write(ctx, node, v.Volume, files, v.UID, v.GID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		if err := volumes.RemoveFiles(ctx, node, v.Volume, stale); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		if err := volumes.Write(ctx, node, v.Volume, manifest, v.UID, v.GID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		// Only reached when content actually changed — the hash short-circuit above.
		restartTargets(ctx, node, v.RestartTargets)
	}
	if len(errs) > 0 {
		return hash, commit, warnings, fmt.Errorf("syncing %s: %s", v.Volume, strings.Join(errs, "; "))
	}
	return hash, commit, warnings, nil
}

// refuseDeliveryFileClash stops a volume source from writing a filename that a certificate
// delivery on the same volume owns. The source is the side that refuses because it is the
// side whose contents an operator edits; the delivery's file set is Daffa's own and is
// authoritative. See mixed-config-volumes.md.
//
// This is the sync-time backstop. Inline sources are also checked when they are saved (see
// refuseDeliveryOwnedNames), which is the error an operator should normally see — but a git
// subtree's contents are only known after a clone, so for those this is the first and only
// chance to refuse.
func (s *Server) refuseDeliveryFileClash(ctx context.Context, v *store.VolumeSource, rt *stacks.ResolvedTree) error {
	names := make([]string, 0, len(rt.Files))
	for _, f := range rt.Files {
		names = append(names, f.Name)
	}
	if err := s.refuseDeliveryOwnedNames(ctx, v.EnvID, v.Volume, names); err != nil {
		return fmt.Errorf("the subtree contains a file that clashes: %w", err)
	}
	return nil
}

// probeVolumeSourceGit proves a git source is deployable before a switch commits: it clones the
// repo and resolves the subtree — the exact resolution syncVolumeSource performs — so a bad
// URL/ref/path or a missing credential is rejected as the operator's 400 now, rather than surfacing
// as a red status after the source has already been converted. Mirrors the stack switch's pre-flight.
func (s *Server) probeVolumeSourceGit(ctx context.Context, v *store.VolumeSource) error {
	auth, err := s.gitAuth(ctx, v.GitCredentialID)
	if err != nil {
		return err
	}
	_, err = stacks.ResolveTree(ctx, stacks.Source{
		Kind: "git", URL: v.GitURL, Ref: v.GitRef, Path: v.GitPath, Auth: auth,
		CABundle: s.managedCABundle(ctx),
	})
	return err
}

// inlineTree builds the same ResolvedTree shape a git clone produces, from the files stored
// on an inline source. No commit — an inline source has none and should not pretend to.
func (s *Server) inlineTree(ctx context.Context, v *store.VolumeSource) (*stacks.ResolvedTree, error) {
	files, err := s.store.VolSourceFiles(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	rt := &stacks.ResolvedTree{Files: make([]stacks.TreeFile, 0, len(files))}
	for _, f := range files {
		rt.Files = append(rt.Files, stacks.TreeFile{Name: f.Path, Data: []byte(f.Content), Mode: f.Mode})
	}
	return rt, nil
}

// syncStackVolumeSources syncs every source linked to a stack. Deploys call it before the
// runner starts: a stack must not come up against config Daffa knows is stale, and the
// sync's VolumeCreate is what lets an `external: true` volume exist before compose looks
// for it on a fresh node.
func (s *Server) syncStackVolumeSources(ctx context.Context, stack *store.Stack) error {
	sources, err := s.store.VolumeSourcesByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	var errs []string
	for _, v := range sources {
		if err := s.reportVolumeSourceSync(ctx, v); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", v.Volume, err))
			continue
		}
		// A volume a source just filled may also carry certificates — the mixed dynamic
		// directory. Reconcile them here too, or a fresh node deploys a Traefik that finds
		// its middlewares present and its tls.yml missing until the next sweep. The VOLUME
		// is the join key: a delivery needs no link to the stack to be found this way.
		for _, d := range s.deliveriesForVolume(ctx, v.EnvID, v.Volume) {
			if err := s.reportDeliverySync(ctx, d); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", v.Volume, err))
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// deliveriesForVolume is the lookup above, with its error swallowed on purpose: a store
// read that fails here must not fail a deploy whose config sync already succeeded. The
// deliveries are reconciled by the background sweep regardless.
func (s *Server) deliveriesForVolume(ctx context.Context, envID, volume string) []*store.CertDelivery {
	list, err := s.store.DeliveriesForVolume(ctx, envID, volume)
	if err != nil {
		return nil
	}
	return list
}

// volumeSourceHash is the desired state: names, modes, contents, ownership. Not the tar
// (mtimes would rewrite every volume on every sweep), and not the commit — a force-push
// that lands identical content should not bounce every restart target.
func volumeSourceHash(rt *stacks.ResolvedTree, uid, gid int) string {
	h := sha256.New()
	for _, f := range rt.Files {
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", f.Name, f.Mode, len(f.Data))
		h.Write(f.Data)
	}
	fmt.Fprintf(h, "uid=%d gid=%d", uid, gid)
	return hex.EncodeToString(h.Sum(nil))
}
