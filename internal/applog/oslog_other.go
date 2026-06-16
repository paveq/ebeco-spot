//go:build !darwin

package applog

import (
	"fmt"
	"log/slog"
)

// newOSLogHandler is unavailable off macOS; os_log is an Apple API.
func newOSLogHandler(slog.Level) (slog.Handler, error) {
	return nil, fmt.Errorf("oslog output is only supported on macOS")
}
