//go:build darwin

package applog

/*
#include <os/log.h>
#include <stdlib.h>

// os_log_with_type is a macro, so it cannot be called from cgo directly. These
// thin wrappers expose the pieces we need. The format is a constant "%{public}s"
// so the (already-formatted) message is logged verbatim and not redacted.
static os_log_t ebeco_log_create(const char *subsystem, const char *category) {
	return os_log_create(subsystem, category);
}

static void ebeco_log_emit(os_log_t logger, unsigned char type, const char *msg) {
	os_log_with_type(logger, type, "%{public}s", msg);
}
*/
import "C"

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"unsafe"
)

// os_log_type_t values from <os/log.h>.
const (
	osLogTypeDefault = 0x00 // persisted, shown by `log show` without --info
	osLogTypeDebug   = 0x02 // memory-only unless debug logging is enabled
	osLogTypeError   = 0x10
)

// logSubsystem tags every record so it can be filtered with
//
//	log show --predicate 'subsystem == "com.github.paveq.ebeco-spot"'
const logSubsystem = "com.github.paveq.ebeco-spot"

func newOSLogHandler(level slog.Level) (slog.Handler, error) {
	sub := C.CString(logSubsystem)
	cat := C.CString("controller")
	defer C.free(unsafe.Pointer(sub))
	defer C.free(unsafe.Pointer(cat))
	return &oslogHandler{level: level, logger: C.ebeco_log_create(sub, cat)}, nil
}

// oslogHandler is a slog.Handler that formats each record as logfmt (minus the
// timestamp, which os_log adds itself) and emits it via os_log_with_type.
type oslogHandler struct {
	level  slog.Level
	logger C.os_log_t
	pre    string // preformatted attrs from WithAttrs
	prefix string // current group key prefix, e.g. "g1.g2."
}

func (h *oslogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *oslogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString("level=")
	b.WriteString(r.Level.String())
	b.WriteString(" msg=")
	appendValue(&b, r.Message)
	b.WriteString(h.pre)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.prefix, a)
		return true
	})

	cmsg := C.CString(b.String())
	defer C.free(unsafe.Pointer(cmsg))
	C.ebeco_log_emit(h.logger, C.uchar(osLogType(r.Level)), cmsg)
	return nil
}

func (h *oslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	nh := *h
	var b strings.Builder
	b.WriteString(h.pre)
	for _, a := range attrs {
		appendAttr(&b, h.prefix, a)
	}
	nh.pre = b.String()
	return &nh
}

func (h *oslogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.prefix = h.prefix + name + "."
	return &nh
}

// osLogType maps slog levels onto os_log types. INFO and WARN both map to
// DEFAULT so they persist and appear in `log show` without --info (os_log's own
// INFO/DEBUG types are memory-only by default); the textual level is preserved
// in the message itself.
func osLogType(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return osLogTypeError
	case l >= slog.LevelInfo:
		return osLogTypeDefault
	default:
		return osLogTypeDebug
	}
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		grp := a.Value.Group()
		if len(grp) == 0 {
			return
		}
		np := prefix
		if a.Key != "" {
			np = prefix + a.Key + "."
		}
		for _, ga := range grp {
			appendAttr(b, np, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	appendValue(b, a.Value.String())
}

func appendValue(b *strings.Builder, s string) {
	if needsQuote(s) {
		b.WriteString(strconv.Quote(s))
		return
	}
	b.WriteString(s)
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' {
			return true
		}
	}
	return false
}
