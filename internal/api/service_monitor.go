package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
)

const serviceMonitorInterval = 60 * time.Second

type svcKey struct {
	envID   string
	stack   string
	svcName string
	svcID   string
}

type svcSight struct {
	running  int
	desired  int
	notified bool
}

func (s *Server) watchServices(ctx context.Context) {
	t := time.NewTicker(serviceMonitorInterval)
	defer t.Stop()

	seen := map[svcKey]*svcSight{}
	first := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		envs, err := s.store.ListEnvironments(ctx)
		if err != nil {
			slog.Error("service monitor: listing environments", "err", err)
			continue
		}

		for _, e := range envs {
			if !e.IsSwarm() {
				continue
			}

			env, err := s.pool.Get(e.ID)
			if err != nil {
				continue
			}

			ctl, err := env.Control()
			if err != nil {
				continue
			}

			svcs, err := ctl.ListServices(ctx)
			if err != nil {
				slog.Warn("service monitor: listing services", "env", e.Name, "err", err)
				continue
			}

			now := map[svcKey]bool{}
			for _, svc := range svcs {
				if svc.Stack == "" {
					continue
				}
				key := svcKey{envID: e.ID, stack: svc.Stack, svcName: svc.Name, svcID: svc.ID}
				now[key] = true

				sight := seen[key]
				if sight == nil {
					sight = &svcSight{}
					seen[key] = sight
				}

				degraded := svc.Desired > 0 && svc.Running < svc.Desired
				wasDegraded := sight.desired > 0 && sight.running < sight.desired

				sight.desired = svc.Desired
				sight.running = svc.Running

				if first {
					continue
				}

				if degraded && !wasDegraded && !sight.notified {
					sight.notified = true

					detail := crashDetail(ctx, ctl, svc)

					s.notify.Send(ctx, e.ID, notify.Data{
						Event:   notify.ServiceDegraded,
						Subject: fmt.Sprintf("Service degraded: %s/%s on %s", svc.Stack, svc.Name, e.Name),
						Title:   fmt.Sprintf("Service degraded: %s/%s", svc.Stack, svc.Name),
						Summary: fmt.Sprintf("Service %q in stack %q has %d/%d replicas running.",
							svc.Name, svc.Stack, svc.Running, svc.Desired),
						HostName: e.Name,
						Target:   svc.Stack + "/" + svc.Name,
						Detail:   detail,
						Link:     "/stacks",
						Failed:   true,
					})
				}

				if !degraded && sight.notified {
					sight.notified = false
				}
			}

			for key := range seen {
				if key.envID == e.ID && !now[key] {
					delete(seen, key)
				}
			}
		}
		first = false
	}
}

func crashDetail(ctx context.Context, node *dockerx.Node, svc dockerx.Service) string {
	tasks, err := node.ListTasks(ctx, svc.ID)
	if err != nil {
		return ""
	}

	var last *dockerx.Task
	for i := range tasks {
		t := &tasks[i]
		if t.State == "rejected" || t.State == "failed" {
			if last == nil || t.Since.After(last.Since) {
				last = t
			}
			continue
		}
		if t.Desired == "shutdown" && t.State == "shutdown" && t.Err != "" {
			if last == nil || t.Since.After(last.Since) {
				last = t
			}
		}
	}

	if last == nil {
		return ""
	}

	var b strings.Builder

	if last.Err != "" {
		b.WriteString("task error: ")
		b.WriteString(last.Err)
	}

	if last.ContainerID != "" {
		exit, ok := containerExitCode(ctx, node, last.ContainerID)
		if ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(fmt.Sprintf("exit code: %d", exit))
		}

		log, ok := containerTailLog(ctx, node, last.ContainerID, "50")
		if ok && log != "" {
			b.WriteString("\n——\nlast lines:\n")
			b.WriteString(log)
		}
	}

	return b.String()
}

func containerExitCode(ctx context.Context, node *dockerx.Node, ctrID string) (int, bool) {
	raw, err := node.InspectContainer(ctx, ctrID)
	if err != nil {
		return 0, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return 0, false
	}
	var s struct {
		State *struct {
			ExitCode int `json:"ExitCode"`
		} `json:"State"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.State == nil {
		return 0, false
	}
	return s.State.ExitCode, true
}

func containerTailLog(ctx context.Context, node *dockerx.Node, ctrID, tail string) (string, bool) {
	var b strings.Builder
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := node.StreamLogs(ctx, ctrID, tail, false, func(line dockerx.LogLine) error {
		if b.Len() < 2048 {
			b.WriteString(line.Text)
			b.WriteString("\n")
		}
		return nil
	})
	if err != nil || b.Len() == 0 {
		return "", false
	}
	s := b.String()
	if len(s) > 2048 {
		s = s[:2048] + "\n…"
	}
	return s, true
}
