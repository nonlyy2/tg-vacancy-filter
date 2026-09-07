package telegram

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// dialogPageSize is the per-request cap Telegram actually honours for
// messages.getDialogs — asking for more silently returns one page.
const dialogPageSize = 100

// maxDialogPages bounds the sweep so a pathological account (or a server that
// stops advancing the offset) cannot spin forever.
const maxDialogPages = 50

// Dialog folders. Archived chats are not returned by the default listing.
const (
	folderMain    = 0
	folderArchive = 1
)

// ResolvedChannel groups the decoded channel object with an input peer that's
// immediately usable for channel-bound RPCs.
type ResolvedChannel struct {
	Channel   *tg.Channel
	InputPeer *tg.InputPeerChannel
}

// ResolveChannelPeers pages through the signed-in user's dialogs and returns
// the requested channels together with their access hashes. IDs that were not
// found are reported via the second return value so the caller can warn and
// continue.
//
// Two things make this more than one RPC. A single request returns at most
// ~100 dialogs regardless of the requested limit, so a long chat list needs
// paging. And archived chats live in a separate folder that the default
// listing never returns — a source channel the user archived would otherwise
// look like "not joined".
func ResolveChannelPeers(
	ctx context.Context,
	api *tg.Client,
	wantIDs map[int64]struct{},
) (map[int64]ResolvedChannel, []int64, error) {
	found := make(map[int64]ResolvedChannel, len(wantIDs))

	for _, folder := range []int{folderMain, folderArchive} {
		if len(found) == len(wantIDs) {
			break
		}
		if err := sweepFolder(ctx, api, folder, wantIDs, found); err != nil {
			return nil, nil, err
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

// sweepFolder pages one dialog folder, adding every wanted channel it sees.
func sweepFolder(
	ctx context.Context,
	api *tg.Client,
	folder int,
	wantIDs map[int64]struct{},
	found map[int64]ResolvedChannel,
) error {
	var (
		offsetDate int
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)

	for page := 0; page < maxDialogPages && len(found) < len(wantIDs); page++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req := &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      dialogPageSize,
		}
		if folder != folderMain {
			req.SetFolderID(folder)
		}

		resp, err := api.MessagesGetDialogs(ctx, req)
		if err != nil {
			return fmt.Errorf("getDialogs(folder=%d): %w", folder, err)
		}

		dialogs, messages, chats, users, exhausted, err := unpackDialogs(resp)
		if err != nil {
			return err
		}

		collectChannels(chats, wantIDs, found)

		if exhausted || len(dialogs) == 0 || len(found) == len(wantIDs) {
			break
		}

		nextDate, nextID, nextPeer := dialogOffset(dialogs, messages, chats, users)
		if nextPeer == nil || (nextID == offsetID && nextDate == offsetDate) {
			// Offset stopped advancing — stop rather than re-request the
			// same page forever.
			break
		}
		offsetDate, offsetID, offsetPeer = nextDate, nextID, nextPeer
	}
	return nil
}

// unpackDialogs flattens the two dialog response shapes. exhausted is true for
// messages.dialogs, which is the complete list by definition.
func unpackDialogs(resp tg.MessagesDialogsClass) (
	dialogs []tg.DialogClass,
	messages []tg.MessageClass,
	chats []tg.ChatClass,
	users []tg.UserClass,
	exhausted bool,
	err error,
) {
	switch r := resp.(type) {
	case *tg.MessagesDialogs:
		return r.Dialogs, r.Messages, r.Chats, r.Users, true, nil
	case *tg.MessagesDialogsSlice:
		return r.Dialogs, r.Messages, r.Chats, r.Users, len(r.Dialogs) < dialogPageSize, nil
	case *tg.MessagesDialogsNotModified:
		return nil, nil, nil, nil, true, nil
	default:
		return nil, nil, nil, nil, true, fmt.Errorf("unexpected dialogs response: %T", resp)
	}
}

func collectChannels(chats []tg.ChatClass, wantIDs map[int64]struct{}, into map[int64]ResolvedChannel) {
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
			// silently getting empty history.
			continue
		}
		into[ch.ID] = ResolvedChannel{
			Channel: ch,
			InputPeer: &tg.InputPeerChannel{
				ChannelID:  ch.ID,
				AccessHash: ch.AccessHash,
			},
		}
	}
}

// dialogOffset derives the (date, id, peer) triple that continues the listing
// after the last dialog of the current page.
func dialogOffset(
	dialogs []tg.DialogClass,
	messages []tg.MessageClass,
	chats []tg.ChatClass,
	users []tg.UserClass,
) (int, int, tg.InputPeerClass) {
	last := dialogs[len(dialogs)-1]
	topID := last.GetTopMessage()

	date := 0
	for _, m := range messages {
		if m.GetID() == topID {
			date = messageDate(m)
			break
		}
	}
	return date, topID, inputPeerFor(last.GetPeer(), chats, users)
}

func messageDate(m tg.MessageClass) int {
	switch v := m.(type) {
	case *tg.Message:
		return v.Date
	case *tg.MessageService:
		return v.Date
	}
	return 0
}

// inputPeerFor resolves a dialog peer into an input peer using the entity
// lists that came with the same response. Returns nil when the access hash is
// not available, which stops pagination instead of sending a broken offset.
func inputPeerFor(peer tg.PeerClass, chats []tg.ChatClass, users []tg.UserClass) tg.InputPeerClass {
	switch p := peer.(type) {
	case *tg.PeerUser:
		for _, u := range users {
			if user, ok := u.(*tg.User); ok && user.ID == p.UserID {
				return &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
			}
		}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}
	case *tg.PeerChannel:
		for _, c := range chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == p.ChannelID {
				return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			}
		}
	}
	return nil
}

// ResolveChannelByID finds one channel by its numeric ID and returns an input
// peer for it. Used for a DESTINATION given as an id rather than a username —
// a private channel you created has no username to resolve.
func ResolveChannelByID(ctx context.Context, api *tg.Client, id int64) (tg.InputPeerClass, error) {
	found, _, err := ResolveChannelPeers(ctx, api, map[int64]struct{}{id: {}})
	if err != nil {
		return nil, err
	}
	ch, ok := found[id]
	if !ok {
		return nil, fmt.Errorf("channel %d not found in this account's dialogs "+
			"(wrong id, or the account is not a member)", id)
	}
	return ch.InputPeer, nil
}
