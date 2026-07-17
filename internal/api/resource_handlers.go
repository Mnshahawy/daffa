package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// errSystemResource is the audit-log reason when a removal is refused because the target
// is protected by this deployment (a system network or volume). It never reaches the
// daemon; the handler answers a 400 with the operator-facing sentence.
var errSystemResource = errors.New("protected by this deployment")

// ── stats ───────────────────────────────────────────────────────────────────────

// handleStatsSnapshot samples the containers the client names. The CLIENT decides
// which ones — it knows what is on screen, and sampling a container nobody is looking
// at is work spent for nothing.
func (s *Server) handleStatsSnapshot(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}

	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	filtered := ids[:0]
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		httpx.JSON(w, http.StatusOK, []dockerx.Stats{})
		return
	}
	// A page cannot render hundreds of rows at once, so a request for hundreds is a
	// bug or an abuse; either way, refuse rather than hammer the daemon.
	if len(filtered) > 100 {
		httpx.BadRequest(w, r, "Too many containers in one snapshot (max 100).")
		return
	}

	stats, err := node.Snapshot(r.Context(), filtered)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client navigated away mid-sample
		}
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// handleStatsStream follows ONE container — the one on screen.
func (s *Server) handleStatsStream(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx := r.Context()
	err = node.StreamStats(ctx, r.PathValue("id"), func(st dockerx.Stats) error {
		return sse.Send("stats", st)
	})
	if err != nil && ctx.Err() == nil {
		_ = sse.Send("error", map[string]string{"message": err.Error()})
	}
}

// ── images / volumes / networks ─────────────────────────────────────────────────

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}
	images, err := fanOutErr(r.Context(), env,
		func(ctx context.Context, n *dockerx.Node) ([]dockerx.Image, error) { return n.ListImages(ctx) },
		func(i *dockerx.Image, n *dockerx.Node) { i.Node, i.NodeID = n.Name, n.ID })
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, images)
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"

	err := node.RemoveImage(r.Context(), id, force)
	s.auditResource(r, node.EnvID, "image.remove", id, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "remove_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// isSystemNetwork reports whether a network is protected from removal — Docker's own
// (bridge/host/none), or one this deployment named in DAFFA_SYSTEM_NETWORKS.
func (s *Server) isSystemNetwork(name string) bool {
	return dockerx.IsSystemNetwork(name) || slices.Contains(s.cfg.SystemNetworks, name)
}

// isSystemVolume reports whether a volume is protected from removal — one this deployment
// named in DAFFA_SYSTEM_VOLUMES. There are no built-in system volumes.
func (s *Server) isSystemVolume(name string) bool {
	return slices.Contains(s.cfg.SystemVolumes, name)
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}
	vols, err := fanOutErr(r.Context(), env,
		func(ctx context.Context, n *dockerx.Node) ([]dockerx.Volume, error) { return n.ListVolumes(ctx) },
		func(v *dockerx.Volume, n *dockerx.Node) { v.Node, v.NodeID = n.Name, n.ID })
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}
	for i := range vols {
		// The daemon has no notion of a system volume, so the flag is ours to set — and
		// it drives both the button the UI hides and the refusal below.
		vols[i].System = s.isSystemVolume(vols[i].Name)
	}
	httpx.JSON(w, http.StatusOK, vols)
}

func (s *Server) handleRemoveVolume(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	force := r.URL.Query().Get("force") == "true"

	// A volume this deployment depends on (its own database, the edge-certificate volume)
	// is refused before the daemon is ever asked — the same posture as bridge/host/none.
	if s.isSystemVolume(name) {
		s.auditResource(r, node.EnvID, "volume.remove", name, errSystemResource)
		httpx.Fail(w, r, http.StatusBadRequest, "system_volume",
			"That volume is part of this Daffa deployment and cannot be removed from here.")
		return
	}

	// The InUse rule, extended to volumes: an attachment saying the volume MATTERS refuses
	// the delete, with the fix named. The daemon already refuses while a container mounts
	// it; this catches the volume nothing mounts right now but Daffa knows is load-bearing.
	if src, err := s.store.VolumeSourceByVolume(r.Context(), node.EnvID, name); err == nil {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"This volume is kept in sync from "+src.GitURL+". Delete that volume source first — removing the volume underneath it would just get rewritten on the next sync.")
		return
	}
	if jobs, err := s.store.VolumeBackupJobNames(r.Context(), node.EnvID, name); err == nil && len(jobs) > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"This volume is backed up by "+strings.Join(jobs, ", ")+". Disable or delete those jobs first — a backup job pointed at a deleted volume fails every night from then on.")
		return
	}

	err := node.RemoveVolume(r.Context(), name, force)
	s.auditResource(r, node.EnvID, "volume.remove", name, err)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "remove_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// Networks are the one list that is BOTH. An overlay network is cluster-wide and every node
// reports it; a bridge network is node-local and only its own does. Fanning out therefore returns
// each overlay once per node — so they are deduplicated by id, and the first node to report one
// wins. An operator debugging service-to-service DNS needs to see the overlay, and does not need
// to see it three times.
func (s *Server) handleListNetworks(w http.ResponseWriter, r *http.Request) {
	env, ok := s.env(w, r)
	if !ok {
		return
	}
	nets, err := fanOutErr(r.Context(), env,
		func(ctx context.Context, n *dockerx.Node) ([]dockerx.Network, error) { return n.ListNetworks(ctx) },
		func(nw *dockerx.Network, n *dockerx.Node) { nw.Node, nw.NodeID = n.Name, n.ID })
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}

	seen := map[string]bool{}
	out := nets[:0]
	for _, nw := range nets {
		if seen[nw.ID] {
			continue
		}
		seen[nw.ID] = true
		// dockerx already flags bridge/host/none; add the ones this deployment protects.
		nw.System = nw.System || slices.Contains(s.cfg.SystemNetworks, nw.Name)
		out = append(out, nw)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (s *Server) handleRemoveNetwork(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	// Remove is by id, so resolve the name to check it against DAFFA_SYSTEM_NETWORKS before
	// asking the daemon. Built-in networks are caught by dockerx below either way; this adds
	// the deployment's own networks (the console's plumbing) to the refusal.
	if name, err := node.NetworkName(r.Context(), id); err == nil && s.isSystemNetwork(name) {
		s.auditResource(r, node.EnvID, "network.remove", name, errSystemResource)
		httpx.Fail(w, r, http.StatusBadRequest, "system_network",
			"That network is part of this Daffa deployment (or one of Docker's own) and cannot be removed from here.")
		return
	}

	err := node.RemoveNetwork(r.Context(), id)
	s.auditResource(r, node.EnvID, "network.remove", id, err)
	if err != nil {
		// Asking to remove bridge/host/none is the caller's mistake, not the daemon's
		// failure — a 400 with the sentence, not a 502 wearing dockerd's error.
		if errors.Is(err, dockerx.ErrSystemNetwork) {
			httpx.Fail(w, r, http.StatusBadRequest, "system_network",
				"bridge, host and none are Docker's own networks. They exist on every daemon and cannot be removed.")
			return
		}
		httpx.Fail(w, r, http.StatusBadGateway, "remove_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// ── disk usage & prune ──────────────────────────────────────────────────────────

func (s *Server) handleDiskUsage(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}
	du, err := node.DiskUsage(r.Context())
	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "docker_unreachable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, du)
}

// handlePrune is admin-only (the route enforces it): it deletes things in bulk, and
// the blast radius of a mistake is the whole host.
func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(w, r)
	if !ok {
		return
	}

	target := dockerx.PruneTarget(r.PathValue("target"))
	if !dockerx.ValidPruneTarget(target) {
		httpx.BadRequest(w, r, "Unknown prune target.")
		return
	}

	res, err := node.Prune(r.Context(), target)

	outcome, detail := "ok", map[string]any{}
	if err != nil {
		outcome = "error"
		detail["error"] = err.Error()
	} else {
		detail["deleted"] = res.Deleted
		detail["freed"] = res.Freed
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: node.EnvID, Action: "prune." + string(target), Outcome: outcome,
		Detail: store.AuditDetail(detail),
	})

	if err != nil {
		httpx.Fail(w, r, http.StatusBadGateway, "prune_failed", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// auditResource records a resource mutation and its outcome in one line, so each
// handler above does not repeat the same six.
func (s *Server) auditResource(r *http.Request, envID, action, target string, err error) {
	outcome := "ok"
	detail := map[string]any{}
	if err != nil {
		outcome = "error"
		detail["error"] = err.Error()
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: envID, Action: action, Target: target, Outcome: outcome,
		Detail: store.AuditDetail(detail),
	})
}
