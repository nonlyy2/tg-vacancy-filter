package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gotd/td/tg"
)

// ParseInviteHash extracts the invite hash from the common Telegram link shapes:
//
//	https://t.me/+ABcd123
//	t.me/+ABcd123
//	https://t.me/joinchat/ABcd123
//	telegram.me/joinchat/ABcd123
//
// Returns (hash, true) on success. If the input does not look like an invite
// link, returns ("", false) so callers can fall back to username resolution.
func ParseInviteHash(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://", "tg://"} {
		trimmed = strings.TrimPrefix(trimmed, p)
	}
	trimmed = strings.TrimPrefix(trimmed, "www.")
	for _, p := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		trimmed = strings.TrimPrefix(trimmed, p)
	}

	switch {
	case strings.HasPrefix(trimmed, "+"):
		h := strings.TrimPrefix(trimmed, "+")
		return h, h != ""
	case strings.HasPrefix(trimmed, "joinchat/"):
		h := strings.TrimPrefix(trimmed, "joinchat/")
		return h, h != ""
	}
	return "", false
}

// ResolveInvite turns an invite hash into an InputPeerClass. If the account is
// already a member, it extracts the access hash from the check response.
// Otherwise it joins the chat via messages.importChatInvite — this is the
// desired behaviour because the user explicitly configured this link as a
// destination.
func ResolveInvite(ctx context.Context, api *tg.Client, hash string, log *slog.Logger) (tg.InputPeerClass, error) {
	invite, err := api.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("check invite: %w", err)
	}
	if chat := chatFromCheckInvite(invite); chat != nil {
		log.Info("invite: account is already a member")
		return chatToInputPeer(chat)
	}

	log.Info("invite: joining chat")
	updates, err := api.MessagesImportChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("import invite: %w", err)
	}
	for _, c := range chatsFromUpdates(updates) {
		peer, err := chatToInputPeer(c)
		if err == nil {
			return peer, nil
		}
	}
	return nil, errors.New("invite imported but no chat returned in updates")
}

func chatFromCheckInvite(i tg.ChatInviteClass) tg.ChatClass {
	switch v := i.(type) {
	case *tg.ChatInviteAlready:
		return v.Chat
	case *tg.ChatInvitePeek:
		return v.Chat
	}
	return nil
}

func chatToInputPeer(c tg.ChatClass) (tg.InputPeerClass, error) {
	switch v := c.(type) {
	case *tg.Channel:
		if v.Left {
			return nil, fmt.Errorf("account left channel %d — re-invite or set DESTINATION=me", v.ID)
		}
		return &tg.InputPeerChannel{
			ChannelID:  v.ID,
			AccessHash: v.AccessHash,
		}, nil
	case *tg.Chat:
		return &tg.InputPeerChat{ChatID: v.ID}, nil
	case *tg.ChatForbidden, *tg.ChannelForbidden:
		return nil, fmt.Errorf("chat is forbidden for this account")
	}
	return nil, fmt.Errorf("unsupported chat type %T", c)
}

// chatsFromUpdates extracts the Chats field from the Updates variants returned
// by importChatInvite. Only the two shapes that actually carry a chat list are
// handled; other variants (e.g. UpdateShort) wouldn't contain the info we need.
func chatsFromUpdates(u tg.UpdatesClass) []tg.ChatClass {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Chats
	case *tg.UpdatesCombined:
		return v.Chats
	}
	return nil
}
