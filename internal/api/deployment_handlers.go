package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// ── deployments ─────────────────────────────────────────────────────────────────
//
// A deployment record existed long before this file did. What did not exist was any way to
// REACH one: the log lived as a selection inside the stack page, so a failed deploy had no URL
// and could not be sent to anybody, and no view crossed stacks — which is exactly the view you
// want when you know something broke but not yet what. See docs/stacks.md §3.

type deploymentView struct {
	ID      string `json:"id"`
	StackID string `json:"stack_id"`
	// StackName and EnvID travel with the row because the cross-stack feed has to say WHICH
	// stack on WHICH host, and making the browser join that against two other lists would be
	// three requests to render one table.
	StackName string `json:"stack_name,omitempty"`
	EnvID     string `json:"env_id,omitempty"`

	Action      string `json:"action"`
	Status      string `json:"status"` // running | ok | failed | cancelled
	Engine      string `json:"engine"`
	TriggerKind string `json:"trigger_kind"` // manual | webhook | rollback
	// StartedBy is the user's id; StartedByName is who that is. A deployment attributed to
	// "u-3f9c2a" is a deployment nobody can ask about.
	StartedBy     string `json:"started_by,omitempty"`
	StartedByName string `json:"started_by_name,omitempty"`

	ExitCode     *int   `json:"exit_code,omitempty"`
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"log_truncated,omitempty"`

	CommitSHA     string `json:"commit_sha,omitempty"`
	CommitSubject string `json:"commit_subject,omitempty"`
	RollbackOf    string `json:"rollback_of,omitempty"`

	// Redeployable says this one can be put back: a succeeded up/pull whose compose file we
	// still have. The button is rendered from this rather than from the browser re-deriving
	// the rule, so the rule lives in one place.
	Redeployable bool `json:"redeployable"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// deployStartedResponse is what a route that STARTS a deploy answers: the id to follow.
// The log is not in here — it streams from GET /api/deployments/{id}/logs.
type deployStartedResponse struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"` // always "running": the deploy just started
}

func viewDeployment(d *store.Deployment) deploymentView {
	v := deploymentView{
		ID: d.ID, StackID: d.StackID, Action: d.Action, Status: d.Status, Engine: d.Engine,
		TriggerKind: d.TriggerKind, StartedBy: d.StartedBy, ExitCode: d.ExitCode,
		Log: d.Log, LogTruncated: d.LogTruncated,
		CommitSHA: d.CommitSHA, CommitSubject: d.CommitSubject, RollbackOf: d.RollbackOf,
		Redeployable: redeployable(d),
		StartedAt:    d.StartedAt,
	}
	if !d.EndedAt.IsZero() {
		t := d.EndedAt
		v.EndedAt = &t
	}
	return v
}

// redeployable reports whether a deployment can be re-applied.
//
// Only a SUCCEEDED up/pull, and only one whose resolved compose file we still have. Re-applying
// a failed deploy would put back a state that never worked; re-applying a `stop` or a `down` is
// not a rollback, it is just doing that again, and the buttons for those are already on the
// stack page.
func redeployable(d *store.Deployment) bool {
	if d.Status != store.DeployOK || d.ComposeYAML == "" {
		return false
	}
	a := stacks.Action(d.Action)
	return a == stacks.ActionUp || a == stacks.ActionPull
}

// nameFor resolves user ids to something a person can act on. One query for the whole page:
// a feed of fifty deployments must not be fifty user lookups.
func (s *Server) nameFor(ctx context.Context, deps []deploymentView) {
	want := map[string]bool{}
	for _, d := range deps {
		if d.StartedBy != "" {
			want[d.StartedBy] = true
		}
	}
	if len(want) == 0 {
		return
	}

	names := map[string]string{}
	for id := range want {
		u, err := s.store.UserByID(ctx, id)
		if err != nil {
			continue // a deleted user still has deployments; they just lose their name
		}
		names[id] = u.Label()
	}
	for i := range deps {
		deps[i].StartedByName = names[deps[i].StartedBy]
	}
}

// handleListStackDeployments is one stack's history.
func (s *Server) handleListStackDeployments(w http.ResponseWriter, r *http.Request) {
	stack, ok := s.stack(w, r)
	if !ok {
		return
	}

	list, err := s.store.ListDeployments(r.Context(), stack.ID, 20)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]deploymentView, 0, len(list))
	for _, d := range list {
		v := viewDeployment(d)
		v.Log = "" // the list does not carry logs; the log endpoint serves them one at a time
		v.StackName, v.EnvID = stack.Name, stack.EnvID
		out = append(out, v)
	}
	s.nameFor(r.Context(), out)
	httpx.JSON(w, http.StatusOK, out)
}

// handleRecentDeployments is the cross-stack feed — the answer to "something broke, and I do
// not know where yet".
func (s *Server) handleRecentDeployments(w http.ResponseWriter, r *http.Request) {
	global, envs := visible(r, caps.StacksView)

	f := store.DeploymentFilter{
		Status:  r.URL.Query().Get("status"),
		StackID: r.URL.Query().Get("stack"),
		EnvID:   r.URL.Query().Get("host"),
		Trigger: r.URL.Query().Get("trigger"),
		Limit:   50,
	}
	if before := r.URL.Query().Get("before"); before != "" {
		t, err := time.Parse(time.RFC3339, before)
		if err != nil {
			httpx.BadRequest(w, r, "`before` must be an RFC 3339 timestamp.")
			return
		}
		f.Before = t
	}

	list, err := s.store.RecentDeployments(r.Context(), global, envs, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The stack a deployment belongs to is what the feed is actually about — its name and its
	// host are the two columns that make the row mean anything.
	stackByID := map[string]*store.Stack{}
	if list != nil {
		all, err := s.store.ListStacks(r.Context(), global, envs)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		for _, st := range all {
			stackByID[st.ID] = st
		}
	}

	out := make([]deploymentView, 0, len(list))
	for _, d := range list {
		v := viewDeployment(d)
		if st, ok := stackByID[d.StackID]; ok {
			v.StackName, v.EnvID = st.Name, st.EnvID
		}
		out = append(out, v)
	}
	s.nameFor(r.Context(), out)
	httpx.JSON(w, http.StatusOK, out)
}

// handleDeploymentDetail is the page you can send to somebody.
func (s *Server) handleDeploymentDetail(w http.ResponseWriter, r *http.Request) {
	dep, stack, ok := s.deployment(w, r)
	if !ok {
		return
	}

	v := viewDeployment(dep)
	v.StackName, v.EnvID = stack.Name, stack.EnvID
	list := []deploymentView{v}
	s.nameFor(r.Context(), list)

	// The log is not in here: it comes over SSE, from one URL that serves a live deploy and a
	// finished one alike. See handleDeploymentLogs.
	list[0].Log = ""
	httpx.JSON(w, http.StatusOK, list[0])
}

// handleDeploymentLogs streams a deployment's output while it is happening, and replays the
// recorded log once it is over — so the same URL works whether you open it during the deploy or
// a week later. That is the entire point of a deployment having a URL.
//
// A running deployment is not one container: a hooked pipeline is several in sequence, each
// force-removed the moment its phase ends, and RunnerCtrID re-pointed at the next. So the live
// path is a LOOP over the row — an empty id means the pipeline is between phases (attach too
// early and the daemon answers its literal "page not found", which painted error banners over
// perfectly good deploys), a new id is the next phase to follow, a vanished container is the
// gap, not a failure. When the row leaves `running`, the stored log is sent as a REPLACE: it
// carries the phase headers and hook output the container streams never did, so the page ends
// up showing the same log a later visitor gets.
func (s *Server) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	dep, stack, ok := s.deployment(w, r)
	if !ok {
		return
	}

	sse, err := httpx.NewSSE(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if dep.Status != store.DeployRunning {
		_ = sse.Send("log", map[string]string{"text": dep.Log})
		_ = sse.Send("end", map[string]any{"status": dep.Status, "exit_code": dep.ExitCode})
		return
	}

	node, err := s.runnerNode(r.Context(), stack)
	if err != nil {
		_ = sse.Send("error", map[string]string{"message": "the environment this stack runs on is not connected"})
		return
	}

	ctx := r.Context()
	lastCtr := ""
	for {
		cur, err := s.store.DeploymentByID(ctx, dep.ID)
		if err != nil {
			return
		}
		if cur.Status != store.DeployRunning {
			_ = sse.Send("log", map[string]any{"text": cur.Log, "replace": true})
			_ = sse.Send("end", map[string]any{"status": cur.Status, "exit_code": cur.ExitCode})
			return
		}

		if cur.RunnerCtrID == "" || cur.RunnerCtrID == lastCtr {
			// Between phases, or the phase we already streamed has not been replaced
			// yet. Poll the row, not the daemon.
			select {
			case <-ctx.Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
			continue
		}

		lastCtr = cur.RunnerCtrID
		err = stacks.StreamLogs(ctx, node, lastCtr, func(chunk string) error {
			return sse.Send("log", map[string]string{"text": chunk})
		})
		if err != nil && !errors.Is(err, stacks.ErrRunnerGone) && ctx.Err() == nil {
			_ = sse.Send("error", map[string]string{"message": err.Error()})
			return
		}
		// Stream over (phase done) or container already gone (we were late): loop —
		// either the next phase's id appears or the row leaves `running`.
	}
}

// handleCancelDeployment kills a deploy that is not going to finish.
func (s *Server) handleCancelDeployment(w http.ResponseWriter, r *http.Request) {
	dep, stack, ok := s.deployment(w, r)
	if !ok {
		return
	}

	// Flag it BEFORE killing the runner. The daemon cannot tell a killed runner from a broken
	// one — both just exit non-zero — so the flag is the only thing that will let the watcher
	// record this as cancelled rather than failed. Killing first would leave a window in which
	// the runner dies, the watcher reads a not-yet-set flag, and a deliberate cancel is
	// reported as a failure and emailed to everybody.
	flagged, err := s.store.RequestCancel(r.Context(), dep.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !flagged {
		httpx.Fail(w, r, http.StatusConflict, "not_running",
			"This deployment has already finished, so there is nothing to cancel.")
		return
	}

	if dep.RunnerCtrID != "" {
		node, err := s.runnerNode(r.Context(), stack)
		if err != nil {
			httpx.Fail(w, r, http.StatusConflict, "host_unreachable",
				"The environment this stack runs on is not connected, so its deploy cannot be stopped.")
			return
		}
		if err := stacks.Kill(r.Context(), node, dep.RunnerCtrID); err != nil {
			// Already gone is the good outcome, not an error: the watcher is about to record
			// it, and the cancel flag is already set, so it will be recorded as cancelled.
			httpx.Fail(w, r, http.StatusBadGateway, "cancel_failed",
				"The deploy runner could not be stopped.\n\n"+err.Error())
			return
		}
	}

	var userID string
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}
	s.audit(r.Context(), store.AuditEntry{
		UserID: userID, EnvID: stack.EnvID, Action: "stack.cancel", Target: stack.Name,
		Outcome: "ok", Detail: store.AuditDetail(map[string]any{"deployment": dep.ID}),
	})
	httpx.JSON(w, http.StatusOK, statusResponse{Status: "cancelling"})
}

// handleRedeployDeployment puts an earlier deployment back.
//
// It re-applies the compose file STORED ON THAT DEPLOYMENT — it does not go back to git. The
// branch may have moved, the tag may be gone, the repo may be unreachable: none of that should
// be able to stop you restoring the thing that worked. See docs/stacks.md §5.
func (s *Server) handleRedeployDeployment(w http.ResponseWriter, r *http.Request) {
	dep, stack, ok := s.deployment(w, r)
	if !ok {
		return
	}

	if !redeployable(dep) {
		httpx.Fail(w, r, http.StatusBadRequest, "not_redeployable",
			"Only a deploy that succeeded can be put back, and only while Daffa still has the "+
				"compose file it used.")
		return
	}

	var userID string
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}

	// Up, not pull: the point is to put back exactly what that deployment ran. Pulling `always`
	// would fetch whatever those tags resolve to NOW, which for a floating tag is the very thing
	// being rolled back from.
	next, err := s.deploy(r.Context(), stack, stacks.ActionUp, store.TriggerRollback, userID, dep)
	switch {
	case errors.Is(err, store.ErrRunInProgress):
		httpx.Fail(w, r, http.StatusConflict, "run_in_progress",
			"A deploy is already running for this stack. Wait for it to finish.")
		return
	case err != nil:
		httpx.Fail(w, r, http.StatusBadRequest, "deploy_failed", err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, deployStartedResponse{DeploymentID: next.ID, Status: "running"})
}

// deployment returns the deployment this request is about, and its stack.
//
// The scopeDeployment middleware already resolved both AND checked the route's capability on
// the stack's host, so this is a context read. The fallback exists only so a handler mounted
// without that middleware fails closed rather than silently skipping the check.
func (s *Server) deployment(w http.ResponseWriter, r *http.Request) (*store.Deployment, *store.Stack, bool) {
	if d, st, ok := deploymentFrom(r.Context()); ok {
		return d, st, true
	}
	httpx.Fail(w, r, http.StatusNotFound, "no_such_deployment", "No such deployment.")
	return nil, nil, false
}
