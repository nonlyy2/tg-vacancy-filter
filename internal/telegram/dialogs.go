package telegram

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// ResolvedChannel groups the decoded channel object with an input peer that's
// immediately usable for channel-bound RPCs.
type ResolvedChannel struct {
	Channel   *tg.Channel
	InputPeer *tg.InputPeerChannel
}

// ResolveChannelPeers enumerates the signed-in user's dialogs and returns the
// requested channels together with their access hashes. IDs we can't find are
// reported via the second return value so the caller can warn & continue.
//
// We intentionally do a single getDialogs call with a generous limit — the
// userbot is expected to be a member of ≤ a few hundred dialogs. If the
// channel list returns incomplete, the caller surfaces a clear error.
func ResolveChannelPeers(
	ctx context.Context,
	api *tg.Client,
	wantIDs map[int64]struct{},
) (map[int64]ResolvedChannel, []int64, error) {
	resp, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      500,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("getDialogs: %w", err)
	}

	var chats []tg.ChatClass
	switch r := resp.(type) {
	case *tg.MessagesDialogs:
		chats = r.Chats
	case *tg.MessagesDialogsSlice:
		chats = r.Chats
	case *tg.MessagesDialogsNotModified:
		return nil, nil, fmt.Errorf("unexpected DialogsNotModified response")
	default:
		return nil, nil, fmt.Errorf("unexpected dialogs response: %T", resp)
	}

	found := make(map[int64]ResolvedChannel, len(wantIDs))
	for _, c := range chats {
		ch, ok := c.(*tg.Channel)
		if !ok {
			continue
		}
		if _, want := wantIDs[ch.ID]; !want {
			continue
		}
		if ch.Left {
			// Surface as "missing" so the operator fixes it rather than
			// silently get empty history.
			continue
		}
		found[ch.ID] = ResolvedChannel{
			Channel: ch,
			InputPeer: &tg.InputPeerChannel{
				ChannelID:  ch.ID,
				AccessHash: ch.AccessHash,
			},
		}
	}

	missing := make([]int64, 0, len(wantIDs)-len(found))
	for id := range wantIDs {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	return found, missing, nil
}
