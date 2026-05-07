// Command tg-vacancy-filter runs the Telegram userbot that filters channel
// posts through Gemini and forwards matches to a destination chat.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/assylkhan/tg-vacancy-filter/internal/app"
	"github.com/assylkhan/tg-vacancy-filter/internal/config"
)

func main() {
	// Load .env BEFORE parsing LOG_LEVEL — otherwise the logger is initialised
	// from an empty process env and LOG_LEVEL=debug in .env gets silently
	// ignored. godotenv.Load never overwrites existing env vars, so a second
	// call inside config.Load is a no-op.
	_ = godotenv.Load()

	// Derive the log level from env BEFORE config.Load runs, so even config
	// failures are logged at the right verbosity.
	lvl := config.ParseLogLevelEnv()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
