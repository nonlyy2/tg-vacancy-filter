// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Destination kinds the bot knows how to resolve.
const (
	DestSelf    = "me"
	DestSelfAlt = "self"
	MinPhoneLen = 6

	// Gemini-family models enforce the response schema server-side, which is
	// what keeps verdict parsing deterministic. Gemma is kept as the fallback
	// because its free-tier RPD (~14400) is an order of magnitude higher, so
	// it can absorb a bootstrap sweep after the primary model's daily cap.
	defaultModel = "gemini-3.5-flash-lite"
	// Fallback chain, tried in order. Quotas are per model, so each entry is
	// another daily allowance. Order is by speed: the lite models answer in
	// ~5s, Gemma in ~45s, so Gemma is the last resort rather than the first.
	defaultModelFallback = "gemini-3.1-flash-lite,gemini-2.5-flash-lite,gemma-4-26b-a4b-it"
	defaultGeminiRPM     = 12

	defaultProfilePath    = "profile/candidate.md"
	defaultMatchThreshold = 60
	defaultPollStatePath  = "state.json"
	defaultPollMaxRuntime = 20 * time.Minute

	dateFmt = "2006-01-02"
)

// Config holds all runtime settings.
type Config struct {
	AppID       int
	AppHash     string
	Phone       string
	SessionPath string

	// StringSession is a Telethon-format StringSession. When set (and no
	// session file exists yet) the bot authenticates without a terminal,
	// which is what makes unattended hosts possible.
	StringSession string

	// SourceChannels contains unmarked (positive) channel IDs that the bot watches.
	SourceChannels map[int64]struct{}

	// Destination is either "me"/"self", a username, or an invite link.
	Destination string

	GeminiAPIKey string
	GeminiModel  string

	// GeminiModelFallback is a comma-separated chain of models tried in order
	// as each preceding one's quota runs out. Empty disables failover.
	GeminiModelFallback string

	// GeminiRPM caps the request rate. Zero disables rate limiting.
	GeminiRPM int

	// ProfilePath points at the markdown file describing the candidate. Its
	// contents are injected into the prompt, so retuning the filter is an
	// edit to that file rather than a code change.
	ProfilePath string

	// MatchThreshold is the minimum score (0-100) a post must reach to be
	// forwarded.
	MatchThreshold int

	// MaxMessageAge is the cutoff applied to incoming live posts; older
	// messages are skipped so a burst of missed updates after a reconnect is
	// not re-processed. Zero disables the check. Live mode only.
	MaxMessageAge time.Duration

	// PollStatePath stores per-channel cursors and seen-post fingerprints for
	// the --once mode.
	PollStatePath string

	// PollBootstrapSince is the cutoff used the first time a channel is polled
	// and has no cursor yet. Zero means "start from the newest post".
	PollBootstrapSince time.Time

	// PollMaxRuntime bounds a single --once run. On expiry the poller saves
	// its cursor and exits cleanly, so a long backlog is consumed across
	// several scheduled runs instead of failing one oversized job.
	PollMaxRuntime time.Duration

	// MatchLogPath is the JSONL file where every match is appended. Empty
	// disables the log.
	MatchLogPath string

	LogLevel slog.Level
}

// Load reads configuration from a local .env (if present) plus process env.
// Missing required fields return a descriptive error.
func Load() (*Config, error) {
	// .env is optional — production deployments inject real env vars.
	_ = godotenv.Load()

	cfg := &Config{
		SessionPath:         getenvDefault("SESSION_PATH", "session.json"),
		StringSession:       strings.TrimSpace(os.Getenv("TG_STRING_SESSION")),
		GeminiModel:         getenvDefault("GEMINI_MODEL", defaultModel),
		GeminiModelFallback: getenvDefault("GEMINI_MODEL_FALLBACK", defaultModelFallback),
		GeminiRPM:           parsePositiveInt(os.Getenv("GEMINI_RPM"), defaultGeminiRPM),
		Destination:         strings.TrimPrefix(strings.TrimSpace(os.Getenv("DESTINATION")), "@"),
		LogLevel:            parseLogLevel(os.Getenv("LOG_LEVEL")),
		ProfilePath:         getenvDefault("PROFILE_PATH", defaultProfilePath),
		MatchThreshold:      parsePositiveInt(os.Getenv("MATCH_THRESHOLD"), defaultMatchThreshold),
		MaxMessageAge:       parseDurationSeconds(os.Getenv("MAX_MESSAGE_AGE_SECONDS"), 15*time.Minute),
		PollStatePath:       getenvDefault("POLL_STATE_PATH", defaultPollStatePath),
		PollMaxRuntime:      parseDuration(os.Getenv("POLL_MAX_RUNTIME"), defaultPollMaxRuntime),
		MatchLogPath:        getenvDefault("MATCH_LOG_PATH", "matches.jsonl"),
	}

	// MATCH_LOG_PATH is deliberately allowed to be empty (disabled), which
	// getenvDefault cannot express — re-read it directly.
	if _, set := os.LookupEnv("MATCH_LOG_PATH"); set {
		cfg.MatchLogPath = strings.TrimSpace(os.Getenv("MATCH_LOG_PATH"))
	}

	if cfg.MatchThreshold > 100 {
		return nil, fmt.Errorf("MATCH_THRESHOLD must be 0-100, got %d", cfg.MatchThreshold)
	}

	if raw := strings.TrimSpace(os.Getenv("POLL_BOOTSTRAP_SINCE")); raw != "" {
		t, err := time.ParseInLocation(dateFmt, raw, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("POLL_BOOTSTRAP_SINCE must be YYYY-MM-DD: %w", err)
		}
		cfg.PollBootstrapSince = t
	}

	appIDStr := os.Getenv("TG_APP_ID")
	if appIDStr == "" {
		return nil, fmt.Errorf("TG_APP_ID is required")
	}
	appID, err := strconv.Atoi(appIDStr)
	if err != nil {
		return nil, fmt.Errorf("TG_APP_ID must be an integer: %w", err)
	}
	cfg.AppID = appID

	cfg.AppHash = strings.TrimSpace(os.Getenv("TG_APP_HASH"))
	if cfg.AppHash == "" {
		return nil, fmt.Errorf("TG_APP_HASH is required")
	}

	cfg.Phone = strings.TrimSpace(os.Getenv("TG_PHONE"))
	if len(cfg.Phone) < MinPhoneLen {
		return nil, fmt.Errorf("TG_PHONE is required (international format, e.g. +77001234567)")
	}

	sources := strings.TrimSpace(os.Getenv("SOURCE_CHANNEL_IDS"))
	if sources == "" {
		return nil, fmt.Errorf("SOURCE_CHANNEL_IDS is required (comma-separated list)")
	}
	cfg.SourceChannels = make(map[int64]struct{})
	for _, part := range strings.Split(sources, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid channel ID %q: %w", part, err)
		}
		cfg.SourceChannels[normalizeChannelID(id)] = struct{}{}
	}
	if len(cfg.SourceChannels) == 0 {
		return nil, fmt.Errorf("SOURCE_CHANNEL_IDS is empty after parsing")
	}

	if cfg.Destination == "" {
		return nil, fmt.Errorf("DESTINATION is required (\"me\" or a username)")
	}

	cfg.GeminiAPIKey = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if cfg.GeminiAPIKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is required")
	}

	// If the host has no persistent disk, callers can ship the session as a
	// base64 blob. Materialise it onto disk before the client boots.
	if b64 := strings.TrimSpace(os.Getenv("TG_SESSION_BASE64")); b64 != "" {
		if err := materializeSession(cfg.SessionPath, b64); err != nil {
			return nil, fmt.Errorf("restore session from TG_SESSION_BASE64: %w", err)
		}
	}

	return cfg, nil
}

// IsSelfDestination returns true when the destination is the current account.
func (c *Config) IsSelfDestination() bool {
	d := strings.ToLower(c.Destination)
	return d == DestSelf || d == DestSelfAlt
}

// ParseChannelID recognises a destination given as a numeric channel id, in
// either the marked (-100xxxxxxxxxx) or raw form, and returns the unmarked id
// used by MTProto.
func ParseChannelID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return normalizeChannelID(id), true
}

// normalizeChannelID converts the marked form -100xxxx to the unmarked form used
// by MTProto updates. Positive IDs are returned unchanged.
func normalizeChannelID(id int64) int64 {
	s := strconv.FormatInt(id, 10)
	if strings.HasPrefix(s, "-100") {
		if trimmed, err := strconv.ParseInt(strings.TrimPrefix(s, "-100"), 10, 64); err == nil {
			return trimmed
		}
	}
	if id < 0 {
		return -id
	}
	return id
}

func materializeSession(path, b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	// Don't clobber an existing session on disk — file wins if both are present.
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, raw, 0o600)
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseDurationSeconds(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

// parseDuration reads a Go duration string ("20m", "90s"). Invalid or negative
// values fall back rather than failing the boot — a bad budget should not take
// the scheduled run down with it.
func parseDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func parsePositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ParseLogLevelEnv resolves LOG_LEVEL directly from the process environment.
// Used during bootstrap before Load() runs, so logs from config failures
// already honour the requested verbosity.
func ParseLogLevelEnv() slog.Level {
	return parseLogLevel(os.Getenv("LOG_LEVEL"))
}
