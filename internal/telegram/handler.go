package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
)

// Handler wires an incoming Telegram channel update through the Gemini filter
// and, if the post matches, forwards it to the destination chat.
type Handler struct {
	cfg      *config.Config
	analyzer *gemini.Analyzer
	sender   *Sender
	matchLog *MatchLog
	log      *slog.Logger
}

// NewHandler builds a Handler. matchLog may be nil to disable on-disk logging.
func NewHandler(cfg *config.Config, analyzer *gemini.Analyzer, sender *Sender, matchLog *MatchLog, log *slog.Logger) *Handler {
	return &Handler{
		cfg:      cfg,
		analyzer: analyzer,
		sender:   sender,
		matchLog: matchLog,
		log:      log,
	}
}

// OnNewChannelMessage is wired into tg.UpdateDispatcher.OnNewChannelMessage.
// Returning an error aborts update processing, so we swallow operational errors
// (Gemini failures, send failures) and only log them.
func (h *Handler) OnNewChannelMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
	msg, ok := u.Message.(*tg.Message)
	if !ok {
		// Service messages (pin/title change/etc.) — ignore.
		return nil
	}
	peer, ok := msg.PeerID.(*tg.PeerChannel)
	if !ok {
		return nil
	}
	if _, watched := h.cfg.SourceChannels[peer.ChannelID]; !watched {
		return nil
	}

	logger := h.log.With(
		slog.Int64("channel_id", peer.ChannelID),
		slog.Int("msg_id", msg.ID),
	)

	text := strings.TrimSpace(msg.Message)
	if text == "" {
		// Pure media without caption — nothing to classify.
		logger.Debug("skip: empty text")
		return nil
	}

	if h.cfg.MaxMessageAge > 0 && msg.Date > 0 {
		age := time.Since(time.Unix(int64(msg.Date), 0))
		if age > h.cfg.MaxMessageAge {
			logger.Debug("skip: message too old", slog.Duration("age", age))
			return nil
		}
	}

	logger.Info("analysing post", slog.Int("chars", len(text)))

	verdict, err := h.analyzer.Analyze(ctx, text)
	if err != nil {
		logger.Error("gemini analyse failed", slog.Any("err", err))
		return nil
	}

	logger.Info("verdict",
		slog.Bool("match", verdict.Match),
		slog.String("reason", verdict.Reason),
	)

	if !verdict.Match {
		return nil
	}

	channel, ok := e.Channels[peer.ChannelID]
	if !ok {
		logger.Warn("match but channel entity missing from update; cannot build link")
		return nil
	}

	// Persist the match BEFORE attempting the Telegram send — if the send
	// fails, the user still has an on-disk record to recover from.
	if err := h.matchLog.Append(MatchRecord{
		ChannelID:    peer.ChannelID,
		ChannelTitle: channel.Title,
		MessageID:    msg.ID,
		Reason:       verdict.Reason,
		Link:         BuildPostLink(channel, msg.ID),
		Source:       "live",
	}); err != nil {
		logger.Warn("matchlog append failed", slog.Any("err", err))
	}

	if err := h.sender.Notify(ctx, channel, msg.ID, verdict.Reason, text); err != nil {
		logger.Error("notify failed", slog.Any("err", err))
		return nil
	}
	logger.Info("match forwarded")
	return nil
}
