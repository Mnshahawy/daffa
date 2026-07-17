package volumes

import (
	"reflect"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	names := []string{"b.yml", "sub/dir/a.yml", "hook.sh"}
	b := Manifest("abc123", "deadbeef", names)

	got := ParseManifest(b)
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("round trip lost names: got %v, want %v", got, names)
	}
}

func TestParseManifestDegradesToDeletingLess(t *testing.T) {
	// A hand-mangled manifest must parse to fewer deletions, never more: comments, blank
	// lines and whitespace are skipped, and garbage lines are just names that will not
	// match anything current.
	got := ParseManifest([]byte("# comment\n\n  a.yml  \n# hash x\n"))
	if !reflect.DeepEqual(got, []string{"a.yml"}) {
		t.Fatalf("got %v, want just a.yml", got)
	}
	if names := ParseManifest(nil); names != nil {
		t.Fatalf("an empty manifest must list nothing, got %v", names)
	}
}
