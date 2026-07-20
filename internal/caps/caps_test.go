package caps

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite caps_golden.json")

const goldenPath = "caps_golden.json"

// goldenCap is what the golden file pins for each capability: the area it lives in, its bit
// within that area, and whether it may be granted on a single host.
type goldenCap struct {
	NS    string `json:"ns"`
	Bit   int    `json:"bit"`
	Scope string `json:"scope"`
}

// TestBitsNeverMove is the load-bearing test in this package.
//
// Role grants are stored as integers, one per namespace. If a capability's bit changes, every
// grant that was ever saved silently re-points at whatever capability now owns that bit — a
// viewer could end up holding system.prune, and nothing at runtime would notice.
//
// The NAMESPACE is pinned for exactly the same reason, and this is new: moving a capability
// from one area to another is not a refactor, it is a permissions change. The bits are only
// unique WITHIN an area, so `stacks.view` (deploy, bit 0) and `backups.view` (data, bit 0) are
// the same number — and a capability that changed area would land on whatever already occupies
// its bit there. That is the same catastrophe as renumbering, dressed up as tidying.
//
// Adding a capability is fine and this test passes with a log. Moving or reusing one fails.
//
// If a capability is retired, leave its row in the golden file. Bits are append-only within
// their area, forever.
func TestBitsNeverMove(t *testing.T) {
	current := map[string]goldenCap{}
	for _, d := range All {
		current[d.Name] = goldenCap{NS: string(d.NS()), Bit: d.Bit(), Scope: string(d.Scope)}
	}

	if *update {
		b, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden rewritten — review the diff: a MOVED bit, a MOVED namespace or a CHANGED " +
			"scope is a permissions change; a NEW row is fine")
		return
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenPath, err)
	}
	var golden map[string]goldenCap
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}

	for name, want := range golden {
		got, ok := current[name]
		if !ok {
			// Retiring a capability is allowed; reusing its bit is not, which the duplicate
			// check below catches.
			continue
		}
		if got.NS != want.NS {
			t.Errorf("capability %q MOVED from area %q to area %q.\n"+
				"Bits are only unique within an area, so it has just landed on top of whatever "+
				"already holds bit %d of %q. Every role ever granted %q now means something else. "+
				"Put it back.", name, want.NS, got.NS, got.Bit, got.NS, name)
		}
		if got.Bit != want.Bit {
			t.Errorf("capability %q MOVED from bit %d to bit %d (area %q).\n"+
				"Every role ever granted %q now silently means something else. Put the bit back.",
				name, want.Bit, got.Bit, got.NS, name)
		}
		if got.Scope != want.Scope {
			t.Errorf("capability %q changed scope from %q to %q.\n"+
				"That changes WHERE a role carrying it may be granted. If it is deliberate, "+
				"run -update and say so in the commit message.", name, want.Scope, got.Scope)
		}
	}

	for name, c := range current {
		if _, ok := golden[name]; !ok {
			t.Logf("new capability %q at %s bit %d (%s) — run `go test ./internal/caps -update`",
				name, c.NS, c.Bit, c.Scope)
			t.Fail()
		}
	}
}

// Every capability must live in an area the registry knows about. A Def naming an unregistered
// namespace would be stored (the store writes whatever namespace the Set carries) and then
// dropped on the way back in by Normalize — a capability that saves and does not load, which is
// the worst of both.
func TestEveryCapabilityIsInAKnownArea(t *testing.T) {
	for _, d := range All {
		if _, ok := AreaOf(d.NS()); !ok {
			t.Errorf("%s is in area %q, which is not in Namespaces. Add it there — the role "+
				"editor renders its sections from that list, so a capability outside it would "+
				"be invisible AND unstorable.", d.Name, d.NS())
		}
	}
}

// A global-only capability must never be reachable through a scoped grant. EnvScopable is what
// a scoped set is filtered through, so this is the belt to the store's braces.
func TestGlobalOnlyCapsAreNotEnvScopable(t *testing.T) {
	for _, d := range All {
		if d.Scope == ScopeGlobal && EnvScopable.Has(d.Cap) {
			t.Errorf("%s is global-only but appears in EnvScopable — a grant on one host "+
				"could carry it", d.Name)
		}
	}

	// And the administrative objects must be global-only, full stop. Daffa is not per-cluster, so
	// "may edit users, on staging" has no meaning to be got right.
	for _, name := range []string{
		"users.view", "users.edit", "roles.view", "roles.edit",
		"settings.view", "settings.edit", "clusters.edit", "clusters.provision",
	} {
		c, ok := ByName(name)
		if !ok {
			t.Fatalf("%s is not in the registry", name)
		}
		if byCap[c].Scope != ScopeGlobal {
			t.Errorf("%s is not global-only. A role carrying it could be granted on one host — "+
				"and if that role is an admin role, EffectiveMask would resolve it to admin of "+
				"the whole fleet.", name)
		}
	}
}

// Bits must be unique WITHIN an area, and a Cap must be exactly one bit. Across areas the same
// number is expected and fine — that is what the namespace buys.
func TestNoDuplicateBitsOrNames(t *testing.T) {
	seenBit := map[Cap]string{}
	seenName := map[string]bool{}
	for _, d := range All {
		if bits.OnesCount32(d.Cap.Bit) != 1 {
			t.Errorf("%s is not a single bit (%#x) — a Cap must be exactly one bit",
				d.Name, d.Cap.Bit)
		}
		if other, dup := seenBit[d.Cap]; dup {
			t.Errorf("%s and %s share bit %d of area %q", d.Name, other, d.Bit(), d.NS())
		}
		seenBit[d.Cap] = d.Name

		if seenName[d.Name] {
			t.Errorf("duplicate capability name %q", d.Name)
		}
		seenName[d.Name] = true
	}
}

// Each area's mask is stored in an INTEGER column, which is 32-bit and SIGNED on Postgres.
// A capability above bit 30 would be refused by the column — on Postgres only, which is the
// worst way to find out.
func TestCeiling(t *testing.T) {
	for _, d := range All {
		if d.Bit() > MaxBit {
			t.Fatalf("%s is at bit %d of area %q, above the %d ceiling. role_caps.mask is INTEGER "+
				"— 32-bit signed on Postgres — so past bit %d the grant fails as \"integer out of "+
				"range\", in production, on one dialect. Add an area rather than a wider column — "+
				"that is what areas are for.", d.Name, d.Bit(), d.NS(), MaxBit, MaxBit)
		}
	}
}

// The zero Cap must never be satisfied. This is what makes a route that forgot to declare a
// capability fail closed rather than open.
func TestZeroCapNeverMatches(t *testing.T) {
	if Everything.Has(Cap{}) {
		t.Fatal("the zero Cap matched a full set — an undeclared route capability would " +
			"authorize everything")
	}
	// A cap with a namespace but no bit, and a bit but no namespace, are both zero values in
	// the only sense that matters.
	if Everything.Has(Cap{NS: NSAdmin}) {
		t.Fatal("a Cap with no bit matched")
	}
	if Everything.Has(Cap{Bit: 1}) {
		t.Fatal("a Cap with no namespace matched")
	}

	var empty Set
	if empty.Has(ContainersView) {
		t.Fatal("an empty set matched a real capability")
	}
}

func TestEditImpliesView(t *testing.T) {
	for _, d := range All {
		if d.Mode != ModeEdit {
			continue
		}
		s := Normalize(Set{}.With(d.Cap))
		v, ok := viewOf[d.Object]
		if !ok {
			t.Errorf("%s is an edit capability but object %q has no view capability", d.Name, d.Object)
			continue
		}
		if !s.Has(v) {
			t.Errorf("Normalize(%s) did not imply %s", d.Name, v)
		}
	}
}

// Edit must NOT imply the standalone capabilities. This is the whole reason they exist: someone
// trusted to restart a container must not thereby get a root shell on the host.
func TestEditDoesNotImplyDangerous(t *testing.T) {
	dangerous := []Cap{ContainersExec, SystemPrune, BackupsRestore, BackupsDownload}

	everyEdit := Set{}
	for _, d := range All {
		if d.Mode == ModeEdit {
			everyEdit = everyEdit.With(d.Cap)
		}
	}
	got := Normalize(everyEdit)

	for _, c := range dangerous {
		if got.Has(c) {
			t.Errorf("holding every edit capability implied %s — it must be granted explicitly", c)
		}
	}
}

func TestNormalizeDropsUnknownBits(t *testing.T) {
	// Bit 31 of a real area is not in the registry and must not survive a round trip, or a
	// hand-edited database row could carry a bit that later becomes a real capability.
	if got := Normalize(Set{NSDocker: 1 << 31}); !got.IsZero() {
		t.Errorf("Normalize kept an unregistered bit: %#x", uint64(got[NSDocker]))
	}
}

// A namespace we do not know must grant NOTHING.
//
// This is what makes a downgrade safe. A newer Daffa writes role_caps rows for an area that
// this build has never heard of; an older one reads them back. Keeping those bits would mean
// resolving them against THIS build's registry — where the same numbers belong to entirely
// different capabilities — and quietly handing someone permissions nobody granted.
func TestNormalizeDropsAnUnknownNamespace(t *testing.T) {
	got := Normalize(Set{"from-the-future": 0xFFFF, NSDeploy: Mask(StacksView.Bit)})

	if _, ok := got["from-the-future"]; ok {
		t.Error("Normalize kept a namespace that is not in the registry. A row written by a " +
			"newer Daffa would be resolved against THIS build's bits, which mean different " +
			"things — granting permissions nobody ever asked for.")
	}
	if !got.Has(StacksView) {
		t.Error("Normalize dropped a legitimate capability while discarding the unknown area")
	}
}

func TestSetFromNames(t *testing.T) {
	s, err := SetFromNames([]string{"stacks.edit", "audit.view"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has(StacksEdit) || !s.Has(AuditView) {
		t.Errorf("missing requested capabilities: %v", s.Names())
	}
	if !s.Has(StacksView) {
		t.Error("stacks.edit did not imply stacks.view")
	}
	if s.Has(ContainersExec) {
		t.Error("the set contains a capability nobody asked for")
	}

	// The two live in different areas, so the set must carry both — a flat mask would have
	// merged them and this is the thing most likely to be got wrong by an implementation that
	// forgets the namespace on the way in.
	if s[NSDeploy] == 0 || s[NSObserve] == 0 {
		t.Errorf("capabilities from two areas did not both survive: %+v", s)
	}

	// An unknown name must be an error, never silently dropped: a role that does less than the
	// screen says it does is worse than an error message.
	if _, err := SetFromNames([]string{"stacks.edit", "stacks.nope"}); err == nil {
		t.Fatal("an unknown capability name was accepted")
	}
}

// Two capabilities in DIFFERENT areas may share a bit number, and must not be confusable.
//
// This is the new failure mode that namespacing introduces, and the one a flat-mask habit walks
// straight into: stacks.view and backups.view are both bit 0. Any check that compares bits
// without comparing areas would answer yes to the wrong question.
func TestSameBitInDifferentAreasDoesNotCollide(t *testing.T) {
	if StacksView.Bit != BackupsView.Bit {
		t.Skip("these two no longer share a bit number; the collision this guards is elsewhere")
	}

	only := Set{}.With(StacksView)
	if !only.Has(StacksView) {
		t.Fatal("a set carrying stacks.view does not have it")
	}
	if only.Has(BackupsView) {
		t.Fatal("a set carrying ONLY stacks.view also reported backups.view — the check is " +
			"comparing bits without comparing areas, so every capability now aliases onto the " +
			"one that shares its number in every other area")
	}
}

// The frontend's capability check must survive a capability above bit 31, and this test runs the
// real generated code to prove it.
//
// It exists because it did not, and the way we found out was an administrator — holding every
// capability in the registry — being told they could not create a resource monitor.
//
// JavaScript's bitwise operators coerce both operands to 32-bit SIGNED integers before doing
// anything at all, whatever the numbers' precision. So `mask & cap` where cap is bit 32
// (4294967296) evaluates the cap to zero, and `mask & 0` is zero, and the check can never pass
// for anybody. monitors.view (then bit 31) survived by accident — the sign bit is still a bit —
// so the tab appeared and the editor did not, which is a uniquely confusing way to fail.
//
// Namespacing means nothing is near bit 31 today. The test stays anyway: the trap is in the
// operator, not in the number, and the next area to fill up would spring it again.
func TestTheBrowserCanSeeEveryCapability(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the generated frontend check is NOT covered by this run")
	}

	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "caps.ts"))
	if err != nil {
		t.Fatal(err)
	}

	// The generated file is TypeScript, but the parts under test are plain JavaScript once the
	// type annotations come off. Strip them rather than pull a compiler in: the point is to
	// execute the ACTUAL shipped arithmetic, not a Go transcription of it that could be right
	// while the real one is wrong.
	js := string(src)
	js = regexp.MustCompile(`(?s)export interface Cap \{.*?\n\}\n`).ReplaceAllString(js, "")
	js = regexp.MustCompile(`(?m)^export type .*$`).ReplaceAllString(js, "")
	js = strings.NewReplacer(
		"export const Ns = {", "const Ns = {",
		"export const Cap = {", "const Cap = {",
		"} as const satisfies Record<string, Cap>", "}",
		"} as const", "}",
		"export function hasCap(set: CapSet | undefined, cap: CapValue): boolean {",
		"function hasCap(set, cap) {",
	).Replace(js)

	// An administrator resolves to every capability at runtime — this exact object is what the
	// server puts in /api/auth/me, and it is what the browser has to reason about.
	var checks strings.Builder
	fmt.Fprintf(&checks, "\nconst set = %s;\nlet bad = [];\n", jsSet(Everything))
	for _, d := range All {
		fmt.Fprintf(&checks, "if (!hasCap(set, Cap[%q])) bad.push(%q);\n", tsNameFor(d.Name), d.Name)
	}
	checks.WriteString(`
if (bad.length) { console.log("MISSING:" + bad.join(",")); process.exit(1); }
console.log("ok");
`)

	out, err := exec.Command(node, "-e", js+checks.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("the browser cannot see every capability an administrator holds.\n\n%s\n"+
			"Either a capability is being lost above bit 31 — if hasCap has been \"simplified\" "+
			"back to `(mask & cap.bit) !== 0`, that is the bug, because JavaScript's bitwise "+
			"operators are 32-bit — or the namespace is being ignored and every area is being "+
			"checked against the wrong mask.", out)
	}
}

// jsSet renders a Set as the JSON object the API actually sends.
func jsSet(s Set) string {
	keys := make([]string, 0, len(s))
	for ns := range s {
		keys = append(keys, string(ns))
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, ns := range keys {
		parts = append(parts, fmt.Sprintf("%q: %d", ns, uint32(s[Namespace(ns)])))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// tsNameFor mirrors the generator's naming, so the test asks for the same constants it emits.
func tsNameFor(name string) string {
	compound := map[string]string{"gitcreds": "GitCreds"}
	var out strings.Builder
	for _, part := range strings.Split(name, ".") {
		if c, ok := compound[part]; ok {
			out.WriteString(c)
			continue
		}
		out.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return out.String()
}
