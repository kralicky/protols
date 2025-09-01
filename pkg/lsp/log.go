package lsp

import (
	"log/slog"
	"strings"
	"sync/atomic"
)

var GlobalAtomicLeveler = &AtomicLeveler{}

type AtomicLeveler struct {
	level atomic.Int32
}

func (a *AtomicLeveler) SetLevel(level slog.Level) {
	a.level.Store(int32(level))
}

// Level implements slog.Leveler.
func (a *AtomicLeveler) Level() slog.Level {
	return slog.Level(a.level.Load())
}

var _ slog.Leveler = (*AtomicLeveler)(nil)

func ParseLogLevel(level string) (slog.Level, bool) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "err", "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}
