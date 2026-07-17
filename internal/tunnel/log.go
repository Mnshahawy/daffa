package tunnel

import (
	"io"
	"log"
)

// yamux insists on a *log.Logger. We route it to nowhere and report connection state
// ourselves through slog, so the operator sees one log format rather than two.
func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
