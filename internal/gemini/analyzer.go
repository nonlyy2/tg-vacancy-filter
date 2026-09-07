// Package gemini wraps the Google Generative AI SDK with an analyser that
// scores Telegram job posts against a candidate profile loaded from disk.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// maxRetries429 bounds how many times Analyze retries one model on
// RESOURCE_EXHAUSTED before giving up (or switching to the fallback model).
const maxRetries429 = 3

// quotaWallHint is the retry delay above which a 429 is treated as a spent
// quota rather than a burst. Free-tier daily caps come back with hints of a
// minute or more and never clear within a run, so sleeping through three of
// them wastes minutes of a scheduled job — switch to the fallback at once.
const quotaWallHint = 30 * time.Second

// Remote formats the model is asked to classify a post into.
const (
	RemoteYes     = "remote"
	RemoteHybrid  = "hybrid"
	RemoteOnsite  = "onsite"
	RemoteUnknown = "unknown"
)

// retryAfterRe pulls the "retry in 38.341s" hint out of the googleapi error
// message. The SDK also exposes this via googleapi.Error.Details, but parsing
// the structured proto is heavier than a regex over the already-formatted text.
var retryAfterRe = regexp.MustCompile(`retry in (\d+(?:\.\d+)?)s`)

// instructions is the task description. The candidate-specific part is not
// here — it is loaded from PROFILE_PATH and appended at construction, so
// retuning the filter never means editing Go code.
const instructions = `Ты — ассистент по поиску работы. Оцениваешь, насколько пост из
Telegram-канала подходит одному конкретному кандидату.

Посты приходят на русском, казахском или английском — обрабатывай их одинаково.

АЛГОРИТМ:

1. Определи, вакансия ли это вообще. Новости, статьи, мемы, опросы, анонсы
   мероприятий, рефералки без описания роли — это не вакансия, score = 0.

   Отдельно: резюме соискателя — это НЕ вакансия, score = 0. Признак: автор
   описывает СВОЙ опыт и ищет работу ("ищу работу", "рассмотрю предложения",
   "мой стек", "опыт 3 года", ссылка на своё резюме), а не описывает позицию
   в компании. Такие посты приходят из каналов с резюме и легко путаются с
   вакансиями, потому что содержат тот же словарь.

2. Если пост содержит несколько разных вакансий, оценивай ЛУЧШУЮ из них для
   кандидата и в поле "role" укажи именно её.

3. Извлеки из поста: название роли, стек, уровень (seniority), город, формат
   работы. Бери ТОЛЬКО то, что написано в посте — ничего не додумывай.

4. Поле "remote" заполняй строго одним из значений:
   - "remote"  — явно указана полностью удалённая работа / удалёнка / remote
   - "hybrid"  — гибрид, частично офис, N дней в офисе
   - "onsite"  — работа в офисе, требуется присутствие, релокация обязательна
   - "unknown" — формат работы в посте не указан

5. СТОП-УСЛОВИЯ. Проверь их ДО того, как ставить score. Если сработало хотя бы
   одно — score НЕ МОЖЕТ быть выше указанного потолка, независимо от того,
   насколько хорошо совпало всё остальное:

   - это не вакансия                                        -> score = 0
   - роль не инженерная (см. профиль)                       -> score <= 10
   - формат работы ОФИС или ГИБРИД                          -> score <= 15
   - требуемый уровень СТРОГО выше профиля кандидата
     (Senior / Lead / Head / Principal / Staff)             -> score <= 15
   - требуемый опыт больше, чем есть у кандидата
     (например "от 3 лет", "5+ years")                      -> score <= 15
   - основной язык вакансии не из стека кандидата, а его
     язык упомянут лишь как "будет плюсом"                  -> score <= 25

   Потолок применяется даже если стек, город и всё прочее идеально совпали.
   Требование "от 3 лет опыта" в вакансии на Go — это стоп-условие, а не
   мелкий недостаток.

   ЧТО НЕ ЯВЛЯЕТСЯ СТОП-УСЛОВИЕМ:
   - грейд Middle сам по себе. Middle входит в диапазон кандидата. Потолок 15
     применяй к Middle-вакансии ТОЛЬКО если в ней отдельно требуют 3+ лет
     опыта. Если требований по годам нет — оценивай по стеку и формату,
     нормальный результат для подходящей Middle-вакансии 60-80.
   - remote = "unknown" (формат работы в посте не указан). Это НЕ отказ.
     Оцени вакансию по стеку и уровню как обычно, а в "cons" напиши, что
     формат работы не указан и его надо уточнить. Многие подходящие
     удалённые вакансии просто не пишут слово "удалённо".
   - отсутствие вилки зарплаты, названия компании или списка бенефитов.

6. Если ни одно стоп-условие не сработало, поставь score от 0 до 100:
   - 80-100: стек и уровень совпадают, удалённый формат подтверждён
   - 60-79 : подходит, есть небольшие пробелы — кандидату стоит откликнуться
   - 40-59 : частичное совпадение, значимые пробелы
   - 5-39  : не подходит

   Порог отклика — 60. Не ставь 60 и выше, если сам не считаешь, что кандидату
   стоит потратить время на этот отклик.

   score = 0 означает РОВНО ОДНО: это не вакансия. Настоящая вакансия, которая
   кандидату не подходит, получает 5-39, а не 0. Не используй 0 как общий отказ.

   Оплачиваемая стажировка или trainee-позиция по стеку кандидата — это
   реальная вакансия низкого приоритета: 40-60, а не 0.

7. Заполни "pros" и "cons" КОНКРЕТИКОЙ из поста: не "хороший стек", а
   "FastAPI + PostgreSQL". В "cons" пиши пробелы даже у сильных совпадений —
   кандидат читает их перед откликом.

ВАЖНО:
- Оценивай только то, что написано в посте. Не додумывай опыт и условия.
- Все текстовые поля ("role", "seniority", "location", "summary", "pros",
  "cons") заполняй ТОЛЬКО на русском языке.
- Поле "remote" — одно из четырёх английских значений выше.`

// jsonContract is appended for models that cannot be given a response schema
// server-side (Gemma). Gemini-family models get the same shape enforced via
// ResponseSchema instead.
const jsonContract = `ФОРМАТ ОТВЕТА:
Верни ТОЛЬКО JSON-объект, без markdown, без пояснений вне JSON:
{"score": 0, "role": "", "stack": [], "seniority": "", "remote": "unknown",
 "location": "", "summary": "", "pros": [], "cons": []}`

// Verdict is the model's assessment of a single post.
type Verdict struct {
	Score     int      `json:"score"`
	Role      string   `json:"role"`
	Stack     []string `json:"stack"`
	Seniority string   `json:"seniority"`
	Remote    string   `json:"remote"`
	Location  string   `json:"location"`
	Summary   string   `json:"summary"`
	Pros      []string `json:"pros"`
	Cons      []string `json:"cons"`
}

// Matches reports whether a post should be forwarded. The remote-only rule is
// enforced here rather than left to the prompt: it is the candidate's one hard
// constraint, and a borderline post must not be able to talk the model out of it.
func (v Verdict) Matches(threshold int) bool {
	if v.Remote == RemoteOnsite || v.Remote == RemoteHybrid {
		return false
	}
	return v.Score >= threshold
}

// Analyzer scores posts against a candidate profile, with a rate limiter and
// an optional fallback model for when the primary model's quota runs out.
type Analyzer struct {
	client *genai.Client

	primary      *genai.GenerativeModel
	primaryName  string
	fallback     *genai.GenerativeModel
	fallbackName string

	limiter *rate.Limiter
	prompt  string

	// mu guards usingFallback, which flips once per process and is read on
	// every Analyze call — the backfill path may run concurrently with live
	// updates.
	mu            sync.Mutex
	usingFallback bool

	onRetry    func(attempt int, wait time.Duration, err error)
	onFallback func(from, to string)
}

// New constructs an Analyzer. profile is the candidate description injected
// into the prompt. fallbackModel may be empty to disable quota failover.
// rpm is the request-per-minute ceiling; 0 disables the limiter.
func New(ctx context.Context, apiKey, modelName, fallbackModel, profile string, rpm int) (*Analyzer, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("gemini: api key is empty")
	}
	if strings.TrimSpace(profile) == "" {
		return nil, errors.New("gemini: candidate profile is empty")
	}
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("gemini: new client: %w", err)
	}

	a := &Analyzer{
		client:      client,
		primaryName: modelName,
		prompt:      instructions + "\n\n=== ПРОФИЛЬ КАНДИДАТА ===\n" + strings.TrimSpace(profile),
	}
	a.primary = a.newModel(modelName)
	if fallbackModel != "" && fallbackModel != modelName {
		a.fallbackName = fallbackModel
		a.fallback = a.newModel(fallbackModel)
	}

	if rpm > 0 {
		// Steady-state 1 request every (60/rpm) seconds. Burst=1 keeps us
		// well under the quota even under retry storms.
		a.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), 1)
	}
	return a, nil
}

// newModel configures one model handle. Gemma rejects SystemInstruction and
// ResponseSchema at the API layer, so for that family the prompt is inlined
// into the user turn and the JSON contract is stated in prose instead.
func (a *Analyzer) newModel(name string) *genai.GenerativeModel {
	m := a.client.GenerativeModel(name)

	if !isGemma(name) {
		m.SystemInstruction = &genai.Content{
			Parts: []genai.Part{genai.Text(a.prompt)},
		}
		m.ResponseMIMEType = "application/json"
		m.ResponseSchema = verdictSchema()
	}

	// Low temperature -> stable, comparable scores for near-identical posts.
	temp := float32(0.1)
	m.Temperature = &temp

	// Harm blocking off — job posts occasionally include salary/demographic
	// phrases that upstream filters misclassify. Output goes to the user's
	// own chat, not anywhere public.
	m.SafetySettings = []*genai.SafetySetting{
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockNone},
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockNone},
	}
	return m
}

func verdictSchema() *genai.Schema {
	strs := &genai.Schema{Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}}
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"score":     {Type: genai.TypeInteger, Description: "0-100"},
			"role":      {Type: genai.TypeString},
			"stack":     strs,
			"seniority": {Type: genai.TypeString},
			"remote": {
				Type: genai.TypeString,
				Enum: []string{RemoteYes, RemoteHybrid, RemoteOnsite, RemoteUnknown},
			},
			"location": {Type: genai.TypeString},
			"summary":  {Type: genai.TypeString},
			"pros":     strs,
			"cons":     strs,
		},
		Required: []string{"score", "remote", "summary"},
	}
}

// Close releases gRPC resources held by the underlying client.
func (a *Analyzer) Close() error {
	if a == nil || a.client == nil {
		return nil
	}
	return a.client.Close()
}

// SetRetryHook registers a callback fired before each 429 sleep.
func (a *Analyzer) SetRetryHook(fn func(attempt int, wait time.Duration, err error)) {
	a.onRetry = fn
}

// SetFallbackHook registers a callback fired once, when the analyser gives up
// on the primary model and switches to the fallback for the rest of the run.
func (a *Analyzer) SetFallbackHook(fn func(from, to string)) {
	a.onFallback = fn
}

// Model returns the model currently in use.
func (a *Analyzer) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usingFallback {
		return a.fallbackName
	}
	return a.primaryName
}

// Analyze scores one post. It blocks on the rate limiter, retries up to
// maxRetries429 on RESOURCE_EXHAUSTED honouring the server's "retry in Xs"
// hint, and switches to the fallback model when the primary's quota is spent.
func (a *Analyzer) Analyze(ctx context.Context, postText string) (Verdict, error) {
	postText = strings.TrimSpace(postText)
	if postText == "" {
		return Verdict{}, errors.New("gemini: empty post")
	}

	var resp *genai.GenerateContentResponse
	for attempt := 0; ; attempt++ {
		if a.limiter != nil {
			if err := a.limiter.Wait(ctx); err != nil {
				return Verdict{}, fmt.Errorf("gemini: rate limit wait: %w", err)
			}
		}

		model, name := a.current()

		var err error
		resp, err = model.GenerateContent(ctx, genai.Text(a.buildInput(name, postText)))
		if err == nil {
			break
		}
		if !isQuotaExceeded(err) {
			return Verdict{}, fmt.Errorf("gemini: generate: %w", err)
		}
		wait := parseRetryAfter(err)
		if wait <= 0 {
			// No hint: exponential backoff starting at 10s.
			wait = time.Duration(1<<attempt) * 10 * time.Second
		}

		// Either the retry budget is spent, or the server is telling us this
		// is a wall rather than a burst. Both mean: stop waiting on this model.
		if attempt >= maxRetries429 || wait >= quotaWallHint {
			if !a.switchToFallback() {
				if attempt >= maxRetries429 {
					return Verdict{}, fmt.Errorf("gemini: generate: %w", err)
				}
			} else {
				attempt = -1 // restart the retry budget on the fallback model
				continue
			}
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

	raw, err := extractJSONObject(resp)
	if err != nil {
		return Verdict{}, err
	}

	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("gemini: decode %q: %w", truncate(raw, 200), err)
	}
	return Normalize(v), nil
}

// Normalize clamps the score into range and maps free-form remote values onto
// the four known constants. Exported so tests can exercise it directly.
func Normalize(v Verdict) Verdict {
	if v.Score < 0 {
		v.Score = 0
	}
	if v.Score > 100 {
		v.Score = 100
	}
	switch strings.ToLower(strings.TrimSpace(v.Remote)) {
	case RemoteYes, "удалённо", "удаленно", "удалёнка", "удаленка":
		v.Remote = RemoteYes
	case RemoteHybrid, "гибрид":
		v.Remote = RemoteHybrid
	case RemoteOnsite, "on-site", "office", "офис":
		v.Remote = RemoteOnsite
	default:
		v.Remote = RemoteUnknown
	}
	return v
}

// buildInput assembles the user turn. Gemini-family models already carry the
// prompt as a system instruction, so they only receive the post.
func (a *Analyzer) buildInput(modelName, postText string) string {
	if !isGemma(modelName) {
		return "ПОСТ:\n" + postText
	}
	return a.prompt + "\n\n" + jsonContract + "\n\n---\nПОСТ:\n" + postText +
		"\n---\n\nВерни только JSON."
}

func (a *Analyzer) current() (*genai.GenerativeModel, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usingFallback {
		return a.fallback, a.fallbackName
	}
	return a.primary, a.primaryName
}

// switchToFallback flips to the fallback model. Returns false when there is no
// fallback configured or the switch already happened.
func (a *Analyzer) switchToFallback() bool {
	a.mu.Lock()
	if a.fallback == nil || a.usingFallback {
		a.mu.Unlock()
		return false
	}
	a.usingFallback = true
	from, to := a.primaryName, a.fallbackName
	a.mu.Unlock()

	if a.onFallback != nil {
		a.onFallback(from, to)
	}
	return true
}

func isGemma(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "gemma")
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

// extractJSONObject pulls the first balanced {...} span out of the model's
// response. Walking the string with a brace counter (respecting string
// literals) is resilient to ```json fences or stray prose around the object,
// which Gemma still emits occasionally.
func extractJSONObject(resp *genai.GenerateContentResponse) (string, error) {
	text, err := extractResponseText(resp)
	if err != nil {
		return "", err
	}
	return ExtractJSONObject(text)
}

// ExtractJSONObject is the string-level half of extractJSONObject, split out
// so it can be tested without constructing SDK response types.
func ExtractJSONObject(text string) (string, error) {
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
