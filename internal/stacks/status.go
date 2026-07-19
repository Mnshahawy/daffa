package stacks

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Status is what a stack is actually doing right now, as opposed to what it was told to
// do. The two drift, and the whole point of showing it is to make the drift visible.
type Status struct {
	State    string          `json:"state"` // running | partial | stopped | not_deployed
	Services []ServiceStatus `json:"services"`
	Changed  bool            `json:"changed"` // the source has changed since the last deploy
}

type ServiceStatus struct {
	Name string `json:"name"`
	// Declared is the image the compose file asks for; Running is what is actually up.
	// They differ when someone deployed, then edited the file — or when a tag moved
	// under a running container.
	Declared string `json:"declared"`
	Running  string `json:"running,omitempty"`
	State    string `json:"state"` // running | exited | missing | …
	// Status is Docker's own `docker ps` line for the backing container — "Up 3 hours (healthy)" —
	// carrying the uptime and healthcheck the UI shows. Compose only: a swarm service is replicas,
	// not a single container, so it has no such line and leaves this empty.
	Status      string `json:"status,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
}

// Describe asks the ENGINE what is running, because the two engines cannot find out the same way:
// compose asks a daemon for containers, swarm asks a manager for services. This function is just
// the dispatch; the knowledge lives in engine.go, which is where it belongs.
func Describe(ctx context.Context, node *dockerx.Node, eng Engine, project string, declared []Service, deployedHash, currentHash string) (*Status, error) {
	return eng.Describe(ctx, node, project, declared, deployedHash, currentHash)
}

// describeContainers is how COMPOSE finds out what is running: it lists the containers on the
// daemon carrying the project's label.
//
// Deliberately NOT attempted: reproducing compose's own config-hash to decide whether a container
// is up to date. That hash is an internal detail of compose, and guessing at it would produce
// confident, wrong answers. Instead we compare the two things we can know for certain — the set of
// services and the images they run — and separately track whether OUR bundle changed since the last
// successful deploy. That is less clever and more honest.
func describeContainers(ctx context.Context, node *dockerx.Node, eng Engine, project string, declared []Service, deployedHash, currentHash string) (*Status, error) {
	f := filters.NewArgs()
	f.Add("label", eng.ProjectLabel()+"="+project)

	list, err := node.Client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}

	running := map[string]types.Container{}
	for _, c := range list {
		if svc := c.Labels[eng.ServiceLabel()]; svc != "" {
			running[svc] = c
		}
	}
	// A hook mid-run carries the project's labels too (`compose run` stamps them), and
	// must not make a not-yet-deployed stack read as deployed-and-stopped.
	for _, d := range declared {
		if d.Hook {
			delete(running, d.Name)
		}
	}

	st := &Status{
		Services: make([]ServiceStatus, 0, len(declared)),
		Changed:  deployedHash != "" && currentHash != "" && deployedHash != currentHash,
	}

	// Hooks are declared, never deployed: they get their own state and stay out of the
	// up/total arithmetic — a hook counted as "missing" reads a healthy stack as partial.
	up, total := 0, 0
	for _, d := range declared {
		if d.Hook {
			st.Services = append(st.Services, ServiceStatus{Name: d.Name, Declared: d.Image, State: "hook"})
			continue
		}
		total++
		s := ServiceStatus{Name: d.Name, Declared: d.Image, State: "missing"}
		if c, ok := running[d.Name]; ok {
			s.State = c.State
			s.Running = c.Image
			s.Status = c.Status // "Up 3 hours (healthy)" — uptime + health for the UI
			s.ContainerID = c.ID
			if c.State == "running" {
				up++
			}
		}
		st.Services = append(st.Services, s)
	}

	switch {
	case len(running) == 0:
		st.State = "not_deployed"
	case up == 0:
		st.State = "stopped"
	case up == total:
		st.State = "running"
	default:
		st.State = "partial"
	}
	return st, nil
}

// SameImage compares an image reference loosely: compose may say `nginx` where the
// daemon reports `nginx:latest`, and calling that a drift would cry wolf on every stack.
func SameImage(declared, running string) bool {
	norm := func(s string) string {
		if !strings.Contains(s, ":") || strings.HasSuffix(s, "/") {
			return s + ":latest"
		}
		return s
	}
	return norm(declared) == norm(running)
}
