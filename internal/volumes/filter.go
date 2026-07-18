package volumes

import (
	"archive/tar"
	"io"
	"path"
	"strings"
)

// FilterTar returns a tar stream identical to src but with every entry whose path —
// relative to the archive root — equals, or lies under, one of the excluded paths
// dropped. Excludes match as cleaned path prefixes: "cache" drops "./cache" and
// everything beneath it, "tmp/sessions" drops just that subtree. There are no
// wildcards, on purpose — prefix/subtree is what "--exclude=DIR" means and the only
// rule that stays predictable across a stranger's directory layout.
//
// This lives in Go at all because the volume tar is built by the daemon's
// CopyFromContainer, which has no --exclude flag: the sole place to drop an entry is
// between reading it and writing it back. src is read but NOT closed — the caller owns
// it (a Snapshot stream it already defers Close on); closing it here would double-close
// the helper container's cleanup.
//
// With no excludes, src is returned untouched — no goroutine, no re-encode.
func FilterTar(src io.Reader, exclude []string) io.ReadCloser {
	prefixes := cleanExcludes(exclude)
	if len(prefixes) == 0 {
		return io.NopCloser(src)
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		tr := tar.NewReader(src)
		var err error
		for {
			var h *tar.Header
			h, err = tr.Next()
			if err == io.EOF {
				err = nil
				break
			}
			if err != nil {
				break
			}
			if excluded(h.Name, prefixes) {
				continue
			}
			if err = tw.WriteHeader(h); err != nil {
				break
			}
			// Directory and other non-regular entries carry no body; io.Copy on them is
			// a harmless zero-byte read, so no type switch is needed here.
			if _, err = io.Copy(tw, tr); err != nil {
				break
			}
		}
		if err == nil {
			// Flush the footer before signalling clean EOF — an archive without it reads
			// as truncated, and even an all-excluded volume still emits this, so the
			// downstream empty-archive guard never false-trips.
			err = tw.Close()
		}
		// nil closes the pipe cleanly (reader sees EOF); non-nil surfaces to the reader.
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// cleanExcludes normalizes the patterns once and drops the ones that would match
// nothing meaningful ("" and "." — the latter is the archive root, which is never a
// path a caller means to exclude).
func cleanExcludes(exclude []string) []string {
	out := make([]string, 0, len(exclude))
	for _, e := range exclude {
		if p := normalizeEntry(e); p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// excluded reports whether a tar entry name falls under any pattern. Both sides are run
// through normalizeEntry so "./cache/x" and a "cache" pattern compare on equal footing.
func excluded(name string, prefixes []string) bool {
	n := normalizeEntry(name)
	for _, p := range prefixes {
		if n == p || strings.HasPrefix(n, p+"/") {
			return true
		}
	}
	return false
}

// normalizeEntry maps a tar entry name or an exclude pattern to one comparable form:
// the daemon roots entries at "./", so that prefix is stripped, then path.Clean folds
// away trailing slashes and redundant separators. The archive root "./" normalizes to
// ".".
func normalizeEntry(name string) string {
	return path.Clean(strings.TrimPrefix(name, "./"))
}
