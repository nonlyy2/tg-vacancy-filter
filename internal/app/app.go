// Package app wires config, Telegram MTProto and Gemini into runnable modes.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
	tgclient "github.com/assylkhan/tg-vacancy-filter/internal/telegram"
)

// env holds everything built before the MTProto connection opens.
type env struct {
	cfg           *config.Config
	log           *slog.Logger
	analyzer      *gemini.Analyzer
	client        *telegram.Client
	sessionSource string
}

// session bundles the per-connection objects every mode needs.
type session struct {
	api  *tg.Client
	mgr  *peers.Manager
	self *tg.User
	proc *tgclient.Processor
	send *tgclient.Sender
}

// Run boots the userbot in live mode and blocks until ctx is cancelled.
// Used on an always-on host; the scheduled deploy calls RunOnce instead.
func Run(ctx context.Context, log *slog.Logger) error {
	// The dispatcher must exist before the client, which takes a reference to
	// it through Options.UpdateHandler.
	dispatcher := tg.NewUpdateDispatcher()

	e, err := setup(ctx, log, dispatcher)
	if err != nil {
		return err
	}
	defer e.close()

	return e.client.Run(ctx, func(ctx context.Context) error {
		s, err := e.connect(ctx)
		if err != nil {
			return err
		}

		handler := tgclient.NewHandler(e.cfg, s.proc, log)
		dispatcher.OnNewChannelMessage(handler.OnNewChannelMessage)

		log.Info("listening for channel posts — press Ctrl+C to stop")

		<-ctx.Done()
		log.Info("shutdown signal received")
		if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
}

// RunOnce polls every source channel once and exits. This is the mode the
// scheduled workflow runs: no long-lived process, no open laptop.
func RunOnce(ctx context.Context, log *slog.Logger, dryRun bool) error {
	e, err := setup(ctx, log, nil)
	if err != nil {
		return err
	}
	defer e.close()

	return e.client.Run(ctx, func(ctx context.Context) error {
		s, err := e.connect(ctx)
		if err != nil {
			return err
		}
		if dryRun {
			s.proc.SetDryRun(true)
			log.Warn("dry run: verdicts are logged, nothing is sent")
		}

		sources, missing, err := tgclient.ResolveChannelPeers(ctx, s.api, e.cfg.SourceChannels)
		if err != nil {
			return fmt.Errorf("resolve source channels: %w", err)
		}
		for _, id := range missing {
			log.Warn("source channel not found in dialogs (account not joined?)",
				slog.Int64("channel_id", id))
		}
		if len(sources) == 0 {
			return errors.New("none of the configured source channels are reachable by this account")
		}

		deadline := time.Now().Add(e.cfg.PollMaxRuntime)
		poller := tgclient.NewPoller(s.api, s.proc, e.cfg.PollStatePath, e.cfg.PollBootstrapSince, log)
		return poller.Run(ctx, sources, deadline)
	})
}

// setup loads config and the candidate profile, builds the analyser and the
// MTProto client. updates may be nil, which puts the client in no-updates
// mode — correct for the poll and doctor paths.
func setup(ctx context.Context, log *slog.Logger, updates telegram.UpdateHandler) (*env, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	profile, err := loadProfile(cfg.ProfilePath)
	if err != nil {
		return nil, err
	}

	log.Info("config loaded",
		slog.Int("watched_channels", len(cfg.SourceChannels)),
		slog.String("destination", cfg.Destination),
		slog.String("model", cfg.GeminiModel),
		slog.Int("threshold", cfg.MatchThreshold),
		slog.String("profile", cfg.ProfilePath),
	)

	analyzer, err := gemini.New(ctx, cfg.GeminiAPIKey, cfg.GeminiModel,
		cfg.GeminiModelFallback, profile, cfg.GeminiRPM)
	if err != nil {
		return nil, fmt.Errorf("init gemini: %w", err)
	}
	analyzer.SetRetryHook(func(attempt int, wait time.Duration, _ error) {
		log.Warn("gemini: quota hit — backing off",
			slog.Int("attempt", attempt),
			slog.Duration("sleep", wait),
		)
	})
	analyzer.SetFallbackHook(func(from, to string) {
		log.Warn("gemini: primary model out of quota — switching for the rest of this run",
			slog.String("from", from),
			slog.String("to", to),
		)
	})

	storage, source, err := tgclient.BuildSessionStorage(ctx, cfg)
	if err != nil {
		_ = analyzer.Close()
		return nil, err
	}
	log.Info("session source", slog.String("from", source))

	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  updates,
		Device: telegram.DeviceConfig{
			DeviceModel:    "Samsung Galaxy S23",
			SystemVersion:  "Android 13.0",
			AppVersion:     "10.6.1",
			SystemLangCode: "en",
			LangCode:       "en",
		},
	})

	return &env{
		cfg:           cfg,
		log:           log,
		analyzer:      analyzer,
		client:        client,
		sessionSource: source,
	}, nil
}

func (e *env) close() {
	if err := e.analyzer.Close(); err != nil {
		e.log.Warn("gemini close failed", slog.Any("err", err))
	}
}

// connect authenticates and assembles the per-connection objects. Must be
// called inside client.Run.
func (e *env) connect(ctx context.Context) (*session, error) {
	flow := auth.NewFlow(tgclient.NewTerminalAuth(e.cfg.Phone), auth.SendCodeOptions{})
	if err := e.client.Auth().IfNecessary(ctx, flow); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	self, err := e.client.Self(ctx)
	if err != nil {
		return nil, fmt.Errorf("self: %w", err)
	}
	e.log.Info("logged in",
		slog.Int64("user_id", self.ID),
		slog.String("username", self.Username),
		slog.String("first_name", self.FirstName),
	)

	api := tg.NewClient(e.client)
	mgr := peers.Options{}.Build(api)

	destPeer, err := resolveDestination(ctx, api, mgr, e.cfg, e.log)
	if err != nil {
		return nil, fmt.Errorf("resolve destination %q: %w", e.cfg.Destination, err)
	}
	e.log.Info("destination resolved")

	sender := tgclient.NewSender(api, destPeer)
	matchLog := tgclient.NewMatchLog(e.cfg.MatchLogPath)
	if matchLog != nil {
		e.log.Info("match log enabled", slog.String("path", e.cfg.MatchLogPath))
	}

	proc := tgclient.NewProcessor(e.analyzer, sender, matchLog, nil, e.cfg.MatchThreshold, e.log)

	return &session{api: api, mgr: mgr, self: self, proc: proc, send: sender}, nil
}

// loadProfile reads the candidate description injected into the prompt.
func loadProfile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read candidate profile %q "+
			"(set PROFILE_PATH or create the file): %w", path, err)
	}
	profile := strings.TrimSpace(string(raw))
	if profile == "" {
		return "", fmt.Errorf("candidate profile %q is empty", path)
	}
	return profile, nil
}

// resolveDestination turns the DESTINATION config value into a concrete
// InputPeer usable for messages.sendMessage. Supported shapes:
//
//   - "me" / "self"            -> Saved Messages
//   - "-1004390136284" / "4390136284" -> channel by numeric id, resolved
//     through the account's dialogs (a private channel has no username)
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
	if id, ok := config.ParseChannelID(cfg.Destination); ok {
		return tgclient.ResolveChannelByID(ctx, api, id)
	}
	p, err := mgr.Resolve(ctx, cfg.Destination)
	if err != nil {
		return nil, fmt.Errorf("resolve username %q: %w", cfg.Destination, err)
	}
	return p.InputPeer(), nil
}
