package agent

import (
	"log/slog"
	"os"
)

// NewLogger builds the structured logger described by cfg.
//
// A copy of the hub's rather than a shared helper: importing internal/hub here
// would link the broker into the agent and break the boundary the three-binary
// split exists to enforce (docs/architecture.md ADR-007). Thirty lines of
// duplication is the cheaper side of that trade.
func NewLogger(cfg Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
