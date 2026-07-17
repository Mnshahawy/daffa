package dockerx

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
)

// Container is the projection the UI actually renders. The raw Docker summary is
// large and noisy; sending it whole would make the list view pay for fields nobody
// reads.
type Container struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	State   string   `json:"state"`  // running | exited | paused | …
	Status  string   `json:"status"` // "Up 3 hours"
	Created int64    `json:"created"`
	Ports   []Port   `json:"ports"`
	Project string   `json:"project"` // compose project, if any
	Service string   `json:"service"` // compose service, if any
	Labels  []string `json:"labels,omitempty"`

	// Which machine this is on. Empty on a standalone environment, where the question does not
	// arise; set when the list fanned out across a swarm's nodes, because "where is that
	// container?" is the question you actually have — and a node PICKER would make you already
	// know the answer in order to ask it.
	Node   string `json:"node,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

type Port struct {
	Private uint16 `json:"private"`
	Public  uint16 `json:"public,omitempty"`
	Type    string `json:"type"`
	IP      string `json:"ip,omitempty"`
}

const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeService = "com.docker.compose.service"
)

func (e *Node) ListContainers(ctx context.Context, all bool) ([]Container, error) {
	list, err := e.Client.ContainerList(ctx, container.ListOptions{All: all})
	if err != nil {
		return nil, fmt.Errorf("dockerx: listing containers on %s: %w", e.Name, err)
	}

	out := make([]Container, 0, len(list))
	for _, c := range list {
		item := Container{
			ID:      c.ID,
			Name:    prettyName(c.Names),
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
			Project: c.Labels[labelComposeProject],
			Service: c.Labels[labelComposeService],
		}
		for _, p := range c.Ports {
			item.Ports = append(item.Ports, Port{
				Private: p.PrivatePort, Public: p.PublicPort, Type: p.Type, IP: p.IP,
			})
		}
		out = append(out, item)
	}

	// Group a compose project's services together, then order by name, so the list
	// reads the way the stack is actually organized rather than by daemon whim.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// prettyName strips the leading slash Docker puts on container names, and prefers
// the first name when a container has several.
func prettyName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func (e *Node) InspectContainer(ctx context.Context, id string) (any, error) {
	info, err := e.Client.ContainerInspect(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("dockerx: inspecting %s: %w", id, err)
	}
	return info, nil
}

// Action is a lifecycle operation. Keeping them in one switch (rather than a handler
// per verb) means the audit log, the permission check and the error shape are written
// once.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionKill    Action = "kill"
	ActionPause   Action = "pause"
	ActionUnpause Action = "unpause"
	ActionRemove  Action = "remove"
)

func ValidAction(a Action) bool {
	switch a {
	case ActionStart, ActionStop, ActionRestart, ActionKill, ActionPause, ActionUnpause, ActionRemove:
		return true
	}
	return false
}

func (e *Node) DoAction(ctx context.Context, id string, a Action, force bool) error {
	var err error
	switch a {
	case ActionStart:
		err = e.Client.ContainerStart(ctx, id, container.StartOptions{})
	case ActionStop:
		err = e.Client.ContainerStop(ctx, id, container.StopOptions{})
	case ActionRestart:
		err = e.Client.ContainerRestart(ctx, id, container.StopOptions{})
	case ActionKill:
		err = e.Client.ContainerKill(ctx, id, "KILL")
	case ActionPause:
		err = e.Client.ContainerPause(ctx, id)
	case ActionUnpause:
		err = e.Client.ContainerUnpause(ctx, id)
	case ActionRemove:
		err = e.Client.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
	default:
		return fmt.Errorf("dockerx: unknown action %q", a)
	}
	if err != nil {
		return fmt.Errorf("dockerx: %s %s: %w", a, id, err)
	}
	return nil
}

// LogLine is one line of container output, tagged with the stream it came from so
// the UI can distinguish stderr without parsing prose.
type LogLine struct {
	Stream string `json:"stream"` // stdout | stderr
	Text   string `json:"text"`
}

// StreamLogs follows a container's logs and calls emit for each line until ctx ends
// or the stream closes.
//
// Docker multiplexes stdout and stderr over one connection unless the container has
// a TTY, in which case the stream is raw. stdcopy handles the former; the latter is
// plain text. Getting this wrong is why log viewers show binary garbage in the first
// eight bytes of every line.
func (e *Node) StreamLogs(ctx context.Context, id string, tail string, follow bool, emit func(LogLine) error) error {
	info, err := e.Client.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("dockerx: inspecting %s for logs: %w", id, err)
	}

	rc, err := e.Client.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
		Timestamps: false,
	})
	if err != nil {
		return fmt.Errorf("dockerx: streaming logs for %s: %w", id, err)
	}
	defer rc.Close()

	// Close the reader when the client goes away, otherwise a followed stream keeps
	// the goroutine (and the daemon connection) alive forever. Cancel on return too, so a
	// stream that ends on its own (a non-followed read, a container that exits) does not leave
	// the watchdog parked on <-ctx.Done() until the request context finally cancels.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()

	if info.Config != nil && info.Config.Tty {
		return scanLines(rc, func(text string) error {
			return emit(LogLine{Stream: "stdout", Text: text})
		})
	}

	stdout, stderr := newLineWriter("stdout", emit), newLineWriter("stderr", emit)
	if _, err := stdcopy.StdCopy(stdout, stderr, rc); err != nil {
		if ctx.Err() != nil || isClosed(err) {
			return nil // the client hung up; not an error
		}
		return fmt.Errorf("dockerx: demultiplexing logs for %s: %w", id, err)
	}
	if err := stdout.flush(); err != nil {
		return err
	}
	return stderr.flush()
}

// Events follows the daemon's event stream. The UI subscribes once per environment
// and invalidates its queries on what arrives, instead of polling lists.
func (e *Node) Events(ctx context.Context, emit func(action, actorID, actorName string) error) error {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("type", "image")
	f.Add("type", "volume")
	f.Add("type", "network")

	msgs, errs := e.Client.Events(ctx, DockerEventOptions(f))
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			if err == nil || ctx.Err() != nil || isClosed(err) || err == io.EOF {
				return nil
			}
			return fmt.Errorf("dockerx: event stream on %s: %w", e.Name, err)
		case m := <-msgs:
			name := m.Actor.Attributes["name"]
			if err := emit(string(m.Action), m.Actor.ID, name); err != nil {
				return err
			}
		}
	}
}

// Info summarizes the daemon for the host card.
type Info struct {
	Name          string `json:"name"`
	ServerVersion string `json:"server_version"`
	Containers    int    `json:"containers"`
	Running       int    `json:"running"`
	Images        int    `json:"images"`
	NCPU          int    `json:"ncpu"`
	MemTotal      int64  `json:"mem_total"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

func (e *Node) Info(ctx context.Context) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	info, err := e.Client.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("dockerx: querying daemon info on %s: %w", e.Name, err)
	}
	return &Info{
		Name:          info.Name,
		ServerVersion: info.ServerVersion,
		Containers:    info.Containers,
		Running:       info.ContainersRunning,
		Images:        info.Images,
		NCPU:          info.NCPU,
		MemTotal:      info.MemTotal,
		OS:            info.OperatingSystem,
		Arch:          info.Architecture,
	}, nil
}

// Ping reports whether the daemon is reachable — the liveness behind an environment's
// online/offline badge.
func (e *Node) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := e.Client.Ping(ctx); err != nil {
		return fmt.Errorf("dockerx: %s is unreachable: %w", e.Name, err)
	}
	return nil
}

func isClosed(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "context canceled") ||
		err == io.EOF || err == io.ErrClosedPipe)
}
