# tg-vacancy-filter

A small Telegram **userbot** written in Go that watches a list of job channels,
asks **Gemini / Gemma** whether each post fits a fixed candidate profile
(Junior / Intern Go · remote or Astana), and forwards the matches to a
destination chat of your choice.

- **MTProto client:** [`github.com/gotd/td`](https://github.com/gotd/td)
- **AI filter:** [`github.com/google/generative-ai-go/genai`](https://github.com/google/generative-ai-go) — supports both Gemini (structured JSON) and Gemma (text format) families
- **Config:** [`github.com/joho/godotenv`](https://github.com/joho/godotenv)

The default model is **`gemma-4-26b-a4b-it`** because, as of May 2026, it is
the only free-tier model with a daily quota (≈14 400 RPD) large enough to
backfill a multi-week sweep over 20+ channels in one go. Switch
`GEMINI_MODEL` to a Gemini variant for stricter outputs at the cost of
tighter daily caps — see [Model selection](#model-selection).

---

## How it works

```
Telegram update ──▶ dispatcher ──▶ is source channel? ──▶ analyser ──▶ Gemini family ─▶ JSON via ResponseSchema
                                                                  │
                                                                  └─▶ Gemma  family ─▶ "MATCH: yes/no \n REASON: ..." text
                                                          match=true │
                                                                     ▼
                                                              destination chat
                                                                     +
                                                              matches.jsonl
```

1. `gotd` streams channel updates into an `UpdateDispatcher`.
2. Posts from channels not in `SOURCE_CHANNEL_IDS` are dropped.
3. Each remaining post is sent to the configured model:
   - **Gemini family** (`gemini-*`): the SDK enforces a strict JSON schema
     `{"match": bool, "reason": string}` via `ResponseSchema`.
   - **Gemma family** (`gemma-*`): Gemma rejects `SystemInstruction` and
     `ResponseSchema` at the API layer, so the analyser inlines the prompt
     and asks for a two-line text reply (`MATCH: yes\nREASON: ...`),
     parsed with a regex. JSON would be ideal, but Gemma 4 has a tendency
     to literally echo `{"match": boolean, "reason": string}` on short or
     ambiguous posts — switching to plain text eliminates that failure mode.
4. Calls are throttled by an in-process token-bucket limiter
   (`golang.org/x/time/rate`) configured from `GEMINI_RPM`. Both the
   live and backfill paths share one limiter, so an in-flight backfill
   naturally slows live posts instead of pushing you over quota.
5. If `match == true`, the bot:
   - appends a record to `MATCH_LOG_PATH` (defaults to `matches.jsonl`),
   - sends a notification to `DESTINATION` with the reason and a `t.me/...`
     link to the original post.
6. **History backfill (optional):** on boot, if `BACKFILL_SINCE` is set, the
   bot pages `messages.getHistory` for every source channel and replays
   posts from that date through the same pipeline before going live.

---

## Project layout

```
tg-vacancy-filter/
├── main.go                     # entrypoint, signal handling
├── internal/
│   ├── app/         app.go     # wiring: config + client + dispatcher + analyser
│   ├── config/      config.go  # env parsing, validation, session base64 restore
│   ├── gemini/      analyzer.go# rate-limited model client; JSON or text output
│   └── telegram/
│       ├── auth.go             # terminal-based login (code / 2FA)
│       ├── backfill.go         # messages.getHistory sweep + persistent state
│       ├── dialogs.go          # resolve channel access hashes via getDialogs
│       ├── handler.go          # update -> filter -> notify
│       ├── history.go          # paginated getHistory with date cutoff
│       ├── invite.go           # t.me/+xxx invite link resolver
│       ├── matchlog.go         # append-only JSONL audit trail of matches
│       └── sender.go           # composes notification, builds post links
├── Dockerfile
├── .env.example
└── .gitignore
```

Generated runtime files — **all gitignored**:

| File                      | Purpose                                                   |
| ------------------------- | --------------------------------------------------------- |
| `session.json`            | MTProto session — full account access, treat as a secret. |
| `backfill_state.json`     | Resume cursor for the history sweep.                      |
| `matches.jsonl`           | Append-only log of every `match=true` verdict.            |

---

## Prerequisites

1. **Telegram API credentials.** Go to <https://my.telegram.org/apps> and create
   an app — keep the `api_id` and `api_hash`.
2. **Gemini API key.** Generate at <https://aistudio.google.com/apikey>.
3. **Channel IDs.** Forward any message from a source channel to
   [@userinfobot](https://t.me/userinfobot) or [@getidsbot](https://t.me/getidsbot)
   to read its numeric id (format `-100xxxxxxxxxx`).
4. **Destination.** Three shapes are supported:
   - `DESTINATION=me` — matches land in the userbot's **Saved Messages**.
     Simplest; no channel needed.
   - `DESTINATION=my_vacancies_feed` — username of a public channel / user
     the userbot account can reach.
   - `DESTINATION=https://t.me/+AbCdEf123` — **invite link** of a private
     channel. On first boot the userbot calls `messages.checkChatInvite`; if
     it is not yet a member it auto-joins via `messages.importChatInvite`.
     Legacy `t.me/joinchat/…` links work too.
5. The userbot account must **already be joined** to every source channel;
   MTProto only delivers updates for chats the account participates in.

---

## Quick start (local)

```bash
# 1. Initialise modules (first time only — the repo already ships go.mod).
go mod tidy

# 2. Copy the example env file and fill it in.
cp .env.example .env
$EDITOR .env

# 3. Run. The first boot prompts for the Telegram login code (and
#    2FA password if enabled). Subsequent runs reuse session.json.
go run .
```

To build a native binary:

```bash
go build -o bin/bot .
./bin/bot
```

Stop with `Ctrl+C` — the process flushes state and closes the MTProto
connection cleanly.

---

## Configuration reference

All variables live in `.env` (or the host's environment). See
[`.env.example`](./.env.example) for the full list; the important ones:

| Variable                  | Required | Default                | Notes                                                   |
| ------------------------- | :------: | ---------------------- | ------------------------------------------------------- |
| `TG_APP_ID`               |    ✅    |                        | Integer from my.telegram.org.                           |
| `TG_APP_HASH`             |    ✅    |                        | 32-char hex string.                                     |
| `TG_PHONE`                |    ✅    |                        | `+7...` — account that acts as the userbot.             |
| `SOURCE_CHANNEL_IDS`      |    ✅    |                        | Comma-separated; `-100` prefix is optional.             |
| `DESTINATION`             |    ✅    |                        | `me`, a username, or a `t.me/+...` invite link.         |
| `GEMINI_API_KEY`          |    ✅    |                        | From Google AI Studio.                                  |
| `GEMINI_MODEL`            |          | `gemma-4-26b-a4b-it`   | See [Model selection](#model-selection).                |
| `GEMINI_RPM`              |          | `25`                   | Client-side rate ceiling. `0` disables the limiter.     |
| `SESSION_PATH`            |          | `session.json`         | Keep secret.                                            |
| `TG_SESSION_BASE64`       |          |                        | Base64 of `session.json` — see [Deploy](#deploy).       |
| `MAX_MESSAGE_AGE_SECONDS` |          | `900`                  | Skip live posts older than this at boot.                |
| `BACKFILL_SINCE`          |          |                        | `YYYY-MM-DD` UTC; one-time history sweep.               |
| `BACKFILL_STATE_PATH`     |          | `backfill_state.json`  | Resume cursor for backfill.                             |
| `MATCH_LOG_PATH`          |          | `matches.jsonl`        | Append-only audit log. Empty = disabled.                |
| `LOG_LEVEL`               |          | `info`                 | `debug` / `info` / `warn` / `error`.                    |

---

## Model selection

Free-tier limits per <https://ai.google.dev/gemini-api/docs/rate-limits>.
**These move around** — Google has cut Gemini Flash RPD twice in 2026 and
retired `gemma-3-27b-it` from `v1beta` entirely. Numbers below are accurate
as of **May 2026**; check the page if quota errors look surprising.

| Model                   | Family | Free RPM | Free RPD | Notes                                                   |
| ----------------------- | :----: | :------: | :------: | ------------------------------------------------------- |
| `gemma-4-26b-a4b-it`    | Gemma  |   30     |  14 400  | **Default.** Big enough RPD for multi-week backfills.   |
| `gemini-2.5-flash-lite` | Gemini |   15     |   ~1000  | Stricter JSON via ResponseSchema. Good for live-only.   |
| `gemini-2.5-flash`      | Gemini |    5     |    250   | Smarter, but quota too small for backfill.              |

**How to choose:**

- **Backfill across weeks of history?** Stay on Gemma. ~7000 messages
  through 20 channels can easily exceed 1000 model calls; only Gemma's
  14 400 RPD survives.
- **Live-only, no backfill?** Either family is fine. Gemini gives stricter
  output and slightly better edge-case judgement; Gemma is faster.
- **Paid tier?** Set `GEMINI_RPM=0` and pick whichever model you prefer —
  the limiter is the only RPM gate; quota becomes a billing question, not
  a runtime one.

The analyser detects the family from the model name prefix (`gemma*` vs
everything else) and switches output format automatically — no other code
changes are required to swap models.

Recommended `GEMINI_RPM` values:

| Plan                | Model                   | `GEMINI_RPM` |
| ------------------- | ----------------------- | ------------ |
| Free                | `gemma-4-26b-a4b-it`    | `25`         |
| Free                | `gemini-2.5-flash-lite` | `12`         |
| Free                | `gemini-2.5-flash`      | `4`          |
| Paid, low traffic   | any                     | `60`         |
| Paid, high traffic  | any                     | `0` (off)    |

Why not run at the quota ceiling? Google's accounting is slightly bursty
near the edge — keeping one spare request per minute smooths out
transient 429s, and the built-in retry handles the rest.

---

## History backfill

Live listening only covers posts that arrive **while the bot is running** —
Telegram does not replay weeks of updates to a reconnecting client. To catch
up on a historical window, set `BACKFILL_SINCE` to the earliest date you
care about:

```env
BACKFILL_SINCE=2026-04-17
```

On the next boot the bot will:

1. Resolve source-channel access hashes from the account's dialogs
   (`messages.getDialogs`).
2. For each channel, page `messages.getHistory` backwards until it reaches
   `BACKFILL_SINCE` or an ID it has already processed.
3. Replay the posts (oldest → newest) through the analyser, honouring
   `GEMINI_RPM`.
4. Append matches to `MATCH_LOG_PATH` and forward them to `DESTINATION`
   exactly like live posts.
5. Persist progress to `BACKFILL_STATE_PATH` every 10 analyses, so a crash
   resumes where it left off.

**Idempotency.** Leaving `BACKFILL_SINCE` set after a successful run is
safe: subsequent boots see `state.done = true` for that date and skip
immediately. To trigger a fresh sweep, **change the date** — the state
file's `since` field is part of the key, so a different date invalidates
the old cursor and starts over.

**Resuming a window.** If you previously backfilled `2026-04-01 → 2026-04-16`
and want to extend through today, set `BACKFILL_SINCE=2026-04-17` (one day
**after** the previous boot timestamp). Using `2026-04-16` would re-analyse
posts from the morning of the 16th that were already classified — at best
duplicate Gemini calls, at worst duplicate match notifications.

**Stateless hosts.** Without a persistent volume, `backfill_state.json`
disappears on redeploy and the sweep runs again. Either mount a volume for
`BACKFILL_STATE_PATH` or clear `BACKFILL_SINCE` once the first run finishes.

---

## Match log (`matches.jsonl`)

Every `match=true` verdict is appended as one JSON object per line:

```jsonl
{"ts":"2026-05-07T12:34:56Z","channel":"IT Jobs","channel_id":1944996511,"msg_id":348,"reason":"Junior Go разработчик, удалёнка","link":"https://t.me/c/1944996511/348","source":"backfill"}
```

This is a persistent audit trail independent of the Telegram send. If the
destination chat is later cleared, or a single send fails, the verdict is
not lost. Set `MATCH_LOG_PATH=` (empty) to disable. The file is
**gitignored** — it contains private post text by way of the `reason`
field.

---

## Rate limiting & retries

The analyser wraps every `GenerateContent` call with a token-bucket limiter
configured from `GEMINI_RPM`, plus transparent retry-on-429 that honours
the `retry in Xs` hint the API returns.

- Limiter: `rate.NewLimiter(rate.Every(60s/RPM), burst=1)`.
- Up to **3 retries** on `RESOURCE_EXHAUSTED` / 429, with the
  server-supplied wait time + 1s safety margin. Falls back to
  exponential `10s · 2^attempt` if no `retry in Xs` hint is present.
- A single channel error never aborts the whole backfill — the channel
  is logged and the next one starts.

Set `LOG_LEVEL=debug` to see every per-message verdict (including
non-matches) and any retry sleeps.

---

## Deploy

The shipped **Dockerfile** runs unchanged on any Docker-capable host.
The tricky bit everywhere is **persisting `session.json` and
`backfill_state.json`** — lose the session and the account has to
re-authenticate via SMS / 2FA on every cold boot; lose the backfill
state and your configured `BACKFILL_SINCE` may run again.

> ⚠️ **There is no truly free always-on PaaS in 2026.**
> Render removed its free Background Worker tier in 2024. Fly.io moved
> to pay-as-you-go (no persistent free allowance) in Oct 2024. Railway
> gives a one-time $5 trial then requires $5/mo. Koyeb's free web
> services sleep.
> For a genuinely free, always-on deploy, rent a free-tier VM:
> **Oracle Cloud Always Free** (recommended) or **Google Cloud `e2-micro`**.

### Oracle Cloud Always Free (recommended)

Oracle's Always-Free tier includes either 2 AMD `VM.Standard.E2.1.Micro`
instances (1/8 OCPU, 1 GB RAM each) **or** an ARM Ampere A1 instance with
up to 4 OCPU and 24 GB RAM. All permanent, no 12-month cliff. One-time
signup requires a credit card for verification — no charges.

```bash
# 1. Sign up at https://signup.cloud.oracle.com (pick a home region you'll
#    keep forever; you can't change it later). Wait for the account to
#    be approved (usually minutes).
#
# 2. In the console: Compute -> Instances -> Create Instance
#    - Shape:  "Ampere" (ARM, 1 OCPU, 6 GB RAM) is plenty, and easiest to get.
#              If ARM is out of capacity in your region, pick "AMD E2.1.Micro".
#    - Image:  Ubuntu 22.04 or 24.04 (Always Free eligible)
#    - Network: accept defaults; note the public IPv4.
#    - SSH:    upload your ~/.ssh/id_ed25519.pub (or generate one).
#
# 3. SSH in and install Docker:
ssh ubuntu@<public-ip>
sudo apt update && sudo apt install -y docker.io git
sudo usermod -aG docker ubuntu && exec sudo -u ubuntu -i   # re-login for group

# 4. Clone & build the image.
git clone https://github.com/<you>/tg-vacancy-filter.git
cd tg-vacancy-filter
docker build -t tg-vacancy-filter .

# 5. Create a host directory for session + backfill state (survives restarts).
mkdir -p ~/bot-data

# 6. First run — attach stdin/tty to enter the Telegram login code.
docker run --rm -it \
  -v ~/bot-data:/data \
  -e SESSION_PATH=/data/session.json \
  -e BACKFILL_STATE_PATH=/data/backfill_state.json \
  -e MATCH_LOG_PATH=/data/matches.jsonl \
  --env-file <(cat <<'EOF'
TG_APP_ID=...
TG_APP_HASH=...
TG_PHONE=+7...
SOURCE_CHANNEL_IDS=-1001111,-1002222
DESTINATION=https://t.me/+AbCdEf
GEMINI_API_KEY=AIza...
GEMINI_MODEL=gemma-4-26b-a4b-it
GEMINI_RPM=25
EOF
) tg-vacancy-filter
# Enter the code when prompted. Once you see "listening for channel posts",
# stop with Ctrl+C. session.json is now in ~/bot-data.

# 7. Run it detached with auto-restart. Use the same env-file.
docker run -d --name tg-bot \
  --restart unless-stopped \
  -v ~/bot-data:/data \
  -e SESSION_PATH=/data/session.json \
  -e BACKFILL_STATE_PATH=/data/backfill_state.json \
  -e MATCH_LOG_PATH=/data/matches.jsonl \
  --env-file ~/bot.env \
  tg-vacancy-filter

docker logs -f tg-bot    # should show "logged in" and "listening for channel posts"
```

To trigger a backfill later:

```bash
# edit ~/bot.env, add BACKFILL_SINCE=2026-04-17
docker restart tg-bot
docker logs -f tg-bot    # wait for "backfill: complete"
```

**Firewall.** Oracle's default VCN blocks inbound, but the bot only makes
outbound calls to Telegram and Gemini, so no ingress rules are needed.

**Staying always-free.** Oracle used to reclaim idle Always-Free
instances — that policy was removed in 2024, but to be safe, pick a real
workload (this bot) and keep it running. CPU usage of the bot sits near
zero, so you'll never hit throttling.

### Alternative: Google Cloud `e2-micro` (Always Free)

Similar idea: one `e2-micro` VM in `us-west1`, `us-central1`, or
`us-east1` is free forever (1 per account, 30 GB disk). Same Docker-based
setup as Oracle. Requires a credit card but no charges within the free
envelope.

### Railway ($5/mo starter credit)

1. `railway login` && `railway init` (pick "Empty Project").
2. `railway up` or connect the repo through the web UI — Railway
   auto-detects the Dockerfile.
3. Add a **Volume** in the service settings mounted at `/data`. Set
   `SESSION_PATH=/data/session.json`, `BACKFILL_STATE_PATH=/data/backfill_state.json`,
   `MATCH_LOG_PATH=/data/matches.jsonl`.
4. Paste the remaining `.env` contents into the **Variables** tab.
5. Railway containers have no stdin, so authenticate locally first and
   upload `session.json` into the volume — or use `TG_SESSION_BASE64`.

### Render (paid — Background Worker starts at $7/mo)

1. New → **Background Worker** → connect GitHub → select the repo.
2. Environment: **Docker**. Leave build / start commands empty — the
   Dockerfile's `ENTRYPOINT` is enough.
3. Attach a **Disk** (Settings → Disks) mounted at `/data`, 1 GB is plenty.
   Same env vars as Railway above.
4. Paste the `.env` contents into **Environment → Environment Variables**.
5. Same stdin caveat: authenticate locally, upload session, or use
   `TG_SESSION_BASE64`.

### Base64 session (no volume, cheapest tier — works anywhere)

1. Authenticate **once locally**:

   ```bash
   cp .env.example .env
   $EDITOR .env                          # fill everything
   go run .                              # enter code + 2FA when prompted
   # session.json is now on disk
   ```

2. Encode the session:

   ```bash
   base64 -i session.json | tr -d '\n' > session.b64
   ```

3. Set the env var `TG_SESSION_BASE64` to the contents of `session.b64` on
   the host. On boot the bot materialises the file at `SESSION_PATH`
   **only if one doesn't already exist**, so a mounted volume still wins.
   ⚠️ Without a volume, `BACKFILL_STATE_PATH` and `MATCH_LOG_PATH` are
   also ephemeral — clear `BACKFILL_SINCE` after a successful run, or the
   sweep will repeat on every redeploy.

4. Treat `session.b64` like a password — anyone with it can read your
   Telegram account.

> If Telegram invalidates the session (forced re-login from another device,
> password change, etc.), repeat the two steps locally and update the
> env var.

### Health & logs

- The process writes structured logs (`slog` text handler) to **stderr**.
- Railway / Render stream stderr into the web console; set
  `LOG_LEVEL=debug` temporarily to see per-message decisions.

---

## Security notes

- `session.json` grants **full access** to the Telegram account. Never
  commit it, never paste it into screenshots, and rotate by logging out
  from "Active sessions" if you suspect a leak.
- `matches.jsonl` contains private post text from source channels. Keep
  it out of git (it already is) and out of any logs you share.
- Userbots are allowed by Telegram but **abusing the API** (flooding,
  mass DMs, scraping) can get the account banned. This bot is pull-only
  and safe in typical use.
- Model calls include the post text. Don't point the bot at private
  employer-only channels whose content you're not allowed to share with a
  third-party model.

---

## Troubleshooting

| Symptom                                                | Fix                                                                                                                  |
| ------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| `models/<x> is not found for API version v1beta`       | Google retired the model. Switch `GEMINI_MODEL` to one listed in [Model selection](#model-selection).                |
| `gemini: decode "{ \"match\": boolean ... }"`          | Old prompt + a Gemma model echoing the JSON template. Update the binary — the current Gemma path uses text format.   |
| `gemini: no MATCH line in response`                    | Gemma didn't follow the two-line format on a borderline post. Verdict is dropped, post is skipped on this run only.  |
| `AUTH_KEY_UNREGISTERED` on boot                        | Session revoked — delete `session.json` and re-authenticate.                                                         |
| `PHONE_CODE_INVALID`                                   | The code arrives via **Telegram app**, not SMS — check Saved Messages.                                               |
| Nothing happens for a known post                       | Account must be joined to the channel; also check `MAX_MESSAGE_AGE_SECONDS`.                                         |
| `resolve destination ...: USERNAME_NOT_OCCUPIED`       | Channel username is wrong or the account cannot see it. Prefer `DESTINATION=me`.                                     |
| `INVITE_HASH_EXPIRED` / `INVITE_HASH_INVALID`          | Regenerate the invite link in the channel settings and update `.env`.                                                |
| RESOURCE_EXHAUSTED bursts                              | Drop `GEMINI_RPM` by 5 or wait for the daily reset (UTC midnight). The retry handler covers transient 429s for free. |

Enjoy the cleaner inbox.
