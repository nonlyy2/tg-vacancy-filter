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
	"sync"
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

// pollState is the cursor persisted between runs. Channels are analysed
// concurrently, so every accessor takes mu — including Mark, which the
// processor calls from whichever worker goroutine owns the post.
type pollState struct {
	mu sync.Mutex

	// Channels maps channelID (as string so JSON doesn't rewrite int64 as
	// float) to the highest message ID already analysed.
	Channels map[string]int `json:"channels"`
	// Seen maps a post fingerprint to the unix time it was first analysed.
	Seen map[string]int64 `json:"seen"`
}

// Mark implements Seen.
func (s *pollState) Mark(fingerprint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Seen[fingerprint]; ok {
		return true
	}
	s.Seen[fingerprint] = time.Now().UTC().Unix()
	return false
}

// advance moves a channel cursor forward, never backwards.
func (s *pollState) advance(channelID int64, msgID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.FormatInt(channelID, 10)
	if msgID > s.Channels[key] {
		s.Channels[key] = msgID
	}
}

// cursor reports the highest message ID already analysed for a channel.
func (s *pollState) cursor(channelID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Channels[strconv.FormatInt(channelID, 10)]
}

// marshal serialises the state under the lock, so a save concurrent with a
// worker's cursor update cannot observe a half-written map.
func (s *pollState) marshal() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.MarshalIndent(s, "", "  ")
}

func (s *pollState) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

	// workers bounds how many channels are analysed at once. The model's
	// per-call latency swings by an order of magnitude (6s to 42s observed on
	// the same model), and strictly sequential processing lets one slow call
	// stall the whole run. The shared rate limiter still caps total RPM, so
	// concurrency only buys back the time otherwise spent waiting.
	workers int

	log *slog.Logger
}

// NewPoller builds a Poller.
func NewPoller(
	api *tg.Client,
	proc *Processor,
	statePath string,
	bootstrap time.Time,
	workers int,
	log *slog.Logger,
) *Poller {
	if workers < 1 {
		workers = 1
	}
	return &Poller{
		api:       api,
		proc:      proc,
		statePath: statePath,
		bootstrap: bootstrap,
		workers:   workers,
		log:       log,
	}
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

	byChannel := groupByChannel(pending)
	p.log.Info("poll: analysing",
		slog.Int("pending", len(pending)),
		slog.Int("channels", len(byChannel)),
		slog.Int("workers", p.workers))

	var (
		mu          sync.Mutex
		counts      = map[Outcome]int{}
		processed   int
		budgetSpent bool
		fatal       error
		wg          sync.WaitGroup
	)
	sem := make(chan struct{}, p.workers)

	for id, posts := range byChannel {
		wg.Add(1)
		go func(id int64, posts []pendingPost) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			for _, post := range posts {
				mu.Lock()
				stop := fatal != nil || budgetSpent
				mu.Unlock()
				if stop {
					return
				}
				if err := ctx.Err(); err != nil {
					mu.Lock()
					fatal = err
					mu.Unlock()
					return
				}
				if !deadline.IsZero() && time.Now().After(deadline) {
					mu.Lock()
					budgetSpent = true
					mu.Unlock()
					return
				}

				text := strings.TrimSpace(post.msg.Message)
				if text == "" {
					// Media without a caption carries nothing to classify.
					mu.Lock()
					counts[OutcomeSkipped]++
					state.advance(id, post.msg.ID)
					processed++
					mu.Unlock()
					continue
				}

				outcome, err := p.proc.Process(ctx, post.channel, post.msg.ID, text, "poll")

				mu.Lock()
				if err != nil {
					fatal = err
					mu.Unlock()
					return
				}
				counts[outcome]++
				// An operational failure must not advance the cursor — the
				// post is retried on the next run.
				if outcome != OutcomeError {
					state.advance(id, post.msg.ID)
				}
				processed++
				flush := processed%stateFlushEvery == 0
				mu.Unlock()

				if flush {
					if err := p.saveState(state); err != nil {
						p.log.Warn("poll: save state failed", slog.Any("err", err))
					}
				}
			}
		}(id, posts)
	}
	wg.Wait()

	if fatal != nil {
		_ = p.saveState(state)
		return true, fatal
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
		lastSeen := state.cursor(id)

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

		// Every message goes into the queue, captionless media included. The
		// cursor must only ever move over posts the worker has actually
		// reached, in ID order — advancing here would jump it past pending
		// posts whenever a newer one happens to carry no text, and those
		// posts would never be fetched again.
		for _, m := range msgs {
			pending = append(pending, pendingPost{channelID: id, channel: resolved.Channel, msg: m})
		}

		p.log.Info("poll: fetched",
			slog.String("channel", resolved.Channel.Title),
			slog.Int("new", len(msgs)),
			slog.Int("cursor", lastSeen))
	}
	return pending, nil
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
	raw, err := s.marshal()
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

// groupByChannel buckets the queue per channel in ascending message ID order.
// The order is what makes the cursor safe: a channel's cursor may only move
// over posts the worker has already passed, so anything still queued must have
// a higher ID than anything completed.
func groupByChannel(pending []pendingPost) map[int64][]pendingPost {
	byChannel := make(map[int64][]pendingPost)
	for _, post := range pending {
		byChannel[post.channelID] = append(byChannel[post.channelID], post)
	}
	for _, posts := range byChannel {
		sort.Slice(posts, func(i, j int) bool { return posts[i].msg.ID < posts[j].msg.ID })
	}
	return byChannel
}
