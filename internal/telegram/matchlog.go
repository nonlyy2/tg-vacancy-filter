package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// MatchRecord is a single append-only entry written per successful match.
// Kept as a separate audit trail so the user does not lose results if the
// Telegram send step fails or the destination channel is later cleared.
type MatchRecord struct {
	Time         time.Time `json:"time"`
	ChannelID    int64     `json:"channel_id"`
	ChannelTitle string    `json:"channel"`
	MessageID    int       `json:"msg_id"`
	Reason       string    `json:"reason"`
	Link         string    `json:"link"`
	Source       string    `json:"source"` // "live" or "backfill"
}

// MatchLog appends JSON-lines to a file on disk. A nil *MatchLog is a safe
// no-op, so callers can wire it unconditionally and the user can opt out by
// setting MATCH_LOG_PATH to empty.
type MatchLog struct {
	mu   sync.Mutex
	path string
}

// NewMatchLog returns a MatchLog writing to path. If path is empty, returns
// nil — every method on a nil *MatchLog is a no-op.
func NewMatchLog(path string) *MatchLog {
	if path == "" {
		return nil
	}
	return &MatchLog{path: path}
}

// Append writes one JSON-encoded record followed by a newline. Safe to call
// concurrently from the live handler and the backfiller.
func (m *MatchLog) Append(rec MatchRecord) error {
	if m == nil {
		return nil
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("matchlog: marshal: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("matchlog: open: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("matchlog: write: %w", err)
	}
	return nil
}
