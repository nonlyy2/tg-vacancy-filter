package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/filter"
	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
)

// Outcome is what happened to one post. Used for the per-run summary.
type Outcome string

const (
	OutcomeSkipped   Outcome = "skipped"   // prefilter said it is not a vacancy
	OutcomeDuplicate Outcome = "duplicate" // same vacancy already seen
	OutcomeRejected  Outcome = "rejected"  // scored below the threshold
	OutcomeMatched   Outcome = "matched"
	OutcomeError     Outcome = "error"
)

// Seen tracks fingerprints of posts already analysed, so a vacancy
// cross-posted to several channels costs one model call instead of five.
type Seen interface {
	// Mark records the fingerprint and reports whether it was already present.
	Mark(fingerprint string) bool
}

// Processor runs a single post through prefilter, dedup, the model and the
// score threshold. Shared by the poller and the live update handler.
type Processor struct {
	analyzer  *gemini.Analyzer
	sender    *Sender
	matchLog  *MatchLog
	seen      Seen
	threshold int
	log       *slog.Logger

	// dryRun classifies and logs but never sends. Used to calibrate the
	// profile and the threshold against real traffic without filling the
	// destination chat with test output.
	dryRun bool
}

// NewProcessor builds a Processor. matchLog and seen may be nil.
func NewProcessor(
	analyzer *gemini.Analyzer,
	sender *Sender,
	matchLog *MatchLog,
	seen Seen,
	threshold int,
	log *slog.Logger,
) *Processor {
	return &Processor{
		analyzer:  analyzer,
		sender:    sender,
		matchLog:  matchLog,
		seen:      seen,
		threshold: threshold,
		log:       log,
	}
}

// SetDryRun disables sending. Verdicts are still logged and written to the
// match log, so a run can be scored without notifying anyone.
func (p *Processor) SetDryRun(dry bool) {
	p.dryRun = dry
}

// Process classifies one post and forwards it when it clears the threshold.
// Operational failures (model errors, send errors) are logged and reported as
// OutcomeError; only context cancellation comes back as an error, because the
// caller's loop must stop on it.
func (p *Processor) Process(
	ctx context.Context,
	ch *tg.Channel,
	msgID int,
	text string,
	source string,
) (Outcome, error) {
	logger := p.log.With(slog.Int64("channel_id", ch.ID), slog.Int("msg_id", msgID))

	if keep, why := filter.Keep(text); !keep {
		logger.Debug("prefilter: skip", slog.String("why", why))
		return OutcomeSkipped, nil
	}

	if p.seen != nil && p.seen.Mark(filter.Fingerprint(text)) {
		logger.Debug("dedup: already analysed this vacancy")
		return OutcomeDuplicate, nil
	}

	verdict, err := p.analyzer.Analyze(ctx, text)
	if err != nil {
		if ctx.Err() != nil {
			return OutcomeError, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return OutcomeError, err
		}
		logger.Error("analyse failed", slog.Any("err", err))
		return OutcomeError, nil
	}

	attrs := []any{
		slog.Int("score", verdict.Score),
		slog.String("role", verdict.Role),
		slog.String("remote", verdict.Remote),
		slog.String("summary", verdict.Summary),
	}
	if p.dryRun {
		// During calibration the verdict is only useful next to the text it
		// was made from — that is how a misread format or seniority is spotted.
		attrs = append(attrs, slog.String("post", excerpt(text, 700)))
	}
	logger.Debug("verdict", attrs...)

	if !verdict.Matches(p.threshold) {
		logger.Debug("below threshold",
			slog.String("channel", ch.Title),
			slog.Int("score", verdict.Score),
			slog.String("role", verdict.Role),
			slog.String("remote", verdict.Remote),
		)
		return OutcomeRejected, nil
	}

	logger.Info("match",
		slog.String("channel", ch.Title),
		slog.Int("score", verdict.Score),
		slog.String("role", verdict.Role),
		slog.String("remote", verdict.Remote),
		slog.String("link", BuildPostLink(ch, msgID)),
	)

	// Persist before the Telegram send so a send failure cannot silently drop
	// the match.
	if err := p.matchLog.Append(MatchRecord{
		ChannelID:    ch.ID,
		ChannelTitle: ch.Title,
		MessageID:    msgID,
		Score:        verdict.Score,
		Role:         verdict.Role,
		Remote:       verdict.Remote,
		Summary:      verdict.Summary,
		Link:         BuildPostLink(ch, msgID),
		Source:       source,
	}); err != nil {
		logger.Warn("matchlog append failed", slog.Any("err", err))
	}

	if p.dryRun {
		return OutcomeMatched, nil
	}

	if err := p.sender.Notify(ctx, ch, msgID, verdict, text); err != nil {
		if ctx.Err() != nil {
			return OutcomeError, ctx.Err()
		}
		logger.Error("notify failed", slog.Any("err", err))
		return OutcomeError, nil
	}
	return OutcomeMatched, nil
}

// SetSeen attaches the dedup store. The poller calls this once its state file
// is loaded, since the store and the cursor live in the same file.
func (p *Processor) SetSeen(s Seen) {
	p.seen = s
}

// excerpt shortens post text for calibration logs, counting runes so Cyrillic
// is not cut mid-character.
func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
