package dockerx

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
)

// Mutations against a Swarm, all of them CLUSTER-WIDE and therefore all of them asked of a manager.
// Env.Control() is the only thing that hands one out.
//
// # Why every one of these re-reads the object first
//
// Swarm is optimistically concurrent: every update carries the Version.Index of the object it is
// updating, and the manager rejects it if that index has moved. This is not ceremony — it is what
// stops two operators, or an operator and a rolling update, from silently overwriting each other.
//
// The consequence is that you cannot update a service you have not just inspected. So each of these
// inspects, mutates the spec it got back, and sends the whole thing. Sending a spec you assembled
// yourself, rather than one the daemon gave you, is how a scale operation quietly drops a service's
// healthcheck, its placement constraints and its update policy — every field you did not think to
// copy. We copy nothing; we edit what swarm handed us.

// ScaleService sets a replicated service's replica count.
func (e *Node) ScaleService(ctx context.Context, id string, replicas uint64) error {
	svc, _, err := e.Client.ServiceInspectWithRaw(ctx, id, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	// A global service runs one task per node. There is no number to set, and pretending there is
	// would produce a service that silently ignores what it was told.
	if svc.Spec.Mode.Replicated == nil {
		return fmt.Errorf("dockerx: %s is a global service — it runs one task per node, so it has no replica count to set", svc.Spec.Name)
	}
	svc.Spec.Mode.Replicated.Replicas = &replicas

	_, err = e.Client.ServiceUpdate(ctx, id, svc.Version, svc.Spec, types.ServiceUpdateOptions{})
	return err
}

// RedeployService forces every task to be recreated, even though nothing in the spec changed.
//
// This is `docker service update --force`, and the mechanism is a counter: swarm reschedules when
// the spec differs, so bumping ForceUpdate makes it differ. It is the honest way to say "pull the
// image again and restart", which on a floating tag like :latest is the only way to get the new
// bytes without editing anything.
//
// QueryRegistry is what actually re-resolves the tag. Without it swarm reuses the digest it already
// pinned, and a "redeploy" of :latest would restart the same image it was already running — which
// looks exactly like it worked.
func (e *Node) RedeployService(ctx context.Context, id string) error {
	svc, _, err := e.Client.ServiceInspectWithRaw(ctx, id, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	svc.Spec.TaskTemplate.ForceUpdate++

	_, err = e.Client.ServiceUpdate(ctx, id, svc.Version, svc.Spec, types.ServiceUpdateOptions{
		QueryRegistry: true,
	})
	return err
}

// RollbackService puts back the service's PREVIOUS spec.
//
// Swarm keeps it — PreviousSpec is a real field — so this is a genuine rollback of the service,
// not a re-apply of an old file. It is the fastest way out of a bad update, and it is why the
// service page offers it next to the deploy that caused the trouble.
//
// It rolls back ONE service. A stack rollback is a different act: it re-applies the whole compose
// file that a past deployment stored, and it lives on the deployment, not here.
func (e *Node) RollbackService(ctx context.Context, id string) error {
	svc, _, err := e.Client.ServiceInspectWithRaw(ctx, id, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}
	if svc.PreviousSpec == nil {
		return fmt.Errorf("dockerx: %s has no previous version to roll back to — it has only ever been deployed once", svc.Spec.Name)
	}

	_, err = e.Client.ServiceUpdate(ctx, id, svc.Version, *svc.PreviousSpec, types.ServiceUpdateOptions{
		Rollback: "previous",
	})
	return err
}

// RemoveService deletes one service. Its volumes are NOT removed: they are node-local, they are on
// whichever machines its tasks ran on, and they are somebody's data.
func (e *Node) RemoveService(ctx context.Context, id string) error {
	return e.Client.ServiceRemove(ctx, id)
}

// ── nodes ───────────────────────────────────────────────────────────────────────

// SetNodeAvailability moves a machine between active, pause and drain.
//
//	active  — schedulable, and running what it was given
//	pause   — keeps what it has, takes nothing new
//	drain   — evicts its tasks, and takes nothing new
//
// A DRAIN EVICTS SWARM TASKS ONLY. A plain container on that machine — including a compose stack
// pinned to it — keeps running, because Swarm does not know it exists. An operator who drains a node
// expecting EVERYTHING to move off it will be wrong, and the UI says so at the moment they do it.
func (e *Node) SetNodeAvailability(ctx context.Context, id string, availability string) error {
	n, _, err := e.Client.NodeInspectWithRaw(ctx, id)
	if err != nil {
		return err
	}

	switch availability {
	case "active", "pause", "drain":
	default:
		return fmt.Errorf("dockerx: %q is not a node availability (active, pause, drain)", availability)
	}
	n.Spec.Availability = swarm.NodeAvailability(availability)

	return e.Client.NodeUpdate(ctx, id, n.Version, n.Spec)
}

// SetNodeRole promotes a worker to a manager, or demotes a manager to a worker.
//
// Promotion is not free: managers hold the raft consensus, and an EVEN number of them cannot break
// a tie. Demoting the wrong one can lose quorum and leave a cluster that runs but cannot be
// changed. Swarm itself refuses the demotion that would leave no managers at all, which is the one
// guard that matters; the rest is a judgement the operator has to make, and the UI shows the counts
// so it can be made.
func (e *Node) SetNodeRole(ctx context.Context, id string, role string) error {
	n, _, err := e.Client.NodeInspectWithRaw(ctx, id)
	if err != nil {
		return err
	}

	switch role {
	case "manager", "worker":
	default:
		return fmt.Errorf("dockerx: %q is not a node role (manager, worker)", role)
	}
	n.Spec.Role = swarm.NodeRole(role)

	return e.Client.NodeUpdate(ctx, id, n.Version, n.Spec)
}

// RemoveNode takes a machine out of the swarm's records.
//
// It does not reach the machine: `docker swarm leave` is what a node runs on ITSELF, and a node
// that is still up will keep believing it is a member. This is for the machine that is already gone
// — burned, reimaged, decommissioned — and is otherwise a `down` row in the node list forever.
func (e *Node) RemoveNode(ctx context.Context, id string, force bool) error {
	return e.Client.NodeRemove(ctx, id, types.NodeRemoveOptions{Force: force})
}
