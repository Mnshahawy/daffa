package volumes

import (
	"bytes"
	"fmt"
	"strings"
)

// ManifestName is the file every mirrored sync writes into the volume root: the list of
// files Daffa delivered, plus where they came from. It is what lets the NEXT sync delete
// exactly the files Daffa wrote and the repo no longer contains — and never anything the
// consumer wrote beside them ("refuse, don't orphan", in the deletion direction). The
// leading dot keeps it out of glob-watching consumers like Traefik's file provider.
const ManifestName = ".daffa-manifest"

// Manifest builds the manifest content. Names are the delivered files, manifest excluded
// (it is always rewritten, so listing it would only mean never deleting it — same result,
// less honest a list).
func Manifest(commit, hash string, names []string) []byte {
	var b bytes.Buffer
	b.WriteString("# Written by Daffa. Do not edit: it is rewritten on every sync.\n")
	fmt.Fprintf(&b, "# commit %s\n", commit)
	fmt.Fprintf(&b, "# hash %s\n", hash)
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ParseManifest returns the file names a previous sync recorded. Parsing is tolerant —
// comments and blank lines are skipped — because a half-understood manifest must degrade
// to deleting less, never more.
func ParseManifest(b []byte) []string {
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, line)
	}
	return names
}
