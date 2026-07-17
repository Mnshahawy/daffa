package stacks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// Warnings are things that are TRUE about a stack and that nobody asked about, but that somebody
// is going to wish they had been told.
//
// They are not errors: the deploy proceeds. Daffa is not in the business of refusing a file its
// owner meant to write. But it IS in the business of saying the thing out loud rather than letting
// it be discovered from the wrong side.
type Warning struct {
	Service string `json:"service"`
	Text    string `json:"text"`
}

// SwarmWarnings inspects a swarm stack for the ways swarm quietly differs from what a compose file
// looks like it says.
//
// # The volume trap
//
// A NAMED VOLUME IN SWARM IS NODE-LOCAL. It is created on whichever machine the task happened to
// land on. If that task is rescheduled — a node drains, a machine reboots, a rolling update moves
// it — the new task gets a FRESH, EMPTY volume of the same name on the new machine, and the
// database it was serving is simply gone from its point of view. Nothing errors. The service comes
// up healthy, and it is empty.
//
// This is the most expensive Swarm mistake available and it is completely silent. Dokploy's answer
// is to secretly constrain any service with a mount to `node.role==manager`, which works only
// because every Dokploy swarm is single-node and is a fig leaf on any other. Portainer says nothing
// at all.
//
// Daffa already parses the compose file, so it can just SAY SO. Not refuse — say so, at deploy
// time, in the log, and on the stack page. It costs one walk over the parsed services and it will
// one day save somebody's database.
func SwarmWarnings(ctx context.Context, yaml, projectName string, env []EnvVar, nodes int) ([]Warning, error) {
	envMap := map[string]string{}
	for _, v := range env {
		envMap[v.Key] = v.Value
	}

	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: composePath, Content: []byte(yaml)}},
		Environment: envMap,
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		// A file that will not parse is a deploy failure, and the deploy path already says so far
		// more precisely than a warning could. Do not double-report it.
		return nil, nil
	}

	var out []Warning
	for name, svc := range project.Services {
		vols := namedVolumes(svc)
		if len(vols) == 0 {
			continue
		}
		if pinned(svc) {
			continue
		}

		out = append(out, Warning{
			Service: name,
			Text:    volumeTrapText(name, vols, nodes),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// namedVolumes are the mounts that carry data and follow nothing. A bind mount is the operator's
// own problem and they already know where it is; an anonymous volume holds nothing anybody meant
// to keep.
func namedVolumes(svc types.ServiceConfig) []string {
	var out []string
	for _, v := range svc.Volumes {
		if v.Type == string(types.VolumeTypeVolume) && v.Source != "" {
			out = append(out, v.Source)
		}
	}
	sort.Strings(out)
	return out
}

// pinned reports whether this service can only ever land on one machine — in which case its volume
// is always the same volume, and there is nothing to warn about.
//
// A placement constraint is the honest fix, and it is the one the warning recommends. Both `node.`
// constraints (hostname, id, role) and a node label pin a service somewhere; whether they pin it to
// exactly ONE machine is not something we can know without asking the cluster, so any constraint at
// all is taken as "the author has thought about placement", which is all this check is entitled to
// conclude.
func pinned(svc types.ServiceConfig) bool {
	if svc.Deploy == nil {
		return false
	}
	if len(svc.Deploy.Placement.Constraints) > 0 {
		return true
	}
	// A global service runs one task per node and never moves. Its volume on each machine is that
	// machine's, which is a design, not an accident.
	if svc.Deploy.Mode == "global" {
		return true
	}
	return false
}

func volumeTrapText(service string, vols []string, nodes int) string {
	var b strings.Builder

	plural := "the volume"
	if len(vols) > 1 {
		plural = "the volumes"
	}
	fmt.Fprintf(&b, "%s mounts %s %s", service, plural, strings.Join(quoteAll(vols), ", "))

	// The node count is the difference between a warning and a scare. On a single-node swarm there
	// is nowhere else for the task to go, so the trap cannot spring — but it springs the day
	// somebody adds a second machine, which is precisely when nobody will be thinking about it.
	if nodes <= 1 {
		b.WriteString(" and has no placement constraint. This Swarm has one node, so the data " +
			"stays put today — but a named volume is node-local, and the day a second node is " +
			"added this service can be scheduled onto it and will find an empty volume there. " +
			"Constrain it to a node, or use a volume driver that follows it.")
		return b.String()
	}

	fmt.Fprintf(&b, " and can be scheduled onto any of %d nodes. A named volume is node-local: "+
		"if this service moves, it will find a fresh, empty volume on the new machine and will "+
		"NOT find its data. Constrain it to a node, or use a volume driver that follows it.", nodes)
	return b.String()
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "\"" + s + "\""
	}
	return out
}

// WarningLog renders warnings for the head of a deploy log, where they will actually be read.
func WarningLog(ws []Warning) string {
	if len(ws) == 0 {
		return ""
	}
	var b strings.Builder
	for _, w := range ws {
		b.WriteString("warning: ")
		b.WriteString(w.Text)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
