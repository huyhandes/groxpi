// Package logger configures the process-wide structured logger.
//
// Records are written to stdout in the configured format and, in the same call,
// handed to the OpenTelemetry log bridge. The bridge stamps each record with the
// trace and span identifiers of the context it was logged with, so no
// correlation is done by hand. Until telemetry.Setup installs a real logger
// provider the bridge is inert.
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"go.opentelemetry.io/contrib/bridges/otelslog"

	"github.com/huyhandes/groxpi/internal/telemetry"
)

// LevelFatal sits above slog.LevelError so a fatal record survives any level
// that admits errors. slog has no fatal level of its own.
const LevelFatal = slog.Level(12)

// LogConfig holds logging configuration
type LogConfig struct {
	Level  string // DEBUG, INFO, WARN, ERROR
	Format string // console, json
	Color  bool   // enable color output for console
}

// Init installs the configured logger as the slog default, which is what the
// package-level slog calls throughout the codebase use. An unknown level or
// format falls back to the default and is announced once the logger is up, the
// way config announces any other malformed setting.
func Init(cfg LogConfig) {
	level, levelOK := parseLevel(cfg.Level)

	var sink slog.Handler
	format := strings.ToLower(cfg.Format)
	switch format {
	case "json":
		// JSON format for log collectors
		sink = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: renameAttr,
		})
	default:
		// Console format with optional color
		sink = newConsoleHandler(os.Stdout, cfg.Color)
	}

	slog.SetDefault(slog.New(&fanout{
		level:    level,
		handlers: []slog.Handler{sink, otelslog.NewHandler(telemetry.ScopeName)},
	}))

	if !levelOK {
		warnInvalid("GROXPI_LOGGING_LEVEL", cfg.Level, "INFO")
	}
	if format != "" && format != "json" && format != "console" {
		warnInvalid("GROXPI_LOG_FORMAT", cfg.Format, "console")
	}
}

// warnInvalid matches config's malformed-variable warning, so both read the
// same in the log.
func warnInvalid(key, value, fallback string) {
	slog.Warn("Ignoring malformed environment variable, using default instead",
		"variable", key,
		"value", value,
		"default", fallback)
}

// parseLevel converts a level name to a slog.Level, reporting whether the name
// was recognised; anything unknown is INFO. An empty name is the unset default,
// not a mistake.
func parseLevel(level string) (slog.Level, bool) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return slog.LevelDebug, true
	case "INFO", "":
		return slog.LevelInfo, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	case "FATAL":
		return LevelFatal, true
	default:
		return slog.LevelInfo, false
	}
}

// Fatal logs at LevelFatal and terminates the process. slog has no equivalent,
// which is why this one wrapper exists where the rest of the codebase calls
// slog directly.
func Fatal(msg string, args ...any) {
	slog.Log(context.Background(), LevelFatal, msg, args...)
	os.Exit(1)
}

// levelName renders a level with the names log collectors are already
// configured for: lower case, and "fatal" rather than slog's "ERROR+4".
func levelName(l slog.Level) string {
	if l == LevelFatal {
		return "fatal"
	}
	return strings.ToLower(l.String())
}

// renameAttr keeps the JSON field semantics stable across the logging-library
// swap: "message" rather than slog's "msg", and lower-case level names.
func renameAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.MessageKey:
		a.Key = "message"
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(levelName(l))
		}
	}
	return a
}

// fanout writes each record to every handler that accepts it. The level gate
// lives here so that the configured level applies to the OpenTelemetry bridge as
// well as to stdout. Handler errors are dropped: logging must never fail a
// request.
type fanout struct {
	level    slog.Level
	handlers []slog.Handler
}

func (f *fanout) Enabled(_ context.Context, l slog.Level) bool { return l >= f.level }

func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	return f.derive(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (f *fanout) WithGroup(name string) slog.Handler {
	return f.derive(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

func (f *fanout) derive(fn func(slog.Handler) slog.Handler) slog.Handler {
	next := &fanout{level: f.level, handlers: make([]slog.Handler, len(f.handlers))}
	for i, h := range f.handlers {
		next.handlers[i] = fn(h)
	}
	return next
}

// consoleHandler renders human-readable single lines: time, level tag, message,
// then key=value pairs. Level filtering is the fanout's job.
type consoleHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	color  bool
	prefix string // accumulated group prefix, applied to attrs added later
	pre    string // attrs from WithAttrs, rendered with the prefix they were added under
}

// consoleTimeFormat is the wall-clock stamp on a console line. Console output is
// for a human watching a terminal; the date is not useful there.
const consoleTimeFormat = "15:04:05.000"

func newConsoleHandler(w io.Writer, color bool) *consoleHandler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, color: color}
}

func (h *consoleHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(consoleTimeFormat))
	b.WriteByte(' ')
	if h.color {
		b.WriteString(levelColor(r.Level))
		b.WriteString(levelTag(r.Level))
		b.WriteString("\x1b[0m")
	} else {
		b.WriteString(levelTag(r.Level))
	}
	b.WriteByte(' ')
	b.WriteString(r.Message)
	b.WriteString(h.pre)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.prefix, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.pre)
	for _, a := range attrs {
		appendAttr(&b, h.prefix, a)
	}
	next := *h
	next.pre = b.String()
	return &next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	next := *h
	next.prefix = h.prefix + name + "."
	return &next
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			appendAttr(b, prefix+a.Key+".", ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if needsQuote(v) {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

// needsQuote reports whether a console value must be quoted: it holds a
// key=value delimiter (space, quote, equals) or a rune that is not printable,
// such as a newline or a terminal escape.
func needsQuote(v string) bool {
	for _, r := range v {
		if !unicode.IsPrint(r) || r == ' ' || r == '"' || r == '=' {
			return true
		}
	}
	return false
}

func levelTag(l slog.Level) string {
	switch {
	case l >= LevelFatal:
		return "FTL"
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	default:
		return "DBG"
	}
}

func levelColor(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "\x1b[31m" // red
	case l >= slog.LevelWarn:
		return "\x1b[33m" // yellow
	case l >= slog.LevelInfo:
		return "\x1b[32m" // green
	default:
		return "\x1b[36m" // cyan
	}
}
