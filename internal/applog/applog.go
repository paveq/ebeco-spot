// Package applog builds the application's slog.Logger for a chosen output:
// structured text on stdout, or the macOS unified logging system via os_log.
package applog

import (
	"fmt"
	"log/slog"
	"os"
)

// Output names, mirroring config.LogStdout / config.LogOSLog.
const (
	Stdout = "stdout"
	OSLog  = "oslog"
)

// New returns a logger that writes at the given level to the named output.
// "stdout" emits slog's structured text; "oslog" routes records into the macOS
// unified logging system (and errors on non-macOS builds).
func New(output string, level slog.Level) (*slog.Logger, error) {
	switch output {
	case "", Stdout:
		h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
		return slog.New(h), nil
	case OSLog:
		h, err := newOSLogHandler(level)
		if err != nil {
			return nil, err
		}
		return slog.New(h), nil
	default:
		return nil, fmt.Errorf("unknown log output %q (want %q or %q)", output, Stdout, OSLog)
	}
}
