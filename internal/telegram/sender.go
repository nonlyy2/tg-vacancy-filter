package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// maxPostExcerpt caps how much of the original post we paste into the
// notification. Telegram's hard limit on a text message is 4096 chars; the
// surrounding template eats ~300, so 3500 leaves a comfortable margin
// without truncating most real-world job posts.
const maxPostExcerpt = 3500

// Sender posts match notifications into a pre-resolved destination peer.
type Sender struct {
	api  *tg.Client
	dest tg.InputPeerClass
}

// NewSender builds a Sender bound to the given destination peer.
func NewSender(api *tg.Client, dest tg.InputPeerClass) *Sender {
	return &Sender{api: api, dest: dest}
}

// Notify sends a match notification with the AI reason, an excerpt of the
// original post text and a deep link back to the source. postText may be
// empty (e.g. media-only post) — in that case the body is omitted.
func (s *Sender) Notify(ctx context.Context, sourceChannel *tg.Channel, msgID int, reason, postText string) error {
	link := BuildPostLink(sourceChannel, msgID)
	channelName := sourceChannel.Title
	if channelName == "" {
		channelName = sourceChannel.Username
	}

	var b strings.Builder
	b.WriteString("✅ Подходящая вакансия\n\n📢 Канал: ")
	b.WriteString(channelName)
	if reason = strings.TrimSpace(reason); reason != "" {
		b.WriteString("\n💡 ")
		b.WriteString(reason)
	}
	if excerpt := truncateRunes(strings.TrimSpace(postText), maxPostExcerpt); excerpt != "" {
		b.WriteString("\n\n📝 ")
		b.WriteString(excerpt)
	}
	b.WriteString("\n\n🔗 ")
	b.WriteString(link)

	id, err := randomID()
	if err != nil {
		return fmt.Errorf("random id: %w", err)
	}

	_, err = s.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     s.dest,
		Message:  b.String(),
		RandomID: id,
	})
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	return nil
}

// truncateRunes shortens s to at most n runes (not bytes — Cyrillic chars are
// 2 bytes in UTF-8 and Telegram counts in UTF-16 code units, but rune-count
// is a sane proxy that won't split a multi-byte character mid-sequence).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// BuildPostLink returns a t.me link to a channel post. Public channels use the
// username form; private ones fall back to the /c/<id>/<msg> form.
func BuildPostLink(ch *tg.Channel, msgID int) string {
	if ch.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", ch.Username, msgID)
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", ch.ID, msgID)
}

// randomID returns a cryptographically random int64 suitable for
// MessagesSendMessageRequest.RandomID.
func randomID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}
