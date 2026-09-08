package telegram

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/gotd/td/tg"
)

// Channels are analysed in parallel, so the cursor map is written from several
// goroutines while the periodic save reads it. Run with -race.
func TestPollStateConcurrentAccess(t *testing.T) {
	s := &pollState{Channels: map[string]int{}, Seen: map[string]int64{}}

	const (
		workers   = 8
		perWorker = 50
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 1; i <= perWorker; i++ {
				s.advance(int64(w), i)
				s.Mark("fp-" + strconv.Itoa(w) + "-" + strconv.Itoa(i))
				if _, err := s.marshal(); err != nil {
					t.Error(err)
					return
				}
				s.cursor(int64(w))
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < workers; w++ {
		if got := s.cursor(int64(w)); got != perWorker {
			t.Errorf("cursor(%d) = %d, want %d", w, got, perWorker)
		}
	}
	if len(s.Seen) != workers*perWorker {
		t.Errorf("Seen has %d entries, want %d", len(s.Seen), workers*perWorker)
	}
}

// The cursor must never move backwards: a worker finishing an older post after
// a newer one must not rewind the channel.
func TestPollStateAdvanceNeverRewinds(t *testing.T) {
	s := &pollState{Channels: map[string]int{}, Seen: map[string]int64{}}
	s.advance(42, 100)
	s.advance(42, 7)
	if got := s.cursor(42); got != 100 {
		t.Fatalf("cursor = %d, want 100", got)
	}
}

// The mutex must not leak into the persisted file.
func TestPollStateMarshalShape(t *testing.T) {
	s := &pollState{Channels: map[string]int{"1": 5}, Seen: map[string]int64{"a": 1}}
	raw, err := s.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var back pollState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Channels["1"] != 5 || back.Seen["a"] != 1 {
		t.Fatalf("round-trip lost data: %s", raw)
	}
}

// A caption-less post used to advance the cursor during collect, before any
// analysis. When such a post was newer than queued text posts, the cursor
// jumped past them and the next run's MinID fetch never returned them again —
// 123 posts of a 716-post backfill were lost that way. The queue must now
// carry every message, ordered by ID, so the cursor can only trail the worker.
func TestGroupByChannelKeepsEveryPostInIDOrder(t *testing.T) {
	post := func(ch int64, id int, text string) pendingPost {
		return pendingPost{channelID: ch, msg: &tg.Message{ID: id, Message: text}}
	}
	// Arrival order is deliberately jumbled, and the newest post of channel 1
	// is the caption-less one.
	pending := []pendingPost{
		post(1, 30, ""),
		post(1, 10, "вакансия Go"),
		post(2, 5, "вакансия Python"),
		post(1, 20, "вакансия Python"),
	}

	got := groupByChannel(pending)

	if len(got[1]) != 3 {
		t.Fatalf("channel 1 has %d posts, want 3 — a captionless post must stay in the queue", len(got[1]))
	}
	for i, want := range []int{10, 20, 30} {
		if got[1][i].msg.ID != want {
			t.Fatalf("channel 1 position %d = id %d, want %d", i, got[1][i].msg.ID, want)
		}
	}
	if len(got[2]) != 1 || got[2][0].msg.ID != 5 {
		t.Fatalf("channel 2 = %+v, want a single post id 5", got[2])
	}
}
