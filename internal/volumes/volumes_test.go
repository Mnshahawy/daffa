package volumes

import (
	"archive/tar"
	"errors"
	"io"
	"testing"
)

func TestTarFiles(t *testing.T) {
	// Deliberately unsorted, with an explicit mode and a defaulted one.
	files := []File{
		{Name: "z-last.yml", Data: []byte("z: 1\n")},
		{Name: "a-first.key", Data: []byte("secret"), Mode: 0o600},
		{Name: "bin/hook.sh", Data: []byte("#!/bin/sh\n"), Mode: 0o755},
	}

	tr := tar.NewReader(tarFiles(files, 42, 43))

	type entry struct {
		mode     int64
		uid, gid int
		content  string
	}
	got := map[string]entry{}
	var order []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		order = append(order, h.Name)
		got[h.Name] = entry{mode: h.Mode, uid: h.Uid, gid: h.Gid, content: string(b)}
	}

	if len(order) != 3 {
		t.Fatalf("got %d entries, want 3", len(order))
	}
	// Sorted, regardless of input order — the same file set must always produce the
	// same archive, or content hashing upstream lies about changes.
	want := []string{"a-first.key", "bin/hook.sh", "z-last.yml"}
	for i, name := range want {
		if order[i] != name {
			t.Fatalf("entry %d is %s, want %s", i, order[i], name)
		}
	}

	if e := got["a-first.key"]; e.mode != 0o600 || e.content != "secret" {
		t.Errorf("a-first.key: mode %o content %q", e.mode, e.content)
	}
	if e := got["bin/hook.sh"]; e.mode != 0o755 {
		t.Errorf("bin/hook.sh: mode %o, want 755", e.mode)
	}
	// Zero Mode defaults to 0644 rather than shipping an unreadable file.
	if e := got["z-last.yml"]; e.mode != 0o644 {
		t.Errorf("z-last.yml: mode %o, want 644 (the default)", e.mode)
	}
	for name, e := range got {
		if e.uid != 42 || e.gid != 43 {
			t.Errorf("%s: uid/gid %d/%d, want 42/43", name, e.uid, e.gid)
		}
	}
}

func TestSnapshotStreamCloseRunsCleanup(t *testing.T) {
	cleaned := false
	s := &snapshotStream{
		rc:      io.NopCloser(nil),
		cleanup: func() { cleaned = true },
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !cleaned {
		t.Fatal("Close did not remove the helper — that is a container leaked on someone's box")
	}
}
