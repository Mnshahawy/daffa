package stacks

import (
	"context"
	"strings"
	"testing"
)

// The most expensive Swarm mistake available, and it is completely silent.
//
// A named volume in Swarm is NODE-LOCAL. Reschedule the task — drain a node, reboot a machine, roll
// an update — and the new task gets a fresh, empty volume of the same name on the new machine. The
// service comes up healthy. The database is gone. Nothing errors.
//
// Daffa already parses the compose file, so it can just say so.
func TestTheVolumeTrapIsNamedOutLoud(t *testing.T) {
	yaml := `
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
  web:
    image: nginx
volumes:
  pgdata:
`
	ws, err := SwarmWarnings(context.Background(), yaml, "shop", nil, 3)
	if err != nil {
		t.Fatal(err)
	}

	if len(ws) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 (db); a stateless service must not be warned "+
			"about, or the warning becomes noise nobody reads: %+v", len(ws), ws)
	}
	if ws[0].Service != "db" {
		t.Fatalf("warned about %q; the service with the volume is db", ws[0].Service)
	}

	text := ws[0].Text
	for _, want := range []string{"pgdata", "node-local", "3 nodes"} {
		if !strings.Contains(text, want) {
			t.Errorf("the warning does not mention %q, so it does not actually explain the trap:\n%s",
				want, text)
		}
	}
}

// A service that has said where it runs has thought about this. Warning it anyway is how a warning
// becomes something people learn to scroll past — and then the one that mattered scrolls past too.
func TestAConstrainedServiceIsNotWarnedAbout(t *testing.T) {
	yaml := `
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
    deploy:
      placement:
        constraints:
          - node.labels.storage == ssd
volumes:
  pgdata:
`
	ws, err := SwarmWarnings(context.Background(), yaml, "shop", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("a service pinned by a placement constraint was warned about anyway: %+v.\n\n"+
			"Its volume is always the same volume — there is nothing to warn about, and crying "+
			"wolf here trains people to ignore the case that is real.", ws)
	}
}

// A bind mount is the operator's own directory and they know exactly where it is. An anonymous
// volume holds nothing anybody meant to keep. Neither is the trap.
func TestOnlyNamedVolumesAreTheTrap(t *testing.T) {
	yaml := `
services:
  a:
    image: nginx
    volumes:
      - /srv/data:/data
  b:
    image: nginx
    volumes:
      - /scratch
`
	ws, err := SwarmWarnings(context.Background(), yaml, "shop", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("a bind mount or an anonymous volume was reported as the named-volume trap: %+v", ws)
	}
}

// On a single-node swarm the trap cannot spring — there is nowhere else for the task to go. But it
// springs the day somebody adds a second machine, which is exactly when nobody is thinking about
// it. So it is still said, and it is said differently: honestly, without pretending today's
// deployment is in danger.
func TestASingleNodeSwarmIsWarnedAboutTheFuture(t *testing.T) {
	yaml := `
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`
	ws, err := SwarmWarnings(context.Background(), yaml, "shop", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("a single-node swarm was told nothing about a volume that will not survive its "+
			"second node: %+v", ws)
	}
	if !strings.Contains(ws[0].Text, "second node") {
		t.Errorf("the single-node warning does not explain WHEN this bites:\n%s", ws[0].Text)
	}
	// And it must not claim the data is at risk right now, because it is not.
	if strings.Contains(ws[0].Text, "will NOT find its data") {
		t.Errorf("the single-node warning claims today's data is in danger. It is not, and a "+
			"warning that overstates gets switched off:\n%s", ws[0].Text)
	}
}
