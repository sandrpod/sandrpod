// Package logging configures the process-wide slog logger and routes the
// standard library log package onto it, so every line — a structured slog call
// or a legacy log.Printf — shares one format (text for humans, JSON for log
// aggregators) and one level filter.
package logging

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Setup installs a process-wide slog logger driven by environment:
//
//	SANDRPOD_LOG_FORMAT = text (default) | json
//	SANDRPOD_LOG_LEVEL  = debug | info (default) | warn | error
//
// It also redirects the standard log package through slog so existing log.Printf
// calls inherit the same handler and destination. Returns the logger.
func Setup() *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("SANDRPOD_LOG_LEVEL"))}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("SANDRPOD_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	// Bridge the std log package: each log.Printf becomes an slog record at info
	// so format and level stay uniform. Drop log's own timestamp — slog adds one.
	log.SetFlags(0)
	log.SetOutput(bridgeWriter{})
	return logger
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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

// bridgeWriter forwards each standard-log line to slog at info level.
type bridgeWriter struct{}

func (bridgeWriter) Write(p []byte) (int, error) {
	slog.Default().Log(context.Background(), slog.LevelInfo, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
