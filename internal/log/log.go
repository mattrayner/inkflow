package log

import (
	"log/slog"
	"os"
	"strings"
)

func New() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelFromEnv()}))
}

func levelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SLOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
