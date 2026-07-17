package api

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Mnshahawy/daffa/internal/dockerx"
)

// Containers, images, volumes and bridge networks are NODE-LOCAL: they belong to one daemon, and
// no manager can see another machine's. So on a swarm they are read from every node Daffa can
// reach, in parallel, and rendered as one list with a Node column.
//
// A node PICKER was the other option, and it is the wrong one: "where is that container?" is the
// question you actually have, and a picker makes you already know the answer in order to ask it.
//
// On partial results. A node that is unreachable contributes nothing, and this does NOT fail the
// request — one dead machine must not blank the list of the four that are fine. But it is never
// silently trimmed either: every node's status is already on GET /api/environments, which the
// switcher polls anyway, so the UI names the machine it could not read rather than quietly
// pretending the cluster is smaller than it is. A list that omits a machine without saying so is a
// list that will be trusted and is wrong.

// fanOut reads a node-local resource from every reachable node in the environment and tags each
// item with the machine it came from.
func fanOut[T any](
	ctx context.Context,
	env *dockerx.Env,
	read func(context.Context, *dockerx.Node) ([]T, error),
	tag func(*T, *dockerx.Node),
) []T {
	nodes := env.Nodes()

	// The overwhelmingly common case: one node. Do not spawn a goroutine to talk to it.
	if len(nodes) == 1 {
		items, err := read(ctx, nodes[0])
		if err != nil {
			return nil
		}
		if env.IsSwarm() {
			for i := range items {
				tag(&items[i], nodes[0])
			}
		}
		return items
	}

	var (
		mu  sync.Mutex
		out []T
		wg  sync.WaitGroup
	)
	for _, n := range nodes {
		wg.Add(1)
		go func(n *dockerx.Node) {
			defer wg.Done()

			items, err := read(ctx, n)
			if err != nil {
				// Not fatal. The node is down, or the tunnel died; the other machines still have
				// answers, and the switcher already knows this one is offline.
				slog.Debug("fan-out: a node did not answer", "node", n.Name, "err", err)
				return
			}
			for i := range items {
				tag(&items[i], n)
			}

			mu.Lock()
			out = append(out, items...)
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return out
}

// fanOutErr is fanOut for the single-node case where the caller wants the error — a standalone
// environment has exactly one daemon, and if it is down that IS the answer, not a partial result.
func fanOutErr[T any](
	ctx context.Context,
	env *dockerx.Env,
	read func(context.Context, *dockerx.Node) ([]T, error),
	tag func(*T, *dockerx.Node),
) ([]T, error) {
	nodes := env.Nodes()
	if len(nodes) == 1 {
		items, err := read(ctx, nodes[0])
		if err != nil {
			return nil, err
		}
		if env.IsSwarm() {
			for i := range items {
				tag(&items[i], nodes[0])
			}
		}
		return items, nil
	}
	return fanOut(ctx, env, read, tag), nil
}
