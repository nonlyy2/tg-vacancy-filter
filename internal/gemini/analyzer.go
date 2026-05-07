// Package gemini wraps the Google Generative AI SDK with a small, opinionated
// analyser that classifies Telegram posts as "matching" or "not matching" a
// fixed candidate profile.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// maxRetries429 bounds how many times Analyze retries on RESOURCE_EXHAUSTED.
// The limiter prevents 429s steady-state; this only protects against short
// bursts and slight quota-accounting skew at the Google edge.
const maxRetries429 = 3

// retryAfterRe pulls the "retry in 38.341s" hint out of the googleapi error
// message. The SDK also exposes this via googleapi.Error.Details, but parsing
// the structured proto is heavier than a regex over the already-formatted text.
var retryAfterRe = regexp.MustCompile(`retry in (\d+(?:\.\d+)?)s`)

// decisionRules is the candidate brief + rule list, shared by both output
// formats. Hard-reject rules come first as a checklist — Gemma 4 is weak at
// nuanced instruction-following and tends to over-apply "lean toward match"
// guidance to obviously off-target posts (analyst roles, UX writer, etc.)
// when the rejection criteria are buried below the inclusion ones.
const decisionRules = `You are an HR assistant filtering Telegram posts for one specific candidate.
Posts are in Russian, Kazakh, or English — treat them equally.

CANDIDATE PROFILE (the ONLY person you are filtering for):
- Role family: backend software engineer.
- Seniority: Intern / Trainee / Junior (or entry-level unspecified).
- Required language: Go / Golang must be in the tech stack.
- Location: fully remote, OR on-site / hybrid in Astana, Kazakhstan.

DECISION ALGORITHM. Apply in this order. The FIRST trigger wins.

STEP 1 — HARD REJECTIONS. If ANY of these is true, return MATCH: no immediately.
Do not apply the inclusion rules below; do not "lean toward match" here.

  R-A) The role is NOT software engineering / programming. Examples that MUST
       be rejected even if location and seniority look fine:
         • Аналитик / business analyst / data analyst without coding tasks
         • UX-редактор / UX writer / copywriter / редактор / журналист
         • Designer / UX/UI designer / графический дизайнер
         • Manager / project manager / product manager / scrum master
         • HR / recruiter / sourcer
         • Sales / маркетолог / SMM / контент-менеджер
         • Accountant / бухгалтер / lawyer / юрист / финансист
         • Support / customer service / call-centre / оператор
         • QA manual without coding, тестировщик ручной без автоматизации
         • Teacher / tutor / преподаватель
         • Any blue-collar / non-IT job
       If the role name itself signals non-engineering, reject — even when
       the post mentions "Figma", "Notion", "SQL" or similar tooling.

  R-B) Go / Golang is NOT mentioned anywhere as a required or primary
       technology. The post must contain the token "Go" or "Golang"
       referring to the programming language. If the entire stack is
       Python / Java / JS / TS / PHP / C# / Ruby / Rust / Kotlin / Swift /
       Scala / Elixir / 1C / etc. and Go is absent — reject.

  R-C) Go is mentioned only as "будет плюсом" / "nice to have" / "as a
       bonus" while a different language is clearly the primary one — reject.

  R-D) The post explicitly demands ONLY Middle / Мидл / Senior / Сеньор /
       Lead / Team Lead / Head / Principal / Staff as the seniority, OR
       requires "3+ years of commercial experience" / "от 3 лет опыта" or
       more — reject.

  R-E) The post is on-site or hybrid in a city OTHER than Astana
       (e.g. only Алматы / Almaty, Шымкент, Москва, СПб, Bishkek, Ташкент)
       AND is NOT remote — reject. "Алматы и Астана" / "Astana, Almaty" is
       a CHOICE that includes Astana — that is fine, do not reject under R-E.

  R-F) Not a vacancy: news, memes, questions, articles, opinion pieces,
       event announcements, referral requests without a described role,
       generic "we are hiring!" teasers with no role or stack — reject.

STEP 2 — MULTI-ROLE POSTS. If the post lists several distinct roles
(e.g. "ищем Go-разработчика, PHP-разработчика и дизайнера"), evaluate each
role separately. Match if at least ONE of the listed roles passes STEP 1
hard-rejections AND would pass STEP 3 below. The candidate applies to
that specific role.

STEP 3 — INCLUSION CHECK. Only reached when STEP 1 found no hard rejection.
ALL THREE must hold for MATCH: yes.

  I-1) SENIORITY: post mentions intern / стажёр / стажировка / trainee /
       junior / джуниор / младший / "без опыта" / "no experience required" /
       "students welcome" / "entry-level", OR lists multiple seniorities
       including Junior (e.g. "Junior/Middle", "Middle and below"), OR
       seniority is simply not specified anywhere in the post.

  I-2) TECHNOLOGY: Go / Golang appears as a required or primary backend
       technology (passed R-B and R-C above).

  I-3) LOCATION: any of the following holds:
         • fully remote / удалённая / удалёнка / remote / worldwide
         • cities listed INCLUDE Astana (treat "и/или/,/" as CHOICE)
         • generic "Kazakhstan" / "Казахстан" with no specific city
         • hybrid or on-site in Astana
         • location is not mentioned at all (benefit of the doubt)

When in doubt between MATCH: yes and MATCH: no on a borderline post that
already passed STEP 1, prefer MATCH: yes — the human reviewer filters
borderline cases. But never override a STEP 1 hard rejection.`

// geminiOutput is appended to decisionRules for Gemini-family models, which
// also have ResponseSchema enforced in code — the prose here is mostly to
// shape the "reason" string.
const geminiOutput = `OUTPUT:
Return ONLY a JSON object with keys "match" (true/false) and "reason"
(short Russian sentence, <= 140 chars).
  - For match=true: which role/city/level qualified the post.
  - For match=false: cite the specific rule that failed (e.g. "Правило 2:
    основной язык Python, Go не упомянут").`

// gemmaOutput drives the Gemma path. Gemma 4 has a habit of literally echoing
// shape templates verbatim on short / ambiguous posts (both JSON and any
// "<placeholder>" form). The output spec here therefore uses concrete
// examples ONLY — no placeholder strings, no angle brackets, nothing the
// model could confuse for a literal value to return. parseGemmaText reads
// the two-line response back, and also rejects any reason that still looks
// like a template (just in case).
const gemmaOutput = `OUTPUT:
Reply with EXACTLY two lines. No JSON. No Markdown. No prose before or after.
Line 1 starts with "MATCH:" then the literal word "yes" or "no".
Line 2 starts with "REASON:" then a short Russian sentence (max 140 chars)
that NAMES THE ACTUAL REASON — never repeat or paraphrase this instruction.

Example for a post that fits the candidate:
MATCH: yes
REASON: Junior Go разработчик, удалёнка, упомянут Astana

Example for a post that does NOT fit:
MATCH: no
REASON: Правило 2: основной язык Python, Go не упомянут`

// systemPrompt is the full prompt for Gemini-family models (used as
// SystemInstruction). gemmaPrompt is its Gemma-friendly twin, inlined into
// each user message because Gemma rejects SystemInstruction.
const (
	systemPrompt = decisionRules + "\n\n" + geminiOutput
	gemmaPrompt  = decisionRules + "\n\n" + gemmaOutput
)

// Verdict is the structured response from the model.
type Verdict struct {
	Match  bool   `json:"match"`
	Reason string `json:"reason"`
}

// Analyzer is a thin wrapper around a configured Gemini generative model.
type Analyzer struct {
	client  *genai.Client
	model   *genai.GenerativeModel
	limiter *rate.Limiter
	// isGemma is true when the configured model is a Gemma variant served via
	// the Gemini API. Gemma does not support SystemInstruction /
	// ResponseMIMEType / ResponseSchema, so the analyser inlines the prompt
	// and reads back a two-line "MATCH: / REASON:" text format instead of
	// structured JSON. See gemmaPrompt + parseGemmaText.
	isGemma bool
	// onRetry is invoked before each 429 retry so the caller can observe
	// quota pressure. Nil by default.
	onRetry func(attempt int, wait time.Duration, err error)
}

// New constructs an Analyzer. The returned value must be closed with Close().
// rpm is the target request-per-minute ceiling — the free Gemini tier is
// currently 15. Pass 0 to disable the built-in limiter.
func New(ctx context.Context, apiKey, modelName string, rpm int) (*Analyzer, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("gemini: api key is empty")
	}
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("gemini: new client: %w", err)
	}

	isGemma := strings.HasPrefix(strings.ToLower(modelName), "gemma")

	model := client.GenerativeModel(modelName)

	// Gemma models rejected SystemInstruction / ResponseMIMEType /
	// ResponseSchema at the API level — they are Gemini-only features. For
	// Gemma we fall back to inlining the system prompt into each user turn
	// and extracting JSON from free-form text. See Analyze + extractJSONObject.
	if !isGemma {
		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{genai.Text(systemPrompt)},
		}
		model.ResponseMIMEType = "application/json"
		model.ResponseSchema = &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"match":  {Type: genai.TypeBoolean},
				"reason": {Type: genai.TypeString},
			},
			Required: []string{"match", "reason"},
		}
	}

	// Low temperature → stable, deterministic verdicts for near-identical posts.
	temp := float32(0.1)
	model.Temperature = &temp

	// Harm blocking off — job posts occasionally include salary/demographic
	// phrases that upstream filters misclassify. We are not exposing this
	// output publicly, we're sending it to the user's own chat.
	model.SafetySettings = []*genai.SafetySetting{
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockNone},
	}

	a := &Analyzer{client: client, model: model, isGemma: isGemma}
	if rpm > 0 {
		// Steady-state 1 request every (60/rpm) seconds. Burst=1 keeps us
		// well under the quota even under retry storms.
		a.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), 1)
	}
	return a, nil
}

// Close releases gRPC resources held by the underlying client.
func (a *Analyzer) Close() error {
	if a == nil || a.client == nil {
		return nil
	}
	return a.client.Close()
}

// SetRetryHook registers a callback fired before each 429 sleep. Used by the
// caller to log quota pressure without leaking slog into this package.
func (a *Analyzer) SetRetryHook(fn func(attempt int, wait time.Duration, err error)) {
	a.onRetry = fn
}

// Analyze sends one post to Gemini and returns its classification. It blocks
// on the configured rate limiter and transparently retries up to
// maxRetries429 on RESOURCE_EXHAUSTED, honouring the server-supplied
// "retry in Xs" hint. Cancel ctx to abort both the limiter and the retry sleep.
func (a *Analyzer) Analyze(ctx context.Context, postText string) (Verdict, error) {
	postText = strings.TrimSpace(postText)
	if postText == "" {
		return Verdict{}, errors.New("gemini: empty post")
	}

	input := postText
	if a.isGemma {
		// Gemma ignores SystemInstruction / ResponseSchema, so everything has
		// to live in the user message. The output spec asks for two text
		// lines (MATCH: yes|no / REASON: ...) — see gemmaOutput for why JSON
		// is not used here.
		input = gemmaPrompt +
			"\n\n---\nPOST:\n" + postText +
			"\n---\n\nReply with the two lines now. No JSON. No code fences."
	}

	var resp *genai.GenerateContentResponse
	for attempt := 0; ; attempt++ {
		if a.limiter != nil {
			if err := a.limiter.Wait(ctx); err != nil {
				return Verdict{}, fmt.Errorf("gemini: rate limit wait: %w", err)
			}
		}

		var err error
		resp, err = a.model.GenerateContent(ctx, genai.Text(input))
		if err == nil {
			break
		}
		if !isQuotaExceeded(err) || attempt >= maxRetries429 {
			return Verdict{}, fmt.Errorf("gemini: generate: %w", err)
		}

		wait := parseRetryAfter(err)
		if wait <= 0 {
			// Fallback: exponential backoff starting at 10s.
			wait = time.Duration(1<<attempt) * 10 * time.Second
		}
		// Add a small safety margin — Google's retry-after is the minimum.
		wait += time.Second

		if a.onRetry != nil {
			a.onRetry(attempt+1, wait, err)
		}
		select {
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		case <-time.After(wait):
		}
	}

	if a.isGemma {
		text, err := extractResponseText(resp)
		if err != nil {
			return Verdict{}, err
		}
		return parseGemmaText(text)
	}

	raw, err := extractJSONObject(resp)
	if err != nil {
		return Verdict{}, err
	}

	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("gemini: decode %q: %w", truncate(raw, 200), err)
	}
	return v, nil
}

// isQuotaExceeded reports whether err is a 429 from the Gemini API. Works with
// both the structured googleapi.Error (gRPC transport) and the fallback string
// form (some edge cases wrap it as plain error).
func isQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 429 {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "Error 429") ||
		strings.Contains(s, "RESOURCE_EXHAUSTED") ||
		strings.Contains(s, "exceeded your current quota")
}

// parseRetryAfter extracts the server-recommended delay from a Gemini 429.
// Returns 0 when the hint is absent, which is the signal for the caller to
// fall back to exponential backoff.
func parseRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	m := retryAfterRe.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return 0
	}
	sec, perr := strconv.ParseFloat(m[1], 64)
	if perr != nil || sec <= 0 {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

// extractResponseText concatenates every text Part of the first candidate.
// Both response paths (Gemini JSON, Gemma plain text) start here.
func extractResponseText(resp *genai.GenerateContentResponse) (string, error) {
	if resp == nil || len(resp.Candidates) == 0 {
		return "", errors.New("gemini: empty response")
	}
	cand := resp.Candidates[0]
	if cand.Content == nil || len(cand.Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: no content (finish=%v)", cand.FinishReason)
	}
	var buf strings.Builder
	for _, part := range cand.Content.Parts {
		if t, ok := part.(genai.Text); ok {
			buf.WriteString(string(t))
		}
	}
	text := buf.String()
	if strings.TrimSpace(text) == "" {
		return "", errors.New("gemini: no text parts in response")
	}
	return text, nil
}

// gemmaMatchRe matches "MATCH: yes/no" (case- and whitespace-insensitive).
// Accepts Russian "да/нет" and "true/false" as defensive aliases — Gemma
// occasionally substitutes those for the requested literals.
var (
	gemmaMatchRe  = regexp.MustCompile(`(?im)^[\s>*\-]*MATCH\s*[:=]\s*"?(yes|no|true|false|да|нет)"?`)
	gemmaReasonRe = regexp.MustCompile(`(?im)^[\s>*\-]*REASON\s*[:=]\s*(.+?)\s*$`)

	// gemmaPlaceholderRe rejects reasons that are clearly the prompt template
	// echoed back. Covers angle-bracket placeholders ("<short Russian ...>"),
	// the literal phrasings used in our prompt, and Gemma's own habit of
	// returning the word "string"/"reason" as a value.
	gemmaPlaceholderRe = regexp.MustCompile(`(?i)<[^>]*>|short russian sentence|max 140 chars|^(?:string|reason|placeholder|tbd|n\/a)\.?$`)
)

// parseGemmaText turns the two-line "MATCH: ... / REASON: ..." reply into a
// Verdict. Surrounding markdown bullets, blockquotes or code fences are
// tolerated. A reason that looks like the prompt template (placeholder
// echo) is treated as a parse failure so the post is skipped instead of
// forwarded with a useless caption.
func parseGemmaText(text string) (Verdict, error) {
	m := gemmaMatchRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return Verdict{}, fmt.Errorf("gemini: no MATCH line in response: %q", truncate(text, 200))
	}
	v := Verdict{}
	switch strings.ToLower(m[1]) {
	case "yes", "true", "да":
		v.Match = true
	}
	if r := gemmaReasonRe.FindStringSubmatch(text); len(r) >= 2 {
		v.Reason = strings.Trim(strings.TrimSpace(r[1]), "\"'`")
	}
	if v.Match && gemmaPlaceholderRe.MatchString(v.Reason) {
		return Verdict{}, fmt.Errorf("gemini: reason looks like prompt template (echo): %q", truncate(v.Reason, 200))
	}
	return v, nil
}

// extractJSONObject pulls the first balanced {...} span out of the model's
// response. Used only for the Gemini path (ResponseSchema-enforced JSON).
// Walking the string with a brace counter (respecting string literals) is
// resilient to ```json fences or stray prose around the object.
func extractJSONObject(resp *genai.GenerateContentResponse) (string, error) {
	text, err := extractResponseText(resp)
	if err != nil {
		return "", err
	}

	start := strings.Index(text, "{")
	if start < 0 {
		return "", fmt.Errorf("gemini: no JSON object in response: %q", truncate(text, 200))
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("gemini: unbalanced JSON object in response: %q", truncate(text, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
