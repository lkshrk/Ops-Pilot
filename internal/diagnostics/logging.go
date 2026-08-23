package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	default:
		return "info"
	}
}

func (l Level) slog() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// ParseLevel accepts the operator-facing level names only.
func ParseLevel(value string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn":
		return LevelWarn, true
	default:
		return LevelInfo, false
	}
}

// Logger reports diagnostics. It is a thin façade over log/slog: slog owns
// leveling and dispatch, while handler below owns how a line reads on a terminal
// and removes both the configured values and the credential shapes. A bare
// credential carrying neither a key name nor a recognised shape still survives.
type Logger struct {
	inner *slog.Logger
}

func NewLogger(output io.Writer, redactor *Redactor) *Logger {
	return NewLeveledLogger(output, redactor, LevelInfo, nil)
}

func NewLeveledLogger(
	output io.Writer,
	redactor *Redactor,
	level Level,
	now func() time.Time,
) *Logger {
	if output == nil {
		output = io.Discard
	}
	if redactor == nil {
		redactor = NewRedactor(nil)
	}
	if now == nil {
		now = time.Now
	}
	return &Logger{
		inner: slog.New(&handler{
			output:   output,
			redactor: redactor,
			level:    level.slog(),
			now:      now,
			mu:       new(sync.Mutex),
		}),
	}
}

func DiscardLogger() *Logger { return NewLogger(io.Discard, nil) }

func (l *Logger) Enabled(level Level) bool {
	return l != nil && l.inner != nil && l.inner.Enabled(context.Background(), level.slog())
}

func (l *Logger) Debugf(format string, values ...any) { l.logf(LevelDebug, format, values...) }
func (l *Logger) Infof(format string, values ...any)  { l.logf(LevelInfo, format, values...) }
func (l *Logger) Warnf(format string, values ...any)  { l.logf(LevelWarn, format, values...) }

func (l *Logger) logf(level Level, format string, values ...any) {
	if !l.Enabled(level) {
		return
	}
	l.inner.Log(context.Background(), level.slog(), fmt.Sprintf(format, values...))
}

// handler renders one diagnostic per line for a human watching a terminal: the
// UTC timestamp, a padded level, and the message with every configured secret
// and every credential-shaped span removed. A run over a large queue takes hours
// and crosses midnight, so the date is part of the record rather than noise.
type handler struct {
	output   io.Writer
	redactor *Redactor
	level    slog.Level
	now      func() time.Time
	attrs    []slog.Attr
	// mu is shared by every clone so WithAttrs cannot split the lock that
	// serialises writes to one writer.
	mu *sync.Mutex
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *handler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	fmt.Fprintf(&line, "%s %-5s %s",
		h.now().UTC().Format("2006-01-02 15:04:05Z"),
		strings.ToUpper(levelName(record.Level)),
		record.Message,
	)
	for _, attr := range h.attrs {
		writeAttr(&line, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(&line, attr)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	// Scrubbed before terminalSafe, which deletes the newlines the key-name rules
	// use to tell a nested key from the value beside it.
	clean := h.redactor.Redact(ScrubSecrets(line.String()))
	_, err := io.WriteString(h.output, terminalSafe(clean)+"\n")
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

// WithGroup is not supported: this handler renders one flat line for a human,
// and a half-working implementation is worse than an honest refusal.
func (h *handler) WithGroup(string) slog.Handler { return h }

// writeAttr resolves a value before printing it, so a type that redacts itself
// through slog.LogValuer is given the chance to.
func writeAttr(line *strings.Builder, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	fmt.Fprintf(line, " %s=%v", attr.Key, attr.Value.Resolve())
}

func levelName(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "debug"
	case level >= slog.LevelWarn:
		return "warn"
	default:
		return "info"
	}
}

// terminalSafe applies Storable's rule with the output policy a single log line
// needs, so a hostile provider or registry cannot move the operator's cursor,
// rewrite what they have already read, or reorder it. A newline folds to a space
// rather than vanishing: one record still occupies one line either way, and a
// deletion would close the gap and rejoin whatever the newline held apart.
func terminalSafe(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || isLineSeparator(r):
			return ' '
		case isUnsafe(r):
			return -1
		default:
			return r
		}
	}, value)
}
