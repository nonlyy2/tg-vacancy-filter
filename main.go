// Command tg-vacancy-filter runs the Telegram userbot that scores channel
// posts against a candidate profile and forwards the matches.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/assylkhan/tg-vacancy-filter/internal/app"
	"github.com/assylkhan/tg-vacancy-filter/internal/config"
)

func main() {
	once := flag.Bool("once", false,
		"poll every source channel once and exit (used by the scheduled deploy)")
	doctor := flag.Bool("doctor", false,
		"run preflight checks (api key, account, channels, destination) and exit")
	doctorSend := flag.Bool("doctor-send", false,
		"with -doctor: also send a test message to DESTINATION to prove write access")
	flag.Parse()

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

	var err error
	switch {
	case *doctor:
		err = app.Doctor(ctx, log, *doctorSend)
	case *once:
		err = app.RunOnce(ctx, log)
	default:
		err = app.Run(ctx, log)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
