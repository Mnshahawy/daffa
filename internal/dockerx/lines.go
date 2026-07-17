package dockerx

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// These little constructors keep the Docker option structs in one place; the API moves
// them between packages and versions more than anything else we depend on, so an
// upgrade should break one line here rather than five call sites.

func DockerEventOptions(f filters.Args) events.ListOptions {
	return events.ListOptions{Filters: f}
}

func DiskUsageOptions() types.DiskUsageOptions {
	return types.DiskUsageOptions{}
}

func BuildCachePruneOptions() types.BuildCachePruneOptions {
	// Not All: pruning only the unused cache leaves the layers an in-flight build is
	// relying on alone.
	return types.BuildCachePruneOptions{}
}

// lineWriter turns the byte stream Docker hands us into whole lines. Docker does not
// promise that a Write lands on a line boundary, so a naive "one Write = one line"
// emitter splits long log lines in half and interleaves them.
type lineWriter struct {
	stream string
	emit   func(LogLine) error
	buf    bytes.Buffer
}

func newLineWriter(stream string, emit func(LogLine) error) *lineWriter {
	return &lineWriter{stream: stream, emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf.Write(p)
			// Guard against a container that writes megabytes without a newline:
			// flush anyway rather than growing without bound.
			if w.buf.Len() > 64*1024 {
				if err := w.flush(); err != nil {
					return n, err
				}
			}
			return n, nil
		}

		w.buf.Write(p[:i])
		if err := w.flush(); err != nil {
			return n, err
		}
		p = p[i+1:]
	}
}

func (w *lineWriter) flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	text := strings.TrimRight(w.buf.String(), "\r")
	w.buf.Reset()
	return w.emit(LogLine{Stream: w.stream, Text: text})
}

// scanLines reads a raw (TTY) stream line by line.
func scanLines(r io.Reader, emit func(string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := emit(strings.TrimRight(sc.Text(), "\r")); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && !isClosed(err) {
		return err
	}
	return nil
}
