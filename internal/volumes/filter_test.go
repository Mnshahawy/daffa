package volumes

import (
	"archive/tar"
	"bytes"
	"io"
	"sort"
	"testing"
)

// entry is one file or directory in a test archive. Directories carry no body, exactly
// as the daemon's snapshot presents them.
type entry struct {
	name string
	dir  bool
	body string
}

func buildTar(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Size: int64(len(e.body)), Typeflag: tar.TypeReg, Mode: 0o644}
		if e.dir {
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatalf("write body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// readNames drains a tar stream and returns its entry names, also asserting each regular
// file's body survived intact (a filter that corrupted content would be worse than one
// that dropped the wrong entry).
func readNames(t *testing.T, r io.Reader, wantBody map[string]string) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading filtered tar: %v", err)
		}
		names = append(names, h.Name)
		if want, ok := wantBody[h.Name]; ok {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("reading body %s: %v", h.Name, err)
			}
			if string(b) != want {
				t.Errorf("body of %s = %q; want %q", h.Name, b, want)
			}
		}
	}
	sort.Strings(names)
	return names
}

func TestFilterTar(t *testing.T) {
	// The archive the daemon would hand back: rooted at "./", a mix of files and dirs.
	// "keeplogs.txt" is the trap — a "logs" exclude must match on the separator, not as a
	// substring, or it would wrongly take this file too.
	src := []entry{
		{name: "./", dir: true},
		{name: "./keep.txt", body: "keep"},
		{name: "./keeplogs.txt", body: "also keep"},
		{name: "./cache/", dir: true},
		{name: "./cache/x", body: "junk"},
		{name: "./cache/sub/", dir: true},
		{name: "./cache/sub/y", body: "more junk"},
		{name: "./logs/", dir: true},
		{name: "./logs/z", body: "log line"},
	}
	bodies := map[string]string{"./keep.txt": "keep", "./keeplogs.txt": "also keep"}

	cases := []struct {
		name    string
		exclude []string
		want    []string
	}{
		{
			name:    "directory subtrees",
			exclude: []string{"cache", "logs"},
			want:    []string{"./", "./keep.txt", "./keeplogs.txt"},
		},
		{
			name:    "exact file",
			exclude: []string{"keep.txt"},
			want:    []string{"./", "./cache/", "./cache/sub/", "./cache/sub/y", "./cache/x", "./keeplogs.txt", "./logs/", "./logs/z"},
		},
		{
			name:    "patterns normalize (trailing slash, ./ prefix)",
			exclude: []string{"./cache/", "logs/"},
			want:    []string{"./", "./keep.txt", "./keeplogs.txt"},
		},
		{
			name:    "nested pattern keeps its parent",
			exclude: []string{"cache/sub"},
			want:    []string{"./", "./cache/", "./cache/x", "./keep.txt", "./keeplogs.txt", "./logs/", "./logs/z"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := FilterTar(bytes.NewReader(buildTar(t, src)), c.exclude)
			defer rc.Close()
			got := readNames(t, rc, bodies)
			if !equalStrings(got, c.want) {
				t.Errorf("filtered names = %v; want %v", got, c.want)
			}
		})
	}
}

// An empty exclude list must pass the bytes through untouched — the common case (most
// jobs exclude nothing), and the one where re-encoding would be pure waste.
func TestFilterTarNoExcludesPassthrough(t *testing.T) {
	raw := buildTar(t, []entry{{name: "./", dir: true}, {name: "./a", body: "x"}})
	rc := FilterTar(bytes.NewReader(raw), nil)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("passthrough changed the stream: got %d bytes, want %d", len(got), len(raw))
	}
}

// Excluding every real entry still yields a valid, non-empty archive (the tar footer) —
// so the downstream empty-archive guard sees a well-formed stream, not a truncated one.
func TestFilterTarExcludeEverything(t *testing.T) {
	src := []entry{
		{name: "./", dir: true},
		{name: "./cache/", dir: true},
		{name: "./cache/x", body: "junk"},
	}
	rc := FilterTar(bytes.NewReader(buildTar(t, src)), []string{"cache"})
	defer rc.Close()
	got := readNames(t, rc, nil)
	if !equalStrings(got, []string{"./"}) {
		t.Errorf("filtered names = %v; want just the root %v", got, []string{"./"})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
