package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
)

// FetchHistorySince returns channel posts whose `date` is in the half-open
// range [since, until), ordered oldest-first so they can be processed in the
// same sequence as live updates.
//
// Telegram's getHistory is "newest first, paginate backwards via OffsetID".
// We stop as soon as we see a message older than `since`.
func FetchHistorySince(
	ctx context.Context,
	api *tg.Client,
	peer tg.InputPeerClass,
	since time.Time,
	until time.Time,
) ([]*tg.Message, error) {
	if until.Before(since) {
		return nil, fmt.Errorf("history range: until (%s) before since (%s)",
			until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	sinceUnix := int(since.Unix())
	untilUnix := int(until.Unix())

	var (
		collected []*tg.Message
		offsetID  int
	)
	const batch = 100

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			Limit:    batch,
		})
		if err != nil {
			return nil, fmt.Errorf("getHistory: %w", err)
		}

		msgs, err := extractMessages(resp)
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			break
		}

		reachedSince := false
		var oldestID int
		for _, raw := range msgs {
			m, ok := raw.(*tg.Message)
			if !ok {
				// Service messages (pins, title changes, etc.) — keep going
				// but still use their ID for pagination below.
				if id := raw.GetID(); id != 0 && (oldestID == 0 || id < oldestID) {
					oldestID = id
				}
				continue
			}
			if oldestID == 0 || m.ID < oldestID {
				oldestID = m.ID
			}
			if m.Date < sinceUnix {
				reachedSince = true
				continue
			}
			if m.Date >= untilUnix {
				// Skip messages newer than the cutoff — these are handled by
				// the live listener, so collecting them would dup-notify.
				continue
			}
			collected = append(collected, m)
		}

		if reachedSince || oldestID == 0 || oldestID == offsetID {
			break
		}
		offsetID = oldestID
	}

	// Reverse into chronological order.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

func extractMessages(resp tg.MessagesMessagesClass) ([]tg.MessageClass, error) {
	switch r := resp.(type) {
	case *tg.MessagesMessages:
		return r.Messages, nil
	case *tg.MessagesMessagesSlice:
		return r.Messages, nil
	case *tg.MessagesChannelMessages:
		return r.Messages, nil
	case *tg.MessagesMessagesNotModified:
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected getHistory response: %T", resp)
	}
}
