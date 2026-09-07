package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
)

// Handler wires a live Telegram channel update into the shared processor.
// Used by the long-running mode; the scheduled deploy uses Poller instead.
type Handler struct {
	cfg  *config.Config
	proc *Processor
	log  *slog.Logger
}

// NewHandler builds a Handler.
func NewHandler(cfg *config.Config, proc *Processor, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, proc: proc, log: log}
}

// OnNewChannelMessage is wired into tg.UpdateDispatcher.OnNewChannelMessage.
// Returning an error aborts update processing, so operational errors are
// swallowed by the processor and only logged.
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

	channel, ok := e.Channels[peer.ChannelID]
	if !ok {
		logger.Warn("channel entity missing from update; cannot build link")
		return nil
	}

	if _, err := h.proc.Process(ctx, channel, msg.ID, text, "live"); err != nil {
		logger.Debug("processing aborted", slog.Any("err", err))
	}
	return nil
}
