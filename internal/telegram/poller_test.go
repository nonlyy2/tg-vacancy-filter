package telegram

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
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
