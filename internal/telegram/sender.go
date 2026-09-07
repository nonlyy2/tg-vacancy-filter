package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/assylkhan/tg-vacancy-filter/internal/gemini"
)

// maxPostExcerpt caps how much of the original post we paste into the
// notification. Telegram's hard limit on a text message is 4096 chars; the
// surrounding template eats ~400, so 3400 leaves a comfortable margin
// without truncating most real-world job posts.
const maxPostExcerpt = 3400

// sendInterval paces notifications. A bootstrap sweep can produce dozens of
// matches back-to-back, and a userbot firing them into a private chat without
// a gap is exactly the pattern that earns a FLOOD_WAIT.
const sendInterval = 1500 * time.Millisecond

// maxProsCons bounds how many bullet points make it into the message — the
// model occasionally returns a long list and the post text matters more.
const maxProsCons = 4

// Sender posts match notifications into a pre-resolved destination peer.
type Sender struct {
	api  *tg.Client
	dest tg.InputPeerClass

	mu       sync.Mutex
	lastSend time.Time
}

// NewSender builds a Sender bound to the given destination peer.
func NewSender(api *tg.Client, dest tg.InputPeerClass) *Sender {
	return &Sender{api: api, dest: dest}
}

// Notify sends a match notification with the score, the model's reasoning and
// an excerpt of the original post, plus a deep link back to the source.
func (s *Sender) Notify(
	ctx context.Context,
	sourceChannel *tg.Channel,
	msgID int,
	v gemini.Verdict,
	postText string,
) error {
	return s.Send(ctx, formatMatch(sourceChannel, msgID, v, postText))
}

// Send delivers one message, respecting the send interval and retrying once
// after a FLOOD_WAIT.
func (s *Sender) Send(ctx context.Context, text string) error {
	if err := s.pace(ctx); err != nil {
		return err
	}

	err := s.send(ctx, text)
	wait, isFlood := tgerr.AsFloodWait(err)
	if !isFlood {
		return err
	}

	// Telegram told us exactly how long to wait; honour it once, then give up
	// so a stuck send cannot stall the whole run.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait + time.Second):
	}
	return s.send(ctx, text)
}

func (s *Sender) send(ctx context.Context, text string) error {
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("random id: %w", err)
	}
	if _, err := s.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     s.dest,
		Message:  text,
		RandomID: id,
	}); err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	return nil
}

// pace blocks until at least sendInterval has passed since the previous send.
func (s *Sender) pace(ctx context.Context) error {
	s.mu.Lock()
	wait := time.Until(s.lastSend.Add(sendInterval))
	s.lastSend = time.Now().Add(max(wait, 0))
	s.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func formatMatch(ch *tg.Channel, msgID int, v gemini.Verdict, postText string) string {
	channelName := ch.Title
	if channelName == "" {
		channelName = ch.Username
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ %d/100", v.Score)
	if role := strings.TrimSpace(v.Role); role != "" {
		b.WriteString(" · ")
		b.WriteString(role)
	}
	b.WriteString("\n📢 ")
	b.WriteString(channelName)

	if line := formatFormat(v); line != "" {
		b.WriteString("\n🌍 ")
		b.WriteString(line)
	}
	if s := strings.TrimSpace(v.Seniority); s != "" {
		b.WriteString("\n🎯 ")
		b.WriteString(s)
	}
	if stack := strings.Join(v.Stack, ", "); stack != "" {
		b.WriteString("\n🧩 ")
		b.WriteString(stack)
	}
	if sum := strings.TrimSpace(v.Summary); sum != "" {
		b.WriteString("\n\n")
		b.WriteString(sum)
	}
	writeBullets(&b, "➕", v.Pros)
	writeBullets(&b, "➖", v.Cons)

	if excerpt := truncateRunes(strings.TrimSpace(postText), maxPostExcerpt); excerpt != "" {
		b.WriteString("\n\n📝 ")
		b.WriteString(excerpt)
	}
	b.WriteString("\n\n🔗 ")
	b.WriteString(BuildPostLink(ch, msgID))
	return b.String()
}

// formatFormat renders the work-format line in Russian; the model returns the
// value as one of four English constants.
func formatFormat(v gemini.Verdict) string {
	var format string
	switch v.Remote {
	case gemini.RemoteYes:
		format = "удалённо"
	case gemini.RemoteHybrid:
		format = "гибрид"
	case gemini.RemoteOnsite:
		format = "офис"
	default:
		format = "формат не указан"
	}
	if loc := strings.TrimSpace(v.Location); loc != "" {
		return format + " · " + loc
	}
	return format
}

func writeBullets(b *strings.Builder, marker string, items []string) {
	for i, item := range items {
		if i >= maxProsCons {
			return
		}
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(marker)
		b.WriteString(" ")
		b.WriteString(item)
	}
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
