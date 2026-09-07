package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

// seenTTL bounds how long a post fingerprint suppresses reposts. Vacancies get
// re-published after a month or so; treating those as new is correct.
const seenTTL = 30 * 24 * time.Hour

// stateFlushEvery persists the cursor mid-run so an interrupted poll resumes
// where it stopped instead of re-analysing everything.
const stateFlushEvery = 10

// idlePause is how long the poller waits before re-checking the channels when
// a pass found nothing. It keeps a run doing real work for its whole budget
// instead of exiting immediately and leaving the caller to idle, and it means
// a post published mid-run is delivered within a couple of minutes.
const idlePause = 2 * time.Minute

// pollState is the cursor persisted between runs.
type pollState struct {
	// Channels maps channelID (as string so JSON doesn't rewrite int64 as
	// float) to the highest message ID already analysed.
	Channels map[string]int `json:"channels"`
	// Seen maps a post fingerprint to the unix time it was first analysed.
	Seen map[string]int64 `json:"seen"`
}

// Mark implements Seen.
func (s *pollState) Mark(fingerprint string) bool {
	if _, ok := s.Seen[fingerprint]; ok {
		return true
	}
	s.Seen[fingerprint] = time.Now().UTC().Unix()
	return false
}

func (s *pollState) prune(now time.Time) {
	cutoff := now.Add(-seenTTL).Unix()
	for fp, ts := range s.Seen {
		if ts < cutoff {
			delete(s.Seen, fp)
		}
	}
}

// Poller runs one pass over the source channels and exits. This is the mode
// used by the scheduled deploy: there is no long-lived process to keep alive.
type Poller struct {
	api       *tg.Client
	proc      *Processor
	statePath string

	// bootstrap is the date a channel is read from the first time it is
	// polled, before it has a cursor. Zero means "start from now".
	bootstrap time.Time

	log *slog.Logger
}

// NewPoller builds a Poller.
func NewPoller(
	api *tg.Client,
	proc *Processor,
	statePath string,
	bootstrap time.Time,
	log *slog.Logger,
) *Poller {
	return &Poller{api: api, proc: proc, statePath: statePath, bootstrap: bootstrap, log: log}
}

// pendingPost is one message waiting to be classified, tagged with the channel
// it came from.
type pendingPost struct {
	channelID int64
	channel   *tg.Channel
	msg       *tg.Message
}

// Run polls every source channel once and returns when the work is done or the
// deadline passes. Exceeding the deadline is not an error: the cursor is saved
// and the next scheduled run continues from there, which is how a multi-week
// backlog is consumed across several short jobs.
func (p *Poller) Run(
	ctx context.Context,
	sources map[int64]ResolvedChannel,
	deadline time.Time,
) error {
	state, err := p.loadState()
	if err != nil {
		return err
	}
	state.prune(time.Now())
	p.proc.SetSeen(state)

	// Keep polling for the whole budget rather than exiting on the first empty
	// pass: the caller schedules runs, not passes, and a post published a
	// minute after the run started should not wait for the next one.
	for {
		done, err := p.pass(ctx, sources, state, deadline)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idlePause):
		}
	}
}

// pass runs one fetch-and-analyse cycle. done is true when the time budget is
// spent and the caller should stop.
func (p *Poller) pass(
	ctx context.Context,
	sources map[int64]ResolvedChannel,
	state *pollState,
	deadline time.Time,
) (bool, error) {
	if !deadline.IsZero() && time.Now().Add(idlePause).After(deadline) {
		// Not enough budget left for another meaningful cycle.
		return true, p.saveState(state)
	}

	pending, err := p.collect(ctx, sources, state)
	if err != nil {
		return true, err
	}
	if len(pending) == 0 {
		p.log.Info("poll: nothing new")
		return false, p.saveState(state)
	}

	// Process oldest-first across all channels so a truncated run spends its
	// budget evenly instead of finishing channel #1 and never reaching #16.
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].msg.Date != pending[j].msg.Date {
			return pending[i].msg.Date < pending[j].msg.Date
		}
		return pending[i].msg.ID < pending[j].msg.ID
	})
	p.log.Info("poll: analysing", slog.Int("pending", len(pending)))

	counts := map[Outcome]int{}
	processed := 0
	budgetSpent := false

	for _, post := range pending {
		if err := ctx.Err(); err != nil {
			_ = p.saveState(state)
			return true, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			budgetSpent = true
			break
		}

		outcome, err := p.proc.Process(ctx, post.channel, post.msg.ID,
			strings.TrimSpace(post.msg.Message), "poll")
		if err != nil {
			_ = p.saveState(state)
			return true, err
		}
		counts[outcome]++

		// An operational failure must not advance the cursor — the post is
		// retried on the next run.
		if outcome != OutcomeError {
			p.advance(state, post.channelID, post.msg.ID)
		}

		processed++
		if processed%stateFlushEvery == 0 {
			if err := p.saveState(state); err != nil {
				p.log.Warn("poll: save state failed", slog.Any("err", err))
			}
		}
	}

	if err := p.saveState(state); err != nil {
		return true, err
	}

	p.log.Info("poll: pass done",
		slog.Int("processed", processed),
		slog.Int("remaining", len(pending)-processed),
		slog.Int("matched", counts[OutcomeMatched]),
		slog.Int("rejected", counts[OutcomeRejected]),
		slog.Int("skipped", counts[OutcomeSkipped]),
		slog.Int("duplicate", counts[OutcomeDuplicate]),
		slog.Int("errors", counts[OutcomeError]),
		slog.Bool("budget_exhausted", budgetSpent),
	)
	if budgetSpent {
		p.log.Info("poll: time budget spent — the next run continues from the saved cursor")
	}
	return budgetSpent, nil
}

// collect fetches the unprocessed posts of every source channel. A channel
// with no cursor yet is read from the bootstrap date; afterwards the cursor is
// cheaper and exact.
func (p *Poller) collect(
	ctx context.Context,
	sources map[int64]ResolvedChannel,
	state *pollState,
) ([]pendingPost, error) {
	var pending []pendingPost

	for _, id := range sortedIDs(sources) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resolved := sources[id]
		lastSeen := state.Channels[strconv.FormatInt(id, 10)]

		var (
			msgs []*tg.Message
			err  error
		)
		if lastSeen > 0 {
			msgs, err = FetchHistoryAfter(ctx, p.api, resolved.InputPeer, lastSeen)
		} else {
			since := p.bootstrap
			if since.IsZero() {
				since = time.Now()
			}
			msgs, err = FetchHistorySince(ctx, p.api, resolved.InputPeer, since, time.Now())
		}
		if err != nil {
			// One unreachable channel must not sink the whole run.
			p.log.Error("poll: fetch failed",
				slog.String("channel", resolved.Channel.Title),
				slog.Int64("channel_id", id),
				slog.Any("err", err))
			continue
		}

		for _, m := range msgs {
			if strings.TrimSpace(m.Message) == "" {
				// Media without a caption carries nothing to classify, but the
				// cursor must still move past it.
				p.advance(state, id, m.ID)
				continue
			}
			pending = append(pending, pendingPost{channelID: id, channel: resolved.Channel, msg: m})
		}

		p.log.Info("poll: fetched",
			slog.String("channel", resolved.Channel.Title),
			slog.Int("new", len(msgs)),
			slog.Int("cursor", lastSeen))
	}
	return pending, nil
}

// advance moves a channel cursor forward, never backwards.
func (p *Poller) advance(state *pollState, channelID int64, msgID int) {
	key := strconv.FormatInt(channelID, 10)
	if msgID > state.Channels[key] {
		state.Channels[key] = msgID
	}
}

func (p *Poller) loadState() (*pollState, error) {
	s := &pollState{Channels: map[string]int{}, Seen: map[string]int64{}}

	raw, err := os.ReadFile(p.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read poll state: %w", err)
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("parse poll state: %w", err)
	}
	if s.Channels == nil {
		s.Channels = map[string]int{}
	}
	if s.Seen == nil {
		s.Seen = map[string]int64{}
	}
	return s, nil
}

func (p *Poller) saveState(s *pollState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: a job killed mid-write must not leave truncated JSON
	// that fails the next run's parse.
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write poll state: %w", err)
	}
	return os.Rename(tmp, p.statePath)
}

func sortedIDs(sources map[int64]ResolvedChannel) []int64 {
	ids := make([]int64, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
