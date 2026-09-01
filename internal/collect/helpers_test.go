package collect

import (
	"io"
	"log/slog"
)

// discardLogger keeps expected warnings — a rejected credential, a dropped
// event — out of the test output, so a real failure is the only thing on
// screen.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
