package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// TestEveryRouteIsGuarded is the anti-footgun for the whole authorization model.
//
// The old shape registered each privileged route twice — once on a role sub-mux and once
// wrapped on the main mux — and nothing noticed when the two disagreed. Worse, a route
// added to the main mux and forgotten on the sub-mux was simply open to every signed-in
// user, and looked exactly like a route that was open on purpose.
//
// Now: a route declares a capability, or it declares WHY it needs none. Both, or neither,
// fails here. "Open by choice" is a string a reviewer reads; "open by omission" does not
// compile past this test.
func TestEveryRouteIsGuarded(t *testing.T) {
	s := &Server{}

	for _, rt := range s.apiRoutes() {
		switch {
		case rt.cap.IsZero() && rt.open == "":
			t.Errorf("%s declares no capability and no reason to be open.\n"+
				"Every /api route must do one or the other — otherwise it is reachable by any "+
				"signed-in user, and nobody can tell whether that was intended.", rt.pattern)

		case !rt.cap.IsZero() && rt.open != "":
			t.Errorf("%s declares BOTH a capability (%s) and an open reason (%q). "+
				"Pick one — as written, the capability is what runs and the reason is a lie.",
				rt.pattern, rt.cap, rt.open)

		case rt.open != "" && len(rt.open) < 20:
			// A reason has to be an argument, not a shrug. "n/a" is not a justification
			// for an unauthenticated-in-effect route.
			t.Errorf("%s is open with the reason %q — say why, in a sentence someone "+
				"reviewing this in a year can evaluate.", rt.pattern, rt.open)
		}

		if rt.h == nil {
			t.Errorf("%s has no handler", rt.pattern)
		}
	}
}

// Every route must say WHERE its capability is checked, not just which one.
//
// Grants can be limited to a host, so "may this person do X" is only half a question. A
// route that declared no scope would fall into the default branch of guard() — which
// panics, deliberately, but this test is what makes sure nobody ever gets there.
func TestEveryRouteDeclaresAScope(t *testing.T) {
	s := &Server{}

	for _, rt := range s.apiRoutes() {
		if rt.scope == scopeUnset {
			t.Errorf("%s declares no scope. Say where its capability applies: scopeGlobal, "+
				"scopeEnv, scopeStack, scopeJob, scopeAny, or scopeBody.", rt.pattern)
		}
		if rt.open != "" && rt.scope != scopeNone {
			t.Errorf("%s is open but declares a scope — there is nothing to scope", rt.pattern)
		}
		if rt.open == "" && rt.scope == scopeNone {
			t.Errorf("%s declares a capability but scopeNone", rt.pattern)
		}
	}
}

// The routes whose environment arrives in the request BODY, where no middleware can see it.
// Their handlers check for themselves, via s.mayUseEnv.
//
// The list is pinned because it is the one place the route table's guarantee does not hold: a
// body-scoped route added without its check would be silently unguarded across every host.
// Adding one has to be a decision somebody makes on purpose, in this file.
//
// POST /api/monitors was that decision. Its handler checks via s.mayMonitor, which adds a rule
// the other two do not need: an ABSENT environment is not "unspecified", it is "every host" —
// a fleet-wide rule — so it takes monitors.edit globally rather than on any one host.
func TestBodyScopedRoutesAreKnown(t *testing.T) {
	// POST /api/volume-sources joined the list with 0015: a volume source delivers repo
	// content onto one host, so its handler checks volsources.edit at the env decoded
	// from the body, before any other decision.
	want := map[string]bool{
		"POST /api/stacks":         true,
		"POST /api/backups":        true,
		"POST /api/monitors":       true,
		"POST /api/volume-sources": true,
		// Enrolling an agent adds a node to the cluster named in the body, so its handler
		// checks nodes.edit there via s.mayUseEnv before minting a join token.
		"POST /api/agents": true,
		// Inline-compose image upgrades: the env arrives in the body and each handler
		// checks it via s.mayUseEnv before touching a registry. See .ai/image-upgrades.md.
		"POST /api/compose/images":      true,
		"POST /api/compose/tag-check":   true,
		"POST /api/compose/latest-hint": true,
		"POST /api/compose/rewrite":     true,
	}

	s := &Server{}
	got := map[string]bool{}
	for _, rt := range s.apiRoutes() {
		if rt.scope == scopeBody {
			got[rt.pattern] = true
		}
	}

	for p := range got {
		if !want[p] {
			t.Errorf("%s is body-scoped but was not expected.\n"+
				"A body-scoped route is NOT checked by any middleware — its handler must call "+
				"s.mayUseEnv after decoding, or it is reachable on every host. Add it to this "+
				"list once you have written that check.", p)
		}
	}
	for p := range want {
		if !got[p] {
			t.Errorf("%s was expected to be body-scoped but is not — has it been re-scoped, "+
				"and if so is its handler still calling s.mayUseEnv?", p)
		}
	}
}

// A route that names a stack or a job must not be scopeEnv: there is no {cluster} in its path,
// so the extractor would return "" and only a GLOBAL grant would ever satisfy it — which
// would silently lock out every scoped user, and look like a permissions bug.
func TestTargetRoutesUseTheRightScope(t *testing.T) {
	s := &Server{}
	for _, rt := range s.apiRoutes() {
		_, path, _ := strings.Cut(rt.pattern, " ")

		hasEnvVar := strings.Contains(path, "{cluster}")
		if rt.scope == scopeEnv && !hasEnvVar {
			t.Errorf("%s is scopeEnv but has no {cluster} in its path — the extractor would "+
				"return \"\" and only a global grant would pass", rt.pattern)
		}
		if hasEnvVar && rt.scope != scopeEnv && rt.scope != scopeGlobal && rt.scope != scopeNone {
			t.Errorf("%s has an {cluster} but is %v — is that deliberate?", rt.pattern, rt.scope)
		}
		if rt.scope == scopeStack && !strings.HasPrefix(path, "/api/stacks/{id}") {
			t.Errorf("%s is scopeStack but its {id} is not a stack", rt.pattern)
		}
		if rt.scope == scopeJob && !strings.HasPrefix(path, "/api/backups/{id}") {
			t.Errorf("%s is scopeJob but its {id} is not a backup job", rt.pattern)
		}
		if rt.scope == scopeVolumeSource && !strings.HasPrefix(path, "/api/volume-sources/{id}") {
			t.Errorf("%s is scopeVolumeSource but its {id} is not a volume source", rt.pattern)
		}
	}
}

// Two routes with the same pattern would make http.ServeMux panic at boot. Better to find
// out here than on a deploy.
func TestNoDuplicateRoutes(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, rt := range s.apiRoutes() {
		if seen[rt.pattern] {
			t.Errorf("duplicate route %s", rt.pattern)
		}
		seen[rt.pattern] = true
	}
}

// Every route in the table must actually be under /api/, because that is the only subtree
// the session middleware wraps. A route registered here but living elsewhere in the URL
// space would be UNAUTHENTICATED — the capability beside it would never run.
func TestAllRoutesAreUnderAPI(t *testing.T) {
	s := &Server{}
	for _, rt := range s.apiRoutes() {
		_, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			t.Errorf("%q is not a METHOD /path pattern", rt.pattern)
			continue
		}
		if !strings.HasPrefix(path, "/api/") {
			t.Errorf("%s is outside /api/, so s.sessions.Require never wraps it and its "+
				"capability check would never run against an authenticated user", rt.pattern)
		}
	}
}

// The routes that are deliberately outside /api/ — agent enrolment, the git webhook — are
// machine-authenticated (bearer token, HMAC) and must NOT be in this table. If one ever
// migrated in, it would start demanding a browser session and silently stop working.
//
// Note this is about the top-level path, not the word: /api/agents/{id} is the session-
// guarded route by which a human revokes an agent, and belongs here. /agents/connect is
// the one the agent itself dials, and does not.
func TestMachineRoutesAreNotInTheTable(t *testing.T) {
	s := &Server{}
	for _, rt := range s.apiRoutes() {
		_, path, _ := strings.Cut(rt.pattern, " ")
		for _, machine := range []string{"/agents/", "/webhooks/"} {
			if strings.HasPrefix(path, machine) {
				t.Errorf("%s is a machine-authenticated route and does not belong in the "+
					"session-guarded table", rt.pattern)
			}
		}
	}
}

// Every response says it is not for indexing — including the ones that are not HTML.
//
// The <meta name="robots"> tag in index.html covers only what a crawler parses as a document.
// That leaves the API, every asset, and — because the SPA answers ANY unknown path with the same
// shell and a 200 — an unbounded supply of URLs that look like distinct pages from the outside.
// The header is the only signal that covers all of it, and it is one line to lose by accident.
func TestEveryResponseRefusesIndexing(t *testing.T) {
	const want = "noindex, nofollow, noarchive, nosnippet, noimageindex"

	h := noIndex(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// The SPA shell, an API response, an asset, and a path that does not exist — the last one
	// matters most, since that is what a crawler wandering in actually hits.
	for _, path := range []string{"/", "/api/stacks", "/assets/index-abc123.js", "/does/not/exist"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if got := rec.Header().Get("X-Robots-Tag"); got != want {
			t.Errorf("GET %s: X-Robots-Tag = %q; want %q", path, got, want)
		}
	}
}

// A Swarm join token is a CREDENTIAL: anybody holding one can add a machine to the cluster, and a
// machine in the cluster runs whatever the cluster schedules onto it.
//
// Docker hands the tokens back from GET /swarm alongside a great deal of harmless information, so
// any handler that inspects a swarm and forwards the result is one careless line away from leaking
// them. Portainer has to strip JoinTokens out of that response for non-admins; the way to not need
// that is to never put them in a shared payload at all.
//
// So: exactly one route serves them, and it takes the capability that says so.
func TestJoinTokensAreServedByOneRouteAndTakeSwarmEdit(t *testing.T) {
	s := &Server{}

	var serving []route
	for _, rt := range s.apiRoutes() {
		if strings.Contains(rt.pattern, "/swarm/tokens") {
			serving = append(serving, rt)
		}
	}

	if len(serving) != 1 {
		t.Fatalf("%d routes serve the join tokens; want exactly 1.\n\n"+
			"They are a credential. Every additional route that can return one is another place it "+
			"can leak from, and the reason Portainer has to strip them out of a shared response is "+
			"that it put them in one.", len(serving))
	}

	if serving[0].cap != caps.SwarmEdit {
		t.Errorf("the join-token route is guarded by %v; want swarm.edit.\n\n"+
			"Reading a join token is not reading a page. It is taking a key to the cluster.",
			serving[0].cap)
	}
}

// A secret's VALUE is not readable — not by a capability, not by a route, not by anyone. Docker does
// not return it: SecretInspect answers with the spec and an empty Data field, always.
//
// So there must be no route that looks like it could serve one. This test is a tripwire for the
// well-meaning future change that adds `GET /secrets/{id}` because every other resource has one, and
// then has to invent something to put in the body.
func TestNoRouteCanServeASecretsValue(t *testing.T) {
	s := &Server{}

	for _, rt := range s.apiRoutes() {
		if !strings.HasPrefix(rt.pattern, "GET ") {
			continue
		}
		// A LIST of secrets is fine — it carries names, labels and what is mounting them.
		// An INSPECT of one is not, because the only thing it could add is the value.
		if strings.Contains(rt.pattern, "/secrets/{id}") {
			t.Errorf("%s exists.\n\n"+
				"There is nothing for it to return. Docker never hands back a secret's value, so an "+
				"inspect route can only either duplicate the list or invent a field that cannot be "+
				"filled. The one way to read a secret out of a Swarm is to mount it into a container "+
				"you control — which is what services.edit already tells you it confers.", rt.pattern)
		}
	}
}
