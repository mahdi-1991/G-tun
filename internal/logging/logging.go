// Package logging configures the process-wide logger.
package logging

import (
	"log/slog"
	"os"
)

// Init installs a text logger writing to stderr, where systemd collects it.
func Init(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
