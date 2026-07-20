package dockerx

import "testing"

func TestAttributeLine(t *testing.T) {
	taskName := map[string]string{"316p5f38c5s9o7zi5h6rq1nuu": "demo_whoami.3"}
	nodeName := map[string]string{"aq1enldc2p5m0um9ybmr0ab3y": "ip-172-31-12-191"}

	// The exact shape the daemon writes with Details on: attrs sorted by key, url-escaped, joined by
	// commas, then ONE space, then the message (api/server/httputils.WriteLogStream).
	in := LogLine{Stream: "stdout", Text: "com.docker.swarm.node.id=aq1enldc2p5m0um9ybmr0ab3y," +
		"com.docker.swarm.service.id=jgcklajr583kbtfbx06tl0aa2," +
		"com.docker.swarm.task.id=316p5f38c5s9o7zi5h6rq1nuu 2026/07/20 04:14:51 Starting up on port 80"}

	got := attributeLine(in, taskName, nodeName)
	if got.Task != "demo_whoami.3" || got.Node != "ip-172-31-12-191" {
		t.Fatalf("attribution: task=%q node=%q", got.Task, got.Node)
	}
	if got.Text != "2026/07/20 04:14:51 Starting up on port 80" {
		t.Fatalf("prefix not stripped: %q", got.Text)
	}
}

func TestAttributeLineUnresolvedFallsBackToShortID(t *testing.T) {
	// A task older than the daemon's history limit is in no map; the line still gets a short id
	// rather than nothing, and the message is still cleaned of its prefix.
	in := LogLine{Stream: "stdout", Text: "com.docker.swarm.node.id=aq1enldc2p5m0um9ybmr0ab3y," +
		"com.docker.swarm.task.id=deadbeefdeadbeefdeadbeef hello"}
	got := attributeLine(in, map[string]string{}, map[string]string{})
	if got.Task != "deadbeefdead" || got.Node != "aq1enldc2p5m" {
		t.Fatalf("short-id fallback: task=%q node=%q", got.Task, got.Node)
	}
	if got.Text != "hello" {
		t.Fatalf("text=%q", got.Text)
	}
}

func TestAttributeLinePassesThroughPlainText(t *testing.T) {
	// A line without the swarm-details prefix (Details off, or a container log reusing LogLine) is
	// left exactly as-is — no attribution, no truncation at the first space.
	in := LogLine{Stream: "stderr", Text: "plain log line with spaces"}
	got := attributeLine(in, nil, nil)
	if got.Text != "plain log line with spaces" || got.Task != "" || got.Node != "" {
		t.Fatalf("plain line altered: %+v", got)
	}
}

func TestDaemonStreamNote(t *testing.T) {
	// StdCopy wraps a system-error frame with this fixed prefix — the partial-coverage warning we
	// resume past rather than fail on.
	note, ok := daemonStreamNote(errString("error from daemon in stream: node abc is not available"))
	if !ok || note != "node abc is not available" {
		t.Fatalf("note=%q ok=%v", note, ok)
	}
	// A real demux failure is not a note.
	if _, ok := daemonStreamNote(errString("dockerx: demultiplexing logs: unexpected EOF")); ok {
		t.Fatal("a demux error must not be treated as a resumable note")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
