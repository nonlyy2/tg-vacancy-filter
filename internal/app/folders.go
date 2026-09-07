package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/gotd/td/tg"

	tgclient "github.com/assylkhan/tg-vacancy-filter/internal/telegram"
)

// Folders lists the account's Telegram chat folders and the channels in each,
// printing a ready-to-paste SOURCE_CHANNEL_IDS line. Curating the watch list
// as a folder in the Telegram app is far easier than collecting ids by hand.
func Folders(ctx context.Context, log *slog.Logger, want string) error {
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	e, err := setup(ctx, quiet, nil)
	if err != nil {
		return err
	}
	defer e.close()

	return e.client.Run(ctx, func(ctx context.Context) error {
		s, err := e.connect(ctx)
		if err != nil {
			return err
		}

		filters, err := s.api.MessagesGetDialogFilters(ctx)
		if err != nil {
			return fmt.Errorf("getDialogFilters: %w", err)
		}

		for _, f := range filters.Filters {
			title, peers := folderContents(f)
			if title == "" {
				continue
			}
			if want != "" && !strings.EqualFold(strings.TrimSpace(title), strings.TrimSpace(want)) {
				continue
			}
			printFolder(ctx, s.api, title, peers)
		}
		return nil
	})
}

// folderContents flattens the two folder shapes into a title and the channel
// ids they contain. Non-channel peers (users, basic groups) are ignored — the
// bot only reads channels.
func folderContents(f tg.DialogFilterClass) (string, []int64) {
	var (
		title string
		peers []tg.InputPeerClass
	)
	switch v := f.(type) {
	case *tg.DialogFilter:
		title, peers = v.Title.Text, append(v.PinnedPeers, v.IncludePeers...)
	case *tg.DialogFilterChatlist:
		title, peers = v.Title.Text, append(v.PinnedPeers, v.IncludePeers...)
	default:
		return "", nil
	}

	seen := make(map[int64]struct{}, len(peers))
	ids := make([]int64, 0, len(peers))
	for _, p := range peers {
		ch, ok := p.(*tg.InputPeerChannel)
		if !ok {
			continue
		}
		if _, dup := seen[ch.ChannelID]; dup {
			continue
		}
		seen[ch.ChannelID] = struct{}{}
		ids = append(ids, ch.ChannelID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return title, ids
}

func printFolder(ctx context.Context, api *tg.Client, title string, ids []int64) {
	fmt.Printf("\n=== folder %q — %d channels ===\n", title, len(ids))
	if len(ids) == 0 {
		return
	}

	want := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	// Titles are not in the folder payload; the dialog sweep supplies them.
	found, _, err := tgclient.ResolveChannelPeers(ctx, api, want)
	if err != nil {
		fmt.Println("  (could not resolve names:", err, ")")
	}

	for _, id := range ids {
		name, handle := "?", ""
		if ch, ok := found[id]; ok {
			name = ch.Channel.Title
			if ch.Channel.Username != "" {
				handle = "@" + ch.Channel.Username
			} else {
				handle = "private"
			}
		}
		fmt.Printf("  -100%-13d %-42s %s\n", id, truncate(name, 42), handle)
	}

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("-100%d", id))
	}
	fmt.Printf("\nSOURCE_CHANNEL_IDS=%s\n", strings.Join(parts, ","))
}
