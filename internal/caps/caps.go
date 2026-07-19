// Package caps is Daffa's capability registry: the complete, hand-maintained list of
// things a user may be permitted to do, and where each one's bit lives.
//
// # Why capabilities are namespaced
//
// They used to be one flat 64-bit mask. That is a wall you walk into exactly once, and by
// the time you notice it is already a migration: the mask travels to the browser as a JSON
// number, JavaScript integers are exact only to 2^53, and so the real ceiling was 52 bits —
// of which 33 were spent. Adding resource monitors cost two of the last twenty.
//
// So a capability now lives in a NAMESPACE — a functional area — and each namespace carries
// its own mask. Five areas today; the sixth is one line in Namespaces below and nothing else,
// because a role's masks are stored one row per namespace rather than one column per anything.
// The wall is not moved. It is gone.
//
// # Why a Cap is not an integer
//
// A Cap is a namespace plus exactly one bit. The zero value therefore names no namespace and
// sets no bit, so it can never satisfy a check, and a route that forgets to declare a
// capability fails CLOSED rather than open. That property is the whole reason the type is a
// struct and not an int: an int-shaped Cap makes bit 0 a legal capability and leaves the zero
// value indistinguishable from "nobody configured this" — a bug you find in production, from
// the wrong side of it.
package caps

//go:generate go run gen.go

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"sort"
	"strings"
)

// Namespace is a functional area. Each carries its own independent mask, and each is stored
// as one row in role_caps — so adding an area costs a line here and no migration at all.
//
// The areas mirror how the UI already groups things, because the role editor renders its
// sections from them: an administrator reading the permission matrix should recognise the
// same shape they see in the navigation, not a second taxonomy invented for the database.
type Namespace string

const (
	NSDocker   Namespace = "docker"   // the Docker runtime on a host
	NSDeploy   Namespace = "deploy"   // stacks, and the credentials they deploy with
	NSData     Namespace = "data"     // backups, and where they are written
	NSObserve  Namespace = "observe"  // monitors, alerts, the audit log
	NSAdmin    Namespace = "admin"    // administering Daffa itself
	NSSwarm    Namespace = "swarm"    // services, tasks and nodes on a Swarm cluster
	NSCerts    Namespace = "certs"    // certificate authorities, certificates, encryption keys
	NSKeyrings Namespace = "keyrings" // rotatable application encryption keys and their deliveries
	NSSSHKeys  Namespace = "sshkeys"  // SSH keys Daffa dials out to clusters and nodes with
)

// Area is a namespace as a person reads it. The role editor's section headers.
type Area struct {
	NS          Namespace
	Label       string
	Description string
}

// Namespaces is the registry of areas, in display order. THIS is the line you add.
var Namespaces = []Area{
	{NSDocker, "Docker", "Containers, images, volumes and networks on a host."},
	{NSDeploy, "Deployments", "Stacks, and the registry and git credentials they deploy with."},
	{NSData, "Backups", "Backup jobs, and the object storage they are written to."},
	{NSObserve, "Monitoring", "Resource monitors, their alerts, and the audit log."},
	{NSSwarm, "Swarm", "Services, tasks and nodes on a Swarm cluster."},
	{NSCerts, "Certificates", "Certificate authorities, the certificates they sign, and backup encryption keys."},
	{NSKeyrings, "Keyrings", "Rotatable application encryption keys, versioned so old data stays readable, delivered to hosts in volumes."},
	{NSSSHKeys, "SSH keys", "Keys Daffa uses to reach clusters and nodes over SSH."},
	{NSAdmin, "Administration", "Users, roles, clusters and Daffa's own settings."},
}

type (
	// Cap is one capability: a namespace, and exactly one bit within it.
	Cap struct {
		NS  Namespace
		Bit uint32 // a single bit — 1<<n, never an index
	}

	// Mask is the bits held within ONE namespace. This is what a row of role_caps stores.
	//
	// 32 bits, not 64, on purpose: every user's masks sit in the in-process capability cache,
	// and role_caps declares INTEGER — which is 32-bit and SIGNED on Postgres. MaxBit is the
	// ceiling that keeps a legal mask inside that column; TestCeiling enforces it, and adding
	// a namespace is one line, so no area should ever be near it.
	Mask uint32

	// Set is what a user or a role actually holds: one mask per namespace.
	//
	// Treat it as immutable. Every operation below returns a new Set rather than editing the
	// receiver, because a Set is handed out of the capability cache and shared between
	// concurrent requests — a caller that mutated one in place would be changing what somebody
	// else is allowed to do, in another goroutine, halfway through their request.
	Set map[Namespace]Mask
)

// Bits are APPEND-ONLY WITHIN THEIR NAMESPACE. A bit's meaning is permanent, including after
// the capability that owned it is retired, because grants are stored as integers: renumbering
// a bit silently re-points every existing grant at a different permission. caps_golden.json
// and TestBitsNeverMove exist to make that mistake impossible to commit.
//
// Moving a capability to a DIFFERENT namespace is the same crime as renumbering it, and the
// golden file catches that too.
//
// These are vars rather than consts only because Go has no struct constants. Nothing writes to
// them, and the golden file would notice if anything did.
var (
	// ── docker ──────────────────────────────────────────────────────────────────
	ContainersView = Cap{NSDocker, 1 << 0}
	ContainersEdit = Cap{NSDocker, 1 << 1}
	ContainersExec = Cap{NSDocker, 1 << 2}
	ImagesView     = Cap{NSDocker, 1 << 3}
	ImagesEdit     = Cap{NSDocker, 1 << 4}
	NetworksView   = Cap{NSDocker, 1 << 5}
	NetworksEdit   = Cap{NSDocker, 1 << 6}
	VolumesView    = Cap{NSDocker, 1 << 7}
	VolumesEdit    = Cap{NSDocker, 1 << 8}
	SystemPrune    = Cap{NSDocker, 1 << 9}

	// ── deploy ──────────────────────────────────────────────────────────────────
	StacksView     = Cap{NSDeploy, 1 << 0}
	StacksEdit     = Cap{NSDeploy, 1 << 1}
	RegistriesView = Cap{NSDeploy, 1 << 2}
	RegistriesEdit = Cap{NSDeploy, 1 << 3}
	GitCredsView   = Cap{NSDeploy, 1 << 4}
	GitCredsEdit   = Cap{NSDeploy, 1 << 5}
	VolSourcesView = Cap{NSDeploy, 1 << 6}
	VolSourcesEdit = Cap{NSDeploy, 1 << 7}

	// ── data ────────────────────────────────────────────────────────────────────
	BackupsView     = Cap{NSData, 1 << 0}
	BackupsEdit     = Cap{NSData, 1 << 1}
	BackupsRestore  = Cap{NSData, 1 << 2}
	BackupsDownload = Cap{NSData, 1 << 3}
	StorageView     = Cap{NSData, 1 << 4}
	StorageEdit     = Cap{NSData, 1 << 5}

	// ── observe ─────────────────────────────────────────────────────────────────
	MonitorsView = Cap{NSObserve, 1 << 0}
	MonitorsEdit = Cap{NSObserve, 1 << 1}
	AuditView    = Cap{NSObserve, 1 << 2}

	// ── swarm ───────────────────────────────────────────────────────────────────
	// There is deliberately no nodes.view. A node is what an environment is MADE OF, and looking
	// at what an environment is made of is clusters.view, which already exists. A second capability
	// for the same page would be a taxonomy built for the database rather than for the person.
	ServicesView = Cap{NSSwarm, 1 << 0}
	ServicesEdit = Cap{NSSwarm, 1 << 1}
	NodesEdit    = Cap{NSSwarm, 1 << 2}
	SwarmEdit    = Cap{NSSwarm, 1 << 3}

	// ── certs ───────────────────────────────────────────────────────────────────
	CertsView = Cap{NSCerts, 1 << 0}
	CertsEdit = Cap{NSCerts, 1 << 1}
	KeysView  = Cap{NSCerts, 1 << 2}
	KeysEdit  = Cap{NSCerts, 1 << 3}

	// ── keyrings ────────────────────────────────────────────────────────────────
	KeyringsView = Cap{NSKeyrings, 1 << 0}
	KeyringsEdit = Cap{NSKeyrings, 1 << 1}

	// ── sshkeys ──────────────────────────────────────────────────────────────────
	SSHKeysView = Cap{NSSSHKeys, 1 << 0}
	SSHKeysEdit = Cap{NSSSHKeys, 1 << 1}

	// ── admin ───────────────────────────────────────────────────────────────────
	UsersView    = Cap{NSAdmin, 1 << 0}
	UsersEdit    = Cap{NSAdmin, 1 << 1}
	RolesView    = Cap{NSAdmin, 1 << 2}
	RolesEdit    = Cap{NSAdmin, 1 << 3}
	SettingsView = Cap{NSAdmin, 1 << 4}
	SettingsEdit = Cap{NSAdmin, 1 << 5}
	ClustersView = Cap{NSAdmin, 1 << 6}
	ClustersEdit = Cap{NSAdmin, 1 << 7}
	LoggingView  = Cap{NSAdmin, 1 << 8}
	LoggingEdit  = Cap{NSAdmin, 1 << 9}
)

// MaxBit is the highest bit a capability may occupy WITHIN a namespace.
//
// The binding constraint is the DATABASE, now that masks are deliberately small: role_caps
// declares the mask column INTEGER, which on Postgres is 32-bit and SIGNED. Bit 31 is the
// sign bit, so the highest bit a mask can carry and still round-trip through that column is
// 30. That is not hypothetical: when capabilities were one flat mask in an INTEGER column,
// the registry reached bit 31 and Postgres — only Postgres — refused the grant with "integer
// out of range", in production, the moment an administrator ticked the wrong box.
// TestAMaskColumnHoldsAHighBitOnPostgres pins the column against this ceiling.
//
// A WARNING that outlives the width change: JavaScript's BITWISE operators are 32-bit
// SIGNED, whatever a number's precision. `x & y` coerces both sides to int32 first, so a
// capability AT bit 31 would evaluate wrongly in a naive check even though JSON carries the
// value exactly. web/src/lib/caps.ts uses BigInt for exactly this reason; MaxBit = 30 keeps
// every legal mask on the safe side of both traps, and an area that ever fills 31 bits
// should become two areas — that is what areas are for.
const MaxBit = 30

// Mode is which half of an object's view/edit pair a capability is. Standalone capabilities
// (exec, prune, restore, download) have no mode: they are not "more edit", they are their own
// thing.
type Mode string

const (
	ModeView       Mode = "view"
	ModeEdit       Mode = "edit"
	ModeStandalone Mode = ""
)

// Scope is the finest granularity at which a capability means anything.
//
// ScopeGlobal capabilities administer Daffa itself, and Daffa is not per-cluster: there is no
// coherent reading of "may edit users, on staging". A grant of such a role at env scope is
// refused rather than silently widened or silently narrowed.
//
// That refusal is what keeps the admin short-circuit in EffectiveMask honest. An is_admin role
// holds every capability, including the global-only ones, so it can only ever be granted
// globally — which means "Admin on staging" cannot be expressed, and therefore cannot quietly
// become admin of the whole fleet.
//
// Scope is a property of the CAPABILITY, not of its namespace: clusters.view is env-scopable and
// clusters.edit is not, and they live in the same area.
type Scope string

const (
	ScopeGlobal Scope = "global" // fleet-wide only; cannot be granted on one cluster
	ScopeEnv    Scope = "env"    // may be granted globally, or on one cluster
)

// Def is one row of the registry. It is what the UI renders its permission matrix from, so
// Description is user-facing copy, not a code comment.
type Def struct {
	Cap         Cap
	Name        string // stable wire name, e.g. "containers.exec"
	Object      string // the row within an area, in the UI
	Mode        Mode
	Scope       Scope
	Description string
}

// NS is the area the capability belongs to.
func (d Def) NS() Namespace { return d.Cap.NS }

// Bit is the capability's position within its namespace, for display and the golden file.
func (d Def) Bit() int { return bits.TrailingZeros32(d.Cap.Bit) }

// All is the registry, grouped by area and then in display order within it.
var All = []Def{
	// ── docker ──────────────────────────────────────────────────────────────────
	{ContainersView, "containers.view", "containers", ModeView, ScopeEnv, "See containers, their logs and their stats."},
	{ContainersEdit, "containers.edit", "containers", ModeEdit, ScopeEnv, "Start, stop, restart, kill and remove containers."},
	{ContainersExec, "containers.exec", "containers", ModeStandalone, ScopeEnv, "Open a shell inside a container. The Docker socket runs as root, so this is effectively root on the host."},

	{ImagesView, "images.view", "images", ModeView, ScopeEnv, "See images."},
	{ImagesEdit, "images.edit", "images", ModeEdit, ScopeEnv, "Remove images."},

	{NetworksView, "networks.view", "networks", ModeView, ScopeEnv, "See networks."},
	{NetworksEdit, "networks.edit", "networks", ModeEdit, ScopeEnv, "Remove networks."},

	{VolumesView, "volumes.view", "volumes", ModeView, ScopeEnv, "See volumes."},
	{VolumesEdit, "volumes.edit", "volumes", ModeEdit, ScopeEnv, "Remove volumes. Removing a volume destroys its data."},

	{SystemPrune, "system.prune", "system", ModeStandalone, ScopeEnv, "Bulk-delete every unused image, volume and network on a host. Irreversible and host-wide."},

	// ── deploy ──────────────────────────────────────────────────────────────────
	{StacksView, "stacks.view", "stacks", ModeView, ScopeEnv, "See stacks, their services and their deployment history."},
	{StacksEdit, "stacks.edit", "stacks", ModeEdit, ScopeEnv, "Create, deploy, cancel, roll back and remove stacks; set their environment variables, secrets and auto-deploy."},

	// The credential stores have no environment: there is one list of registries, one of git
	// credentials, one of S3 targets, shared by the whole fleet.
	//
	// Their VIEW is nonetheless env-grantable, because an operator scoped to one cluster still has
	// to pick a git credential when they create a stack there. A grant at any scope shows the
	// whole list — which is safe, because the list carries names and kinds and never a secret.
	// Their EDIT is global-only: those rows hold the secrets.
	{RegistriesView, "registries.view", "registries", ModeView, ScopeEnv, "See configured registries — never their passwords. The list is shared by every cluster."},
	{RegistriesEdit, "registries.edit", "registries", ModeEdit, ScopeGlobal, "Add and remove registries, including their passwords. Fleet-wide."},

	{GitCredsView, "gitcreds.view", "gitcreds", ModeView, ScopeEnv, "See which git credentials exist, by name — never their tokens or keys. The list is shared by every cluster."},
	{GitCredsEdit, "gitcreds.edit", "gitcreds", ModeEdit, ScopeGlobal, "Add and remove git credentials, including tokens and SSH keys. Fleet-wide."},

	// Volume sources deliver PUBLIC repo material to a cluster's named volumes, so unlike the
	// credential stores both halves are env-grantable: delivering config to a cluster you
	// operate is a cluster-scoped power, the stacks.edit precedent. The git credential a
	// source uses stays behind gitcreds.*.
	{VolSourcesView, "volsources.view", "volsources", ModeView, ScopeEnv, "See volume sources — repository, ref, subtree, live commit and sync status."},
	{VolSourcesEdit, "volsources.edit", "volsources", ModeEdit, ScopeEnv, "Create, edit, sync and delete volume sources. A sync overwrites the volume's Daffa-delivered files with the repository's."},

	// ── data ────────────────────────────────────────────────────────────────────
	{BackupsView, "backups.view", "backups", ModeView, ScopeEnv, "See backup jobs and whether they are succeeding."},
	{BackupsEdit, "backups.edit", "backups", ModeEdit, ScopeEnv, "Create, edit, enable and run backup jobs."},
	{BackupsRestore, "backups.restore", "backups", ModeStandalone, ScopeEnv, "Restore a snapshot, overwriting a live database."},
	{BackupsDownload, "backups.download", "backups", ModeStandalone, ScopeEnv, "Download a snapshot. It is an encrypted dump of an entire database."},

	{StorageView, "storage.view", "storage", ModeView, ScopeEnv, "See S3/R2 targets by name — never their secret keys. The list is shared by every cluster."},
	{StorageEdit, "storage.edit", "storage", ModeEdit, ScopeGlobal, "Add, edit and remove S3/R2 targets, including their credentials. Fleet-wide."},

	// ── observe ─────────────────────────────────────────────────────────────────
	//
	// Resource monitors are deliberately NOT part of settings.*, which was the cheaper option.
	//
	// Adding a CPU alert should not require the power to add an identity provider, and that is
	// exactly what reusing settings.edit would have meant. And settings.* is ScopeGlobal, so
	// monitors would have become the one object in Daffa that cannot be scoped to a cluster — when
	// "the SRE who runs staging manages staging's alerts" is the obvious case.
	//
	// From which one rule follows, and it is enforced in the store: a monitor with no cluster
	// filter watches the whole fleet, so creating one requires monitors.edit GLOBALLY. A
	// cluster-scoped holder may only create monitors pinned to a cluster they hold it on.
	{MonitorsView, "monitors.view", "monitors", ModeView, ScopeEnv, "See resource monitors and the alerts they have raised."},
	{MonitorsEdit, "monitors.edit", "monitors", ModeEdit, ScopeEnv, "Create and edit resource monitors, and change how long samples are kept. A monitor that watches every cluster requires this on every cluster."},

	// Env-grantable: an operator scoped to staging sees staging's history. Entries with no
	// environment — user, role and settings changes — are visible only to a GLOBAL holder.
	{AuditView, "audit.view", "audit", ModeView, ScopeEnv, "Read the audit log — every privileged action anyone took, and every one they were refused."},

	// ── swarm ───────────────────────────────────────────────────────────────────
	//
	// A swarm STACK is still a stack: it deploys with stacks.edit, not with a swarm capability.
	// Two capabilities to deploy one thing would be a taxonomy invented for the database.
	{ServicesView, "services.view", "services", ModeView, ScopeEnv, "See a Swarm cluster's services, their tasks and their logs. The task is where a service says why it is not running."},
	{ServicesEdit, "services.edit", "services", ModeEdit, ScopeEnv, "Scale, redeploy, roll back and remove Swarm services. On a Swarm this is also read access to every secret: you can mount any of them into a service you control and read it out of the container."},

	// Node operations are their own bit. Draining a machine moves EVERYBODY'S workload; scaling one
	// service moves one. They are not the same authority, and an operator trusted with the second has
	// not thereby been trusted with the first.
	//
	// It is STANDALONE rather than the edit half of a view/edit pair, and that is not a technicality:
	// there is no nodes.view, because a node is what an environment is MADE OF, and looking at what
	// an environment is made of is clusters.view. Inventing a second capability to read the same page
	// would be a taxonomy built for the database rather than for the person using it. (The golden
	// test catches this either way — an edit capability with no matching view is refused.)
	{NodesEdit, "nodes.edit", "nodes", ModeStandalone, ScopeEnv, "Drain, pause, promote and demote the machines in a Swarm, and remove them from it. Draining a node evicts every Swarm task on it. Reading the node list is clusters.view."},

	// There are no swarm secret or config capabilities. A secret is a stack's sealed
	// sub-resource, governed by stacks.view/stacks.edit and delivered on every engine
	// (docs/secrets.md); config lives in git behind a volume source (docs/volumes.md).

	// swarm.edit is the cluster's own existence: creating one, letting machines in, dissolving it.
	// The join tokens are the thing to guard — anybody holding one can add a machine to the cluster,
	// so they are a credential, and they are never returned to anyone without this bit.
	{SwarmEdit, "swarm.edit", "swarm", ModeStandalone, ScopeEnv, "Create a Swarm on a host, read its join tokens, and leave it. A join token lets anybody who has it add a machine to the cluster, so it is a credential and this is the capability that reads one."},

	// ── certs ───────────────────────────────────────────────────────────────────
	//
	// The same split as the other credential stores (registries, git credentials, storage):
	// there is ONE list of CAs and certificates, shared by the fleet. VIEW is env-grantable
	// because an operator scoped to one host still has to pick a certificate when they set up
	// a delivery there — and the list carries names, SANs and expiry dates, never a key.
	// EDIT is global-only, and deserves its warning: a Daffa-held CA signs certificates the
	// whole fleet trusts, so certs.edit is the power to mint one for any name.
	{CertsView, "certs.view", "certs", ModeView, ScopeEnv, "See certificate authorities, certificates and their deliveries — names, SANs, expiry. Never a private key."},
	{CertsEdit, "certs.edit", "certs", ModeEdit, ScopeGlobal, "Create and upload CAs and certificates, rotate them, and deliver them to hosts. A CA Daffa holds signs certificates the whole fleet trusts — this is the power to mint one for any name."},

	{KeysView, "keys.view", "keys", ModeView, ScopeEnv, "See backup encryption keys: their names and public halves. The private halves do not exist on the server."},
	{KeysEdit, "keys.edit", "keys", ModeEdit, ScopeGlobal, "Generate and import backup encryption keys. The private half is downloaded once at generation and never stored."},

	// ── keyrings ────────────────────────────────────────────────────────────────
	//
	// The credential-store split again: ONE list of keyrings, shared by the fleet. VIEW is
	// env-grantable because an operator scoped to one host has to pick a keyring when they set
	// up a delivery there — and the list carries names, version ids and states, never material.
	// EDIT is global-only: rotating or retiring a version changes what every consumer on every
	// host can decrypt, so there is no narrower grant that means anything.
	{KeyringsView, "keyrings.view", "keyrings", ModeView, ScopeEnv, "See keyrings, their version timeline and their deliveries — names, states, ages, sync status. Never key material."},
	{KeyringsEdit, "keyrings.edit", "keyrings", ModeEdit, ScopeGlobal, "Create, rotate and retire keyrings, and deliver them to hosts. Retiring a version makes data encrypted under it unreadable to every consumer."},

	// The SSH-key store is the credential-store pattern (registries, git credentials): its VIEW
	// is env-grantable and secret-free — an operator scoped to one cluster still has to pick a key
	// when they add a node to it — and its EDIT is global, because generating or importing a key,
	// and holding the sealed private half, is a fleet-wide power.
	{SSHKeysView, "sshkeys.view", "sshkeys", ModeView, ScopeEnv, "See SSH keys by name and fingerprint, and copy their PUBLIC half. Never the private key."},
	{SSHKeysEdit, "sshkeys.edit", "sshkeys", ModeEdit, ScopeGlobal, "Generate, import and remove SSH keys. The private half is sealed and used to dial out to clusters and nodes — so this is fleet-wide."},

	// ── admin ───────────────────────────────────────────────────────────────────
	{UsersView, "users.view", "users", ModeView, ScopeGlobal, "See the list of users and the roles they hold."},
	{UsersEdit, "users.edit", "users", ModeEdit, ScopeGlobal, "Create, disable and delete users, reset passwords, and grant roles."},

	{RolesView, "roles.view", "roles", ModeView, ScopeGlobal, "See roles and the capabilities they carry."},
	{RolesEdit, "roles.edit", "roles", ModeEdit, ScopeGlobal, "Create and edit roles. Anyone who can edit a role can grant themselves anything in it — this is an administrative power, not a lesser one."},

	{SettingsView, "settings.view", "settings", ModeView, ScopeGlobal, "See identity provider and notification settings. Never secrets."},
	{SettingsEdit, "settings.edit", "settings", ModeEdit, ScopeGlobal, "Add, edit and remove identity providers, role mappings and notification rules."},

	{ClustersView, "clusters.view", "clusters", ModeView, ScopeEnv, "See a cluster and its disk usage. Without this, a cluster is invisible — it does not appear in the switcher at all."},
	// Enrolling a cluster is how it comes to EXIST, so it cannot be scoped to one.
	{ClustersEdit, "clusters.edit", "clusters", ModeEdit, ScopeGlobal, "Rename clusters, and enroll or revoke agents. An agent brings a new machine Daffa can reach — so this is fleet-wide."},

	// The monitors argument again: NOT settings.* and NOT clusters.edit, because both are
	// ScopeGlobal and "the SRE who runs staging sets staging's log rotation" is the obvious
	// case. ScopeEnv here; the FLEET default is still guarded globally, by the route's
	// scope rather than the capability's — see /api/settings/logging in api/server.go.
	{LoggingView, "logging.view", "logging", ModeView, ScopeEnv, "See the default container log driver and rotation for deploys — the fleet default and any cluster override."},
	{LoggingEdit, "logging.edit", "logging", ModeEdit, ScopeEnv, "Set a cluster's container log defaults, applied to services at their next deploy. Changing the fleet-wide default requires this globally."},
}

// Lookups, built once; the registry is immutable after init.
var (
	byName = map[string]Cap{}
	byCap  = map[Cap]Def{}
	areaOf = map[Namespace]Area{}

	// Everything is the union of every capability. An admin role resolves to THIS at runtime
	// rather than to a stored all-ones set — otherwise the day we add a capability, every
	// existing admin would silently not have it.
	Everything Set

	// EnvScopable is every capability that MAY be granted on a single cluster. A scoped grant is
	// masked to this on the way in, belt and braces: even if a caller slipped a global-only bit
	// past the check, it could not land in an environment's mask.
	EnvScopable Set

	// viewOf maps each object to its view capability, so Normalize can enforce edit ⇒ view.
	viewOf = map[string]Cap{}
)

func init() {
	Everything = Set{}
	EnvScopable = Set{}

	for _, a := range Namespaces {
		areaOf[a.NS] = a
	}
	for _, d := range All {
		byName[d.Name] = d.Cap
		byCap[d.Cap] = d
		Everything[d.Cap.NS] |= Mask(d.Cap.Bit)
		if d.Scope == ScopeEnv {
			EnvScopable[d.Cap.NS] |= Mask(d.Cap.Bit)
		}
		if d.Mode == ModeView {
			viewOf[d.Object] = d.Cap
		}
	}
}

// Of builds a set from capabilities. Normalized, so `Of(StacksEdit)` carries stacks.view too —
// exactly as a grant made through the API would.
func Of(cs ...Cap) Set {
	out := Set{}
	for _, c := range cs {
		if !c.IsZero() {
			out[c.NS] |= Mask(c.Bit)
		}
	}
	return Normalize(out)
}

// IsZero reports whether a capability is the zero value — the one that must never match.
func (c Cap) IsZero() bool { return c.NS == "" || c.Bit == 0 }

// Name is the capability's wire name, or "" if it is not in the registry.
func (c Cap) Name() string {
	if d, ok := byCap[c]; ok {
		return d.Name
	}
	return ""
}

func (c Cap) String() string {
	if n := c.Name(); n != "" {
		return n
	}
	return fmt.Sprintf("cap(%s#%#x)", c.NS, c.Bit)
}

// AreaOf returns the area a namespace names, and whether it is one we know.
func AreaOf(ns Namespace) (Area, bool) {
	a, ok := areaOf[ns]
	return a, ok
}

// ── Set ─────────────────────────────────────────────────────────────────────────

// Has reports whether the set carries the capability.
//
// The IsZero guard is what makes an undeclared capability fail closed instead of matching
// everything: a route that names no capability names the zero Cap, and the zero Cap is in
// nobody's set.
func (s Set) Has(c Cap) bool {
	if c.IsZero() {
		return false
	}
	return s[c.NS]&Mask(c.Bit) != 0
}

// IsZero reports whether the set carries nothing at all, in any namespace.
func (s Set) IsZero() bool {
	for _, m := range s {
		if m != 0 {
			return false
		}
	}
	return true
}

// Equal compares two sets, treating a missing namespace and a zero one as the same thing —
// because they are, and a Set that round-trips through the database comes back without its
// empty rows.
func (s Set) Equal(o Set) bool {
	for _, a := range Namespaces {
		if s[a.NS] != o[a.NS] {
			return false
		}
	}
	return true
}

// Or returns the union. Neither operand is modified.
func (s Set) Or(o Set) Set {
	out := make(Set, len(Namespaces))
	for ns, m := range s {
		out[ns] |= m
	}
	for ns, m := range o {
		out[ns] |= m
	}
	return out
}

// And returns the intersection. Neither operand is modified.
func (s Set) And(o Set) Set {
	out := make(Set, len(Namespaces))
	for ns, m := range s {
		if x := m & o[ns]; x != 0 {
			out[ns] = x
		}
	}
	return out
}

// With returns a copy carrying one more capability.
func (s Set) With(c Cap) Set {
	out := s.Clone()
	if !c.IsZero() {
		out[c.NS] |= Mask(c.Bit)
	}
	return out
}

func (s Set) Clone() Set {
	out := make(Set, len(s))
	for ns, m := range s {
		out[ns] = m
	}
	return out
}

// MarshalJSON emits {"docker": 507, "deploy": 23, …} — the shape web/src/lib/caps.ts expects.
//
// Two things it guarantees that the default map encoding would not. An empty or nil Set becomes
// `{}` rather than `null`, so the browser can index it without a guard at every call site. And
// an area holding nothing is omitted rather than sent as a zero, because a missing area and an
// empty one mean the same thing and sending both spellings invites somebody to tell them apart.
func (s Set) MarshalJSON() ([]byte, error) {
	out := make(map[string]uint32, len(s))
	for ns, m := range s {
		if m != 0 {
			out[string(ns)] = uint32(m)
		}
	}
	return json.Marshal(out)
}

// Names lists the capability names in the set, in registry order. For audit entries and the
// API, where a bare integer would be unreadable.
func (s Set) Names() []string {
	var out []string
	for _, d := range All {
		if s.Has(d.Cap) {
			out = append(out, d.Name)
		}
	}
	return out
}

// ByName resolves a wire name. Unknown names are rejected rather than ignored: silently
// dropping a capability an admin asked for would produce a role that does less than the screen
// says it does.
func ByName(name string) (Cap, bool) {
	c, ok := byName[name]
	return c, ok
}

// SetFromNames builds a set from wire names, normalised. Any unknown name is an error.
func SetFromNames(names []string) (Set, error) {
	out := Set{}
	var unknown []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		c, ok := ByName(n)
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		out[c.NS] |= Mask(c.Bit)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown capabilities: %s", strings.Join(unknown, ", "))
	}
	return Normalize(out), nil
}

// Normalize applies "edit implies view", and drops any bit that is not in the registry —
// including every bit of a namespace we do not know, which is how a row written by a NEWER
// Daffa fails safe when read by an older one: an unrecognised area grants nothing rather than
// granting whatever happens to share its bit positions.
//
// The implication is materialised HERE, at grant time, rather than being evaluated at check
// time. Two reasons: a check stays a single bit test, and — more importantly — the stored set
// is then the whole truth, so the role editor shows an operator exactly what they hold instead
// of a half-truth that the checker quietly widens.
func Normalize(s Set) Set {
	out := make(Set, len(Namespaces))
	for ns, m := range s {
		if v := m & Everything[ns]; v != 0 {
			out[ns] = v
		}
	}
	for _, d := range All {
		if d.Mode == ModeEdit && out.Has(d.Cap) {
			if v, ok := viewOf[d.Object]; ok {
				out[v.NS] |= Mask(v.Bit)
			}
		}
	}
	return out
}

// GlobalOnly returns the global-only capabilities within a set, so an error message can name
// them instead of merely refusing.
func GlobalOnly(s Set) Set {
	out := make(Set, len(Namespaces))
	for _, d := range All {
		if d.Scope == ScopeGlobal && s.Has(d.Cap) {
			out[d.Cap.NS] |= Mask(d.Cap.Bit)
		}
	}
	return out
}

// IsGlobalOnly reports whether the set carries any capability that cannot be scoped to a single
// cluster. Granting such a role on one cluster is refused — see Scope.
func IsGlobalOnly(s Set) bool { return !GlobalOnly(s).IsZero() }
