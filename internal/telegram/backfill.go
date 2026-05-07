package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
	"github.com/gotd/td/tg"
)

// backfillState is persisted between runs so crashes / restarts don't cause
// duplicate Gemini calls or duplicate notifications.
type backfillState struct {
	// Since pins the cut-off the state was produced for — changing BACKFILL_SINCE
	// invalidates the file and forces a fresh sweep.
	Since string `json:"since,omitempty"`
	// Channels maps channelID (as string so JSON doesn't rewrite int64 as float)
	// to the highest message ID already sent through the analyser.
	Channels map[string]int `json:"channels,omitempty"`
	Done     bool           `json:"done,omitempty"`
}

// Backfiller runs a one-shot historical sweep over source channels.
type Backfiller struct {
	api       *tg.Client
	analyzer  *gemini.Analyzer
	sender    *Sender
	matchLog  *MatchLog
	statePath string
	log       *slog.Logger
}

// NewBackfiller builds a Backfiller. matchLog may be nil to disable on-disk logging.
func NewBackfiller(
	api *tg.Client,
	analyzer *gemini.Analyzer,
	sender *Sender,
	matchLog *MatchLog,
	statePath string,
	log *slog.Logger,
) *Backfiller {
	return &Backfiller{
		api:       api,
		analyzer:  analyzer,
		sender:    sender,
		matchLog:  matchLog,
		statePath: statePath,
		log:       log,
	}
}

// Run processes each source channel's history in [since, startTime). Safe to
// call across restarts — already-completed runs are skipped and partial runs
// resume from the last persisted message ID.
func (b *Backfiller) Run(
	ctx context.Context,
	sources map[int64]ResolvedChannel,
	since time.Time,
) error {
	state, err := b.loadState()
	if err != nil {
		return err
	}
	sinceKey := since.UTC().Format("2006-01-02")
	if state.Since != sinceKey {
		// Different cut-off -> state is stale, start over.
		state = backfillState{Since: sinceKey, Channels: map[string]int{}}
	}
	if state.Channels == nil {
		state.Channels = map[string]int{}
	}
	if state.Done {
		b.log.Info("backfill: already completed for this cut-off — skipping",
			slog.String("since", state.Since))
		return nil
	}

	// Freeze "now" so messages that arrive while backfill is running are
	// handled exclusively by the live listener, not here.
	until := time.Now()

	var totalAnalysed, totalMatched int
	for id, resolved := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		analysed, matched, err := b.runChannel(ctx, id, resolved, since, until, &state)
		totalAnalysed += analysed
		totalMatched += matched
		if err != nil {
			// Don't abort the whole sweep on a single-channel error.
			b.log.Error("backfill: channel failed",
				slog.String("channel", resolved.Channel.Title),
				slog.Int64("channel_id", id),
				slog.Any("err", err),
			)
		}
	}

	state.Done = true
	if err := b.saveState(state); err != nil {
		b.log.Warn("backfill: final save failed", slog.Any("err", err))
	}
	b.log.Info("backfill: complete",
		slog.Int("analysed", totalAnalysed),
		slog.Int("matched", totalMatched),
	)
	return nil
}

// runChannel returns counters (analysed, matched) alongside any fatal error so
// the top-level Run can print an honest summary even when some channels failed.
func (b *Backfiller) runChannel(
	ctx context.Context,
	id int64,
	resolved ResolvedChannel,
	since, until time.Time,
	state *backfillState,
) (analysed, matched int, err error) {
	key := strconv.FormatInt(id, 10)
	lastProcessed := state.Channels[key]

	b.log.Info("backfill: fetching history",
		slog.String("channel", resolved.Channel.Title),
		slog.Int64("channel_id", id),
		slog.Time("since", since),
	)

	messages, err := FetchHistorySince(ctx, b.api, resolved.InputPeer, since, until)
	if err != nil {
		return 0, 0, fmt.Errorf("fetch: %w", err)
	}

	toProcess := 0
	for _, m := range messages {
		if m.ID > lastProcessed && strings.TrimSpace(m.Message) != "" {
			toProcess++
		}
	}
	b.log.Info("backfill: analysing",
		slog.String("channel", resolved.Channel.Title),
		slog.Int("total", len(messages)),
		slog.Int("pending", toProcess),
	)

	processed := 0
	for _, msg := range messages {
		if err := ctx.Err(); err != nil {
			return analysed, matched, err
		}
		if msg.ID <= lastProcessed {
			continue
		}
		text := strings.TrimSpace(msg.Message)
		if text == "" {
			state.Channels[key] = msg.ID
			continue
		}

		verdict, analyseErr := b.analyzer.Analyze(ctx, text)
		if analyseErr != nil {
			if errors.Is(analyseErr, context.Canceled) {
				return analysed, matched, analyseErr
			}
			b.log.Error("backfill: analyse failed",
				slog.Int("msg_id", msg.ID), slog.Any("err", analyseErr))
			// Don't bump lastProcessed on failure — we want to retry on next run.
			continue
		}
		analysed++

		// Debug-level trace of every verdict so LOG_LEVEL=debug reveals why
		// posts are being rejected. Matches are always logged at info below.
		b.log.Debug("backfill: verdict",
			slog.Int64("channel_id", id),
			slog.Int("msg_id", msg.ID),
			slog.Bool("match", verdict.Match),
			slog.String("reason", verdict.Reason),
		)

		if verdict.Match {
			matched++
			b.log.Info("backfill: match",
				slog.String("channel", resolved.Channel.Title),
				slog.Int("msg_id", msg.ID),
				slog.String("reason", verdict.Reason),
			)
			// Persist before the Telegram send so a send failure cannot
			// silently drop the match.
			if err := b.matchLog.Append(MatchRecord{
				ChannelID:    id,
				ChannelTitle: resolved.Channel.Title,
				MessageID:    msg.ID,
				Reason:       verdict.Reason,
				Link:         BuildPostLink(resolved.Channel, msg.ID),
				Source:       "backfill",
			}); err != nil {
				b.log.Warn("backfill: matchlog append failed",
					slog.Int("msg_id", msg.ID), slog.Any("err", err))
			}
			if err := b.sender.Notify(ctx, resolved.Channel, msg.ID, verdict.Reason, text); err != nil {
				b.log.Error("backfill: notify failed",
					slog.Int("msg_id", msg.ID), slog.Any("err", err))
			}
		}

		state.Channels[key] = msg.ID
		processed++

		// Flush progress periodically so a crash doesn't wipe the run.
		if processed%10 == 0 {
			if err := b.saveState(*state); err != nil {
				b.log.Warn("backfill: save state failed", slog.Any("err", err))
			}
		}
	}

	b.log.Info("backfill: channel done",
		slog.String("channel", resolved.Channel.Title),
		slog.Int("analysed", analysed),
		slog.Int("matched", matched),
	)

	return analysed, matched, b.saveState(*state)
}

func (b *Backfiller) loadState() (backfillState, error) {
	var s backfillState
	raw, err := os.ReadFile(b.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("read backfill state: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("parse backfill state: %w", err)
	}
	return s, nil
}

func (b *Backfiller) saveState(s backfillState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.statePath, raw, 0o600)
}
