# tg-vacancy-filter

A Telegram **userbot** in Go that watches job channels, scores every post
against a candidate profile with Gemini, and delivers the matches — with the
score and the model's reasoning — to a chat of your choice.

- **MTProto client:** [`github.com/gotd/td`](https://github.com/gotd/td)
- **Model:** [`github.com/google/generative-ai-go/genai`](https://github.com/google/generative-ai-go)
- **Config:** [`github.com/joho/godotenv`](https://github.com/joho/godotenv)

It runs unattended: no terminal, no laptop left open. The shipped GitHub
Actions workflow polls every 30 minutes on the free tier.

---

## How it works

```
                 ┌─ prefilter ─┐        ┌─ score ≥ threshold ─┐
poll / update ──▶│ is it a     │──▶ LLM │ and remote != office│──▶ destination chat
                 │ vacancy?    │        └─────────────────────┘         +
                 │ seen before?│                                    matches.jsonl
                 └─────────────┘
```

1. Posts are read either by polling (`--once`) or from live MTProto updates.
2. **Prefilter** (`internal/filter`) drops anything too short, anything with no
   hiring vocabulary, and any post whose normalised-text fingerprint was
   already analysed. Cross-posting means one vacancy typically appears in
   several channels; only the first copy costs a model call.
3. The surviving post goes to the model together with the **candidate profile**
   read from `PROFILE_PATH`. The model returns a structured verdict:

   ```json
   {"score": 82, "role": "Junior Python Backend", "stack": ["FastAPI", "PostgreSQL"],
    "seniority": "Junior", "remote": "remote", "location": "Worldwide",
    "summary": "…", "pros": ["…"], "cons": ["…"]}
   ```

   Gemini-family models get this enforced server-side via `ResponseSchema`;
   Gemma models are asked for the same JSON in prose and parsed with a brace
   scanner.
4. A post is forwarded when `score >= MATCH_THRESHOLD` **and** `remote` is not
   `onsite`/`hybrid`. The remote rule lives in code, not in the prompt — a
   borderline post cannot talk the model out of a hard constraint.
5. Matches are appended to `MATCH_LOG_PATH` and sent to `DESTINATION`.

**Tuning the filter is editing `profile/candidate.md`.** No Go code involved.

---

## Project layout

```
tg-vacancy-filter/
├── main.go                       # flags: --once, --doctor, --doctor-send
├── profile/candidate.md          # who the vacancies are for — edit this
├── internal/
│   ├── app/
│   │   ├── app.go                # wiring; live mode and poll mode
│   │   └── doctor.go             # preflight report
│   ├── config/    config.go      # env parsing and validation
│   ├── filter/    prefilter.go   # non-vacancy drop + repost fingerprint
│   ├── gemini/    analyzer.go    # scoring, response schema, quota failover
│   └── telegram/
│       ├── auth.go               # interactive login (refuses to hang without a tty)
│       ├── session.go            # session source: file / string / base64
│       ├── dialogs.go            # paginated channel resolution
│       ├── history.go            # getHistory by date or by cursor
│       ├── poller.go             # --once: cursor, bootstrap sweep, time budget
│       ├── process.go            # shared post pipeline
│       ├── handler.go            # live update path
│       ├── invite.go             # t.me/+xxx invite resolver
│       ├── matchlog.go           # JSONL audit trail
│       └── sender.go             # notification format, pacing, FLOOD_WAIT
└── .github/workflows/poll.yml    # the free scheduled deploy
```

Generated runtime files:

| File            | Committed | Purpose                                                  |
| --------------- | :-------: | -------------------------------------------------------- |
| `state.json`    |    yes    | Poll cursor per channel + fingerprints of analysed posts. |
| `session.json`  |    no     | MTProto session — full account access, treat as a secret. |
| `matches.jsonl` |    no     | Every match; contains the text of source posts.           |

---

## Prerequisites

1. **Telegram API credentials** — <https://my.telegram.org/apps>.
2. **Gemini API key** — <https://aistudio.google.com/apikey>.
3. **Channel IDs** — forward a post to [@userinfobot](https://t.me/userinfobot)
   and read the `-100xxxxxxxxxx` id. The userbot account must **already be a
   member** of every source channel; MTProto only serves chats you are in.
4. **A destination.** `me` (Saved Messages), a username, a numeric channel id
   (`-1001234567890`), or a `t.me/+…` invite link. For a private channel you
   created yourself there is no username, so use the numeric id — the bot
   resolves it through the account's own dialog list.

---

## Quick start

```bash
cp .env.example .env
$EDITOR .env            # credentials, channels, destination
$EDITOR profile/candidate.md

go build -o bot .
./bot -doctor           # check everything before the first real run
./bot -once             # one polling pass
```

`-doctor` is the fastest way to find a broken setup. It prints:

```
=== gemini ===
  ✓ api key valid — 47 models support generateContent
  ✓ primary  gemini-3.5-flash-lite
  ✓ fallback gemma-4-26b-a4b-it
=== telegram ===
  session from   TG_STRING_SESSION
  ✓ signed in as id=123456789 username=@someaccount name="…"
  ✓ destination "aslkhn" resolved
=== source channels ===
  ✓ 1096154976    Dev KZ | Vacancy                    @devkz_jobs
  ✗ 1431840960    not in this account's dialogs — join the channel first
  14/16 reachable
```

Add `-doctor-send` to also post a test message — the only real proof that the
account can write to the destination.

---

## Modes

| Command                  | Behaviour                                                        |
| ------------------------ | ---------------------------------------------------------------- |
| `./bot -once`            | Poll every channel once, then exit. Used by the scheduled deploy. |
| `./bot -once -dry-run`   | Same, but sends nothing — scores and logs only. Calibration mode. |
| `./bot`                  | Stay connected and react to live updates. For an always-on host.  |
| `./bot -doctor`          | Preflight checks, then exit.                                      |
| `./bot -folders=all`     | List chat folders and the channel ids in each.                    |

### Filling SOURCE_CHANNEL_IDS from a chat folder

Curating the watch list in the Telegram app is much easier than collecting ids
by hand. Put the channels into a folder, then:

```bash
./bot -folders=vacancies
```

```
=== folder "vacancies" — 28 channels ===
  -1001096154976    Dev KZ | Vacancy                    @devkz_jobs
  -1001344577123    Backend Job Offers                  @runello_rus_backend
  ...

SOURCE_CHANNEL_IDS=-1001096154976,-1001344577123,...
```

Paste the last line into `.env`.

### Calibrating the profile

`-dry-run` classifies real traffic and writes the verdict next to the post text
at `LOG_LEVEL=debug`, without notifying anyone:

```bash
POLL_STATE_PATH=/tmp/cal.json MATCH_LOG_PATH= LOG_LEVEL=debug \
  ./bot -once -dry-run
```

Read the scores. A vacancy that should have matched but did not is a line to
add to `profile/candidate.md`; a rejection you disagree with usually means a
stop-condition is firing too eagerly.

### The first poll sweeps history

A channel with no cursor in `state.json` is read from `POLL_BOOTSTRAP_SINCE`
(e.g. `2026-08-24`) rather than from "now", so the first run picks up the
backlog instead of starting empty. After that the cursor takes over and only
new posts are fetched.

A two-week backlog across many channels is more work than one scheduled job
should attempt. `POLL_MAX_RUNTIME` bounds each run: on expiry the cursor is
saved and the process exits with status 0, so the next scheduled run continues
where this one stopped. The backlog drains over a few hours with no manual
step and no failed jobs.

---

## Configuration

Full list with comments in [`.env.example`](./.env.example). The ones that matter:

| Variable                | Required | Default                  | Notes                                                        |
| ----------------------- | :------: | ------------------------ | ------------------------------------------------------------ |
| `TG_APP_ID`             |    ✅    |                          | Integer from my.telegram.org.                                 |
| `TG_APP_HASH`           |    ✅    |                          | 32-char hex string.                                           |
| `TG_PHONE`              |    ✅    |                          | The account acting as the userbot.                            |
| `TG_STRING_SESSION`     |          |                          | Telethon StringSession — the unattended-auth path.            |
| `SOURCE_CHANNEL_IDS`    |    ✅    |                          | Comma-separated; `-100` prefix optional.                      |
| `DESTINATION`           |    ✅    |                          | `me`, a username, a numeric channel id, or a `t.me/+…` link.   |
| `GEMINI_API_KEY`        |    ✅    |                          | From Google AI Studio.                                        |
| `PROFILE_PATH`          |          | `profile/candidate.md`   | The candidate description injected into the prompt.           |
| `MATCH_THRESHOLD`       |          | `60`                     | Minimum score to forward.                                     |
| `GEMINI_MODEL`          |          | `gemini-3.5-flash-lite`  | Verify with `-doctor`; the free lineup changes.               |
| `GEMINI_MODEL_FALLBACK` |          | `gemma-4-26b-a4b-it`     | Takes over when the primary's daily quota runs out.           |
| `GEMINI_RPM`            |          | `12`                     | Client-side ceiling. `0` disables.                            |
| `POLL_STATE_PATH`       |          | `state.json`             | Cursor + fingerprints. Must persist between runs.             |
| `POLL_BOOTSTRAP_SINCE`  |          |                          | `YYYY-MM-DD` UTC; how far back a channel's first poll reaches.|
| `POLL_MAX_RUNTIME`      |          | `20m`                    | Budget for one `-once` run.                                   |
| `MATCH_LOG_PATH`        |          | `matches.jsonl`          | Empty disables. Contains source post text.                    |
| `LOG_LEVEL`             |          | `info`                   | `debug` prints the score of every post, matched or not.       |

### Model selection

Run `./bot -doctor` — it lists the models your key actually serves and flags
whether your configured names are among them. Google retires free-tier models
regularly, so a hardcoded recommendation ages badly.

The rule of thumb: a **Gemini-family** model as `GEMINI_MODEL` (the response
schema is enforced server-side, so verdicts are always parseable), and a
**Gemma** model as `GEMINI_MODEL_FALLBACK` (much higher requests-per-day, which
is what lets a long backlog finish after the primary model's cap is hit).

**Free-tier caps are per model and vary wildly.** `gemini-2.5-flash-lite`
allows 20 requests *per day*; a newer sibling such as `gemini-3.5-flash-lite`
has its own, far larger allowance. If a run stalls in backoff immediately,
that model is spent — switch to another from the `-doctor` list rather than
lowering `GEMINI_RPM`. A 429 whose retry hint exceeds 30s is treated as a
quota wall and trips the fallback at once instead of sleeping through three
retries.

---

## Unattended authentication

The bot never prompts for a login code on a host without a terminal — it fails
with a clear message instead of hanging forever. Give it a session:

**Telethon StringSession (recommended).** Generate once, locally, with the
**same `api_id`** the bot uses:

```python
# pip install telethon
from telethon.sync import TelegramClient
from telethon.sessions import StringSession

with TelegramClient(StringSession(), API_ID, API_HASH) as client:
    print(client.session.save())
```

Put the result in `TG_STRING_SESSION`. The api_id must match: a string created
with a different app can be rejected as `AUTH_KEY_UNREGISTERED`.

**Or a session file.** Run `./bot` once locally, answer the code prompt, and
either keep `session.json` on a volume or ship it as
`TG_SESSION_BASE64=$(base64 -i session.json | tr -d '\n')`.

An existing `session.json` always wins over both env vars.

> Treat all three like a password — each grants full access to the account.

---

## Deploy: GitHub Actions (free)

[`.github/workflows/poll.yml`](.github/workflows/poll.yml) runs `./bot -once`
every 30 minutes. On a **public** repository Actions minutes are unlimited and
free; on a private one the free allowance is 2000 minutes/month, so widen the
cron to hourly there.

```bash
gh secret set TG_APP_ID
gh secret set TG_APP_HASH
gh secret set TG_PHONE
gh secret set TG_STRING_SESSION
gh secret set GEMINI_API_KEY
gh secret set SOURCE_CHANNEL_IDS
gh secret set DESTINATION

# optional, non-secret knobs
gh variable set GEMINI_MODEL --body "gemini-3.5-flash-lite"
gh variable set GEMINI_MODEL_FALLBACK --body "gemma-4-26b-a4b-it"
gh variable set MATCH_THRESHOLD --body "60"

gh workflow run poll.yml     # first run, then watch it
gh run watch
```

Design notes:

- **Only `schedule` and `workflow_dispatch` triggers.** A `pull_request`
  trigger would hand the Telegram session to anyone opening a PR.
- **`state.json` is committed back** after each run. That is both the cursor
  persistence and what keeps the schedule alive — GitHub disables scheduled
  workflows after 60 days without repository activity.
- **`concurrency: poll`** prevents two runs from fighting over the cursor.
- **`MATCH_LOG_PATH=""`** in the workflow: `matches.jsonl` contains the full
  text of source posts and has no business in a public repository.
- Cron is best-effort; a delayed or skipped run loses nothing, because the
  cursor decides what is new, not the clock.

### Alternative: always-on VM

For real-time delivery instead of 30-minute batches, run the live mode on any
small host — **Oracle Cloud Always Free** (ARM Ampere, permanent, card needed
for signup) or **Google Cloud `e2-micro`**:

```bash
docker build -t tg-vacancy-filter .
docker run -d --name tg-bot --restart unless-stopped \
  -v ~/bot-data:/data \
  -e SESSION_PATH=/data/session.json \
  -e POLL_STATE_PATH=/data/state.json \
  -e MATCH_LOG_PATH=/data/matches.jsonl \
  --env-file ~/bot.env \
  tg-vacancy-filter
```

The bot only makes outbound calls, so no ingress rules are needed.

---

## Rate limiting, retries and quota

- A token-bucket limiter (`GEMINI_RPM`) paces every model call.
- Up to 3 retries on `RESOURCE_EXHAUSTED`, honouring the server's
  `retry in Xs` hint, else exponential backoff from 10s.
- When those are exhausted the analyser switches to `GEMINI_MODEL_FALLBACK`
  once per process and logs a warning. This is what lets a bootstrap sweep
  finish after the primary model's daily cap.
- Notifications are paced 1.5s apart and retried once on `FLOOD_WAIT` — a
  bootstrap can produce dozens of matches back to back.
- A channel that fails to fetch is logged and skipped; the run continues.

`LOG_LEVEL=debug` prints the score and reasoning for every post, including
rejected ones. That is the tool for calibrating `MATCH_THRESHOLD` and the
profile.

---

## Security notes

- `session.json` and `TG_STRING_SESSION` grant **full access** to the Telegram
  account. Never commit them; rotate via "Active sessions" if leaked.
- `matches.jsonl` contains private post text — gitignored, keep it that way.
- `profile/candidate.md` is committed. Keep phone numbers and email addresses
  out of it.
- Userbots are permitted, but abusing the API (flooding, mass DMs, scraping)
  can get an account banned. This bot is pull-only and paced.
- Model calls include the post text. Don't point it at channels whose content
  you are not allowed to share with a third party.

---

## Troubleshooting

| Symptom                                                       | Fix                                                                                              |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `API key not valid`                                           | Key revoked or from the wrong project. Issue a new one; `-doctor` confirms it.                    |
| `models/<x> is not found for API version v1beta`              | Google retired the model. `-doctor` lists what your key actually serves.                          |
| `telegram session missing and stdin is not a terminal`        | Working as intended: set `TG_STRING_SESSION`, or authenticate locally once.                       |
| `AUTH_KEY_UNREGISTERED` on boot                               | Session revoked, or the string session was made with a different `api_id`. Regenerate it.         |
| `PHONE_CODE_INVALID`                                          | The code arrives in the **Telegram app**, not by SMS — check Saved Messages.                       |
| Channel listed as "not in this account's dialogs"             | The signed-in account is not a member. Join it, then re-run `-doctor`.                             |
| No matches at all                                             | `LOG_LEVEL=debug` and read the scores. Usually the profile is too strict or the threshold too high.|
| Too many irrelevant matches                                   | Raise `MATCH_THRESHOLD`, or add the unwanted role to the "не подходит" list in the profile.        |
| `RESOURCE_EXHAUSTED` bursts                                   | Lower `GEMINI_RPM`, or wait for the UTC-midnight reset. The fallback model covers most of it.      |
| Scheduled workflow stopped running                            | GitHub disables cron after 60 days of inactivity. The state commit normally prevents this.         |
