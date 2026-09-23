package api

// Actions on alerts: what a firing monitor does besides telling someone. See
// .ai/monitor-actions.md.
//
// The evaluator decides WHEN (the transition to firing, once); this file decides WHAT, and
// holds the guardrails that make "restart it automatically" safe to leave on overnight:
//
//   - a cooldown per target, measured from when actions last STARTED, so fire → restart →
//     resolve → refire cannot become a restart loop;
//   - actions only on a monitor pinned to one cluster, created by someone holding the power
//     each action wields there (monitor_handlers.go checks, per kind);
//   - every action audited, and its outcome kept on the alert.
//
// An alert's target is a SERVICE — compose's or swarm's — or a standalone container, never a
// single replica (see store.Evaluate). So a restart restarts the service's containers one at
// a time, or force-updates the swarm service and lets its update policy roll the tasks; an
// exec runs in every replica, because the leak you are dumping is in all of them.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// actionsTimeout bounds one alert's whole sequence: five actions, the longest an exec's ten
// minutes, with room to spare. Past it, whatever is still running is abandoned and recorded.
const actionsTimeout = 20 * time.Minute

// RunActions is monitor.ActionRunner.
func (s *Server) RunActions(ctx context.Context, m *store.Monitor, a *store.Alert) {
	ctx, cancel := context.WithTimeout(ctx, actionsTimeout)
	defer cancel()

	cooldown := time.Duration(m.ActionCooldownSecs) * time.Second
	if last, err := s.store.LastActionAt(ctx, m.ID, a.ContainerName); err == nil && !last.IsZero() && time.Since(last) < cooldown {
		res := store.ActionResult{Kind: "cooldown", At: time.Now().UTC(), Detail: fmt.Sprintf(
			"Skipped: this monitor's actions last ran on %s at %s, and its cooldown is %s.",
			a.ContainerName, last.UTC().Format(time.RFC3339), cooldown)}
		_ = s.store.RecordAlertActions(ctx, a.ID, []store.ActionResult{res})
		s.auditAction(ctx, m, a, res)
		return
	}
	if err := s.store.ClaimAlertActions(ctx, a.ID); err != nil {
		s.auditAction(ctx, m, a, store.ActionResult{Kind: "claim", Detail: err.Error()})
		return
	}

	tgt, err := s.resolveActionTarget(ctx, a)
	results := make([]store.ActionResult, 0, len(m.Actions))
	for _, act := range m.Actions {
		var res store.ActionResult
		if err != nil {
			res = store.ActionResult{Kind: act.Kind, Detail: err.Error()}
		} else {
			// Every action runs even when one before it failed. A heap dump that could not be
			// written is no reason to leave a leaking service unrestarted — recovery first,
			// and the failed dump is on the record.
			res = s.runAction(ctx, act, m, a, tgt)
		}
		res.At = time.Now().UTC()
		results = append(results, res)
		s.auditAction(ctx, m, a, res)
	}
	if err := s.store.RecordAlertActions(ctx, a.ID, results); err != nil {
		s.auditAction(ctx, m, a, store.ActionResult{Kind: "record", Detail: err.Error()})
	}
}

func (s *Server) auditAction(ctx context.Context, m *store.Monitor, a *store.Alert, res store.ActionResult) {
	outcome := "ok"
	if !res.OK {
		outcome = "error"
	}
	s.audit(ctx, store.AuditEntry{
		EnvID: a.EnvID, Action: "monitor.action", Target: m.Name + " → " + a.ContainerName,
		Outcome: outcome, Detail: store.AuditDetail(map[string]string{"kind": res.Kind, "detail": res.Detail}),
	})
}

// actionTarget is what an alert is about, found on the live daemon: the swarm service when it
// is one, and the running containers either way.
type actionTarget struct {
	control      *dockerx.Node // a manager, when the cluster is a swarm
	swarmService string
	containers   []targetContainer
}

type targetContainer struct {
	node     *dockerx.Node
	id, name string
}

func (s *Server) resolveActionTarget(ctx context.Context, a *store.Alert) (*actionTarget, error) {
	env, err := s.pool.Get(a.EnvID)
	if err != nil {
		return nil, errors.New("the cluster is not connected, so nothing could be done to " + a.ContainerName)
	}
	t := &actionTarget{}
	if env.IsSwarm() {
		if control, err := env.Control(); err == nil {
			t.control = control
			if _, _, err := control.ServiceReplicas(ctx, a.ContainerName); err == nil {
				t.swarmService = a.ContainerName
			}
		}
	}
	for _, n := range env.Nodes() {
		list, err := n.ListContainers(ctx, false)
		if err != nil {
			continue // one unreachable node does not hide the replicas on the others
		}
		for _, c := range list {
			if matchesAlertTarget(c, a) {
				t.containers = append(t.containers, targetContainer{node: n, id: c.ID, name: c.Name})
			}
		}
	}
	if t.swarmService == "" && len(t.containers) == 0 {
		return nil, fmt.Errorf("no running container matches %s any more — it was removed or redeployed under another name", a.ContainerName)
	}
	return t, nil
}

// matchesAlertTarget is the evaluator's target rule read backwards: a service's containers
// (compose or swarm, within the alert's stack), or a standalone container by its own name.
func matchesAlertTarget(c dockerx.Container, a *store.Alert) bool {
	if c.Service != "" && c.Service == a.ContainerName && (a.Stack == "" || c.Project == a.Stack) {
		return true
	}
	return c.Name == a.ContainerName
}

func (s *Server) runAction(ctx context.Context, act store.MonitorAction, m *store.Monitor, a *store.Alert, t *actionTarget) store.ActionResult {
	res := store.ActionResult{Kind: act.Kind}
	switch act.Kind {
	case store.ActionRestart:
		if t.swarmService != "" {
			if err := t.control.RestartService(ctx, t.swarmService); err != nil {
				res.Detail = "force-updating " + t.swarmService + ": " + err.Error()
				return res
			}
			res.OK, res.Detail = true, "Force-updated "+t.swarmService+" — swarm replaces its tasks under its own update policy."
			return res
		}
		// One at a time, so a multi-replica compose service keeps serving through its restart.
		var done, failed []string
		for _, c := range t.containers {
			if err := c.node.DoAction(ctx, c.id, dockerx.ActionRestart, false); err != nil {
				failed = append(failed, c.name+": "+err.Error())
				continue
			}
			done = append(done, c.name)
		}
		res.OK = len(failed) == 0
		res.Detail = joinOutcome("Restarted", done, failed)

	case store.ActionExec:
		timeout := time.Duration(orDefault(act.TimeoutSecs, store.DefaultExecTimeoutSecs)) * time.Second
		var out strings.Builder
		res.OK = true
		for _, c := range t.containers {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			r, err := c.node.ExecCapture(cctx, c.id, act.Command)
			cancel()
			switch {
			case err != nil:
				res.OK = false
				fmt.Fprintf(&out, "%s: %v\n", c.name, err)
			case r.ExitCode != 0:
				res.OK = false
				fmt.Fprintf(&out, "%s: exit %d\n%s\n", c.name, r.ExitCode, r.Output)
			default:
				fmt.Fprintf(&out, "%s: exit 0\n%s\n", c.name, r.Output)
			}
		}
		if len(t.containers) == 0 {
			res.OK = false
			out.WriteString("no running container to run it in")
		}
		res.Detail = strings.TrimSpace(out.String())

	case store.ActionScale:
		if t.swarmService == "" {
			res.Detail = a.ContainerName + " is not a swarm service — only a swarm service has replicas Daffa can change."
			return res
		}
		cur, replicated, err := t.control.ServiceReplicas(ctx, t.swarmService)
		if err != nil {
			res.Detail = err.Error()
			return res
		}
		if !replicated {
			res.Detail = t.swarmService + " is a global service — it runs one task per node and has no replica count."
			return res
		}
		want, bound := scaleTarget(int(cur), act.By, act.Limit)
		if want == int(cur) {
			res.OK, res.Detail = true, fmt.Sprintf("Already at %d replicas, the %s this action may reach.", cur, bound)
			return res
		}
		if err := t.control.ScaleService(ctx, t.swarmService, uint64(want)); err != nil {
			res.Detail = err.Error()
			return res
		}
		res.OK, res.Detail = true, fmt.Sprintf("Scaled %s from %d to %d replicas.", t.swarmService, cur, want)

	case store.ActionHTTP:
		code, err := s.postAlert(ctx, act, m, a)
		if err != nil {
			res.Detail = err.Error()
			return res
		}
		res.OK = code >= 200 && code < 300
		res.Detail = fmt.Sprintf("POST %s answered %d.", act.URL, code)

	default:
		res.Detail = "unknown action " + act.Kind
	}
	return res
}

// scaleTarget clamps a scale step to its limit: a ceiling scaling out, a floor scaling in.
func scaleTarget(cur, by, limit int) (want int, bound string) {
	want = cur + by
	if by > 0 {
		return min(want, max(limit, cur)), "ceiling"
	}
	return max(want, min(limit, cur)), "floor"
}

func joinOutcome(verb string, done, failed []string) string {
	var parts []string
	if len(done) > 0 {
		parts = append(parts, verb+" "+strings.Join(done, ", ")+".")
	}
	if len(failed) > 0 {
		parts = append(parts, "Failed: "+strings.Join(failed, "; ")+".")
	}
	return strings.Join(parts, " ")
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// alertPayload is what an HTTP action POSTs: enough for a runbook or a ticket to act on without
// calling back into Daffa.
type alertPayload struct {
	Event   string       `json:"event"` // monitor.fired
	Monitor alertMonitor `json:"monitor"`
	Alert   alertBody    `json:"alert"`
}

type alertMonitor struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Metric       string  `json:"metric"`
	Op           string  `json:"op"`
	Threshold    float64 `json:"threshold"`
	DurationSecs int     `json:"duration_secs"`
}

type alertBody struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"`
	Stack     string    `json:"stack,omitempty"`
	ClusterID string    `json:"cluster_id"`
	Cluster   string    `json:"cluster"`
	Value     float64   `json:"value"`
	StartedAt time.Time `json:"started_at"`
}

func (s *Server) postAlert(ctx context.Context, act store.MonitorAction, m *store.Monitor, a *store.Alert) (int, error) {
	body, err := json.Marshal(alertPayload{
		Event:   "monitor.fired",
		Monitor: alertMonitor{ID: m.ID, Name: m.Name, Metric: m.Metric, Op: m.Op, Threshold: m.Threshold, DurationSecs: m.DurationSecs},
		Alert: alertBody{ID: a.ID, Target: a.ContainerName, Stack: a.Stack, ClusterID: a.EnvID,
			Cluster: s.envName(ctx, a.EnvID), Value: a.Value, StartedAt: a.StartedAt},
	})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(orDefault(act.TimeoutSecs, store.DefaultHTTPTimeoutSecs))*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, act.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Daffa")
	if act.SecretEnc != "" {
		secret, err := s.sealer.Open(act.SecretEnc)
		if err != nil {
			return 0, errors.New("the signing secret could not be opened — was the master key replaced?")
		}
		// The GitHub shape, so a receiver written for one forge's webhooks verifies this too.
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Daffa-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("POST %s: %w", act.URL, err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
