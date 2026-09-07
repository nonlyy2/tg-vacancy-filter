// Package filter holds the cheap, deterministic checks that run before a post
// is ever sent to the model. Every post they reject is one LLM call saved,
// which is what keeps a two-week backlog inside the free-tier daily quota.
package filter

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// minPostRunes is the shortest post worth classifying. Real vacancies carry a
// role, a stack and contacts; anything shorter is a teaser, a reaction or a
// channel signature.
const minPostRunes = 120

// Drop reasons, returned by Keep for debug logging.
const (
	ReasonTooShort   = "too short"
	ReasonNotHiring  = "no hiring keywords"
	ReasonEmpty      = "empty"
	ReasonAggregator = "digest link only"
)

var (
	// hiringRe matches the vocabulary every real job post uses in at least one
	// of the three languages these channels publish in.
	hiringRe = regexp.MustCompile(`(?i)(ваканс|вакансия|ищем|ищет|требуется|требуются|нанима|` +
		`открыт[аы]? позици|зарплат|оклад|резюме|откликн|отклик|собеседован|` +
		`hiring|we are looking|we're looking|looking for|job opening|position|` +
		`salary|apply now|send your cv|remote position|` +
		`жұмыс|бос орын|қызметкер)`)

	// urlRe and handleRe are stripped before fingerprinting: the same vacancy
	// reposted across channels differs mainly by its apply link and the
	// reposting channel's signature.
	urlRe    = regexp.MustCompile(`(?i)https?://\S+|t\.me/\S+|www\.\S+`)
	handleRe = regexp.MustCompile(`@[A-Za-z0-9_]{3,}`)

	// digestRe catches "all our vacancies are here" posts that carry a link
	// and nothing classifiable.
	digestRe = regexp.MustCompile(`(?i)^(все ваканси|наши ваканси|дайджест|подборка ваканси)`)
)

// Keep reports whether a post is worth an LLM call. The second return value
// names the rule that rejected it, for debug logs.
func Keep(text string) (bool, string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false, ReasonEmpty
	}
	if utf8.RuneCountInString(trimmed) < minPostRunes {
		return false, ReasonTooShort
	}
	if digestRe.MatchString(trimmed) {
		return false, ReasonAggregator
	}
	if !hiringRe.MatchString(trimmed) {
		return false, ReasonNotHiring
	}
	return true, ""
}

// Fingerprint returns a stable hash of the normalised post text. Two channels
// reposting the same vacancy produce the same fingerprint, so the second copy
// never reaches the model.
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(normalize(text)))
	return hex.EncodeToString(sum[:])
}

// normalize strips everything that varies between reposts of one vacancy:
// links, @handles, emoji, punctuation, letter case and whitespace runs.
func normalize(text string) string {
	s := strings.ToLower(text)
	s = urlRe.ReplaceAllString(s, " ")
	s = handleRe.ReplaceAllString(s, " ")

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case !lastSpace:
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}
