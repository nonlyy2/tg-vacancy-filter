// Package app wires config, Telegram MTProto and Gemini into a single Run func.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
	tgclient "github.com/assylkhan/tg-vacancy-filter/internal/telegram"
)

// Run boots the userbot and blocks until ctx is cancelled or a fatal error
// occurs. Ctrl-C / SIGTERM should cancel ctx to trigger a graceful shutdown.
func Run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log.Info("config loaded",
		slog.Int("watched_channels", len(cfg.SourceChannels)),
		slog.String("destination", cfg.Destination),
		slog.String("model", cfg.GeminiModel),
	)

	analyzer, err := gemini.New(ctx, cfg.GeminiAPIKey, cfg.GeminiModel, cfg.GeminiRPM)
	if err != nil {
		return fmt.Errorf("init gemini: %w", err)
	}
	analyzer.SetRetryHook(func(attempt int, wait time.Duration, err error) {
		log.Warn("gemini: quota hit — backing off",
			slog.Int("attempt", attempt),
			slog.Duration("sleep", wait),
		)
	})
	log.Info("gemini ready",
		slog.String("model", cfg.GeminiModel),
		slog.Int("rpm_cap", cfg.GeminiRPM),
	)
	defer func() {
		if cerr := analyzer.Close(); cerr != nil {
			log.Warn("gemini close failed", slog.Any("err", cerr))
		}
	}()

	// The dispatcher must be constructed before the client because the client
	// takes a reference to it through Options.UpdateHandler.
	dispatcher := tg.NewUpdateDispatcher()

	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath},
		UpdateHandler:  dispatcher,
		Device: telegram.DeviceConfig{
			DeviceModel:    "Samsung Galaxy S23",
			SystemVersion:  "Android 13.0",
			AppVersion:     "10.6.1",
			SystemLangCode: "en",
			LangCode:       "en",
		},
	})

	return client.Run(ctx, func(ctx context.Context) error {
		// Authenticate on first run; subsequent runs reuse the stored session.
		flow := auth.NewFlow(
			tgclient.NewTerminalAuth(cfg.Phone),
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("authenticate: %w", err)
		}

		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}
		log.Info("logged in",
			slog.Int64("user_id", self.ID),
			slog.String("username", self.Username),
			slog.String("first_name", self.FirstName),
		)

		api := tg.NewClient(client)
		mgr := peers.Options{}.Build(api)

		destPeer, err := resolveDestination(ctx, api, mgr, cfg, log)
		if err != nil {
			return fmt.Errorf("resolve destination %q: %w", cfg.Destination, err)
		}
		log.Info("destination resolved")

		sender := tgclient.NewSender(api, destPeer)
		matchLog := tgclient.NewMatchLog(cfg.MatchLogPath)
		if matchLog != nil {
			log.Info("match log enabled", slog.String("path", cfg.MatchLogPath))
		}

		// History backfill (optional). Must happen BEFORE we attach the live
		// handler — the fetcher deliberately caps its `until` at time.Now() so
		// messages that arrive during the sweep go through the live path only.
		if !cfg.BackfillSince.IsZero() {
			sources, missing, err := tgclient.ResolveChannelPeers(ctx, api, cfg.SourceChannels)
			if err != nil {
				return fmt.Errorf("resolve source channels: %w", err)
			}
			for _, id := range missing {
				log.Warn("backfill: source channel not found in dialogs (not joined?)",
					slog.Int64("channel_id", id))
			}
			if len(sources) > 0 {
				bf := tgclient.NewBackfiller(api, analyzer, sender, matchLog, cfg.BackfillStatePath, log)
				if err := bf.Run(ctx, sources, cfg.BackfillSince); err != nil {
					log.Error("backfill failed", slog.Any("err", err))
				}
			}
		}

		handler := tgclient.NewHandler(cfg, analyzer, sender, matchLog, log)
		dispatcher.OnNewChannelMessage(handler.OnNewChannelMessage)

		log.Info("listening for channel posts — press Ctrl+C to stop")

		<-ctx.Done()
		log.Info("shutdown signal received")
		// Returning ctx.Err() propagates the cancellation but is not a failure.
		if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
}

// resolveDestination turns the DESTINATION config value into a concrete
// InputPeer usable for messages.sendMessage. Supported shapes:
//
//   - "me" / "self"            -> Saved Messages
//   - "https://t.me/+<hash>"   -> private invite link (auto-joins if not a member)
//   - "https://t.me/joinchat/<hash>" (same, legacy form)
//   - "<username>"             -> public channel / user (no @ required)
func resolveDestination(
	ctx context.Context,
	api *tg.Client,
	mgr *peers.Manager,
	cfg *config.Config,
	log *slog.Logger,
) (tg.InputPeerClass, error) {
	if cfg.IsSelfDestination() {
		self, err := mgr.Self(ctx)
		if err != nil {
			return nil, fmt.Errorf("self: %w", err)
		}
		return self.InputPeer(), nil
	}
	if hash, ok := tgclient.ParseInviteHash(cfg.Destination); ok {
		return tgclient.ResolveInvite(ctx, api, hash, log)
	}
	p, err := mgr.Resolve(ctx, cfg.Destination)
	if err != nil {
		return nil, fmt.Errorf("resolve username %q: %w", cfg.Destination, err)
	}
	return p.InputPeer(), nil
}
