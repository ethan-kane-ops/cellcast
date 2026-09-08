package hub

import (
	"log/slog"
	"os"
)

// NewLogger builds the structured logger described by cfg.
//
// Output is stdout so it lands in whatever log pipeline the adopter already
// runs. cellcast ships no bespoke log sink.
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

// NewAuditLogger builds the logger the audit trail is written through.
//
// Separate from NewLogger, and deliberately not levelled by --log-level. Audit
// records are emitted at Info, so sharing a handler would mean a hub started
// with --log-level=error silently dropped its entire audit trail. A security
// log that a verbosity flag can silence is not a security log.
//
// Format still follows --log-format, so the one stream does not carry two
// shapes, and the destination is still stdout: cellcast ships no bespoke sink.
func NewAuditLogger(cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
