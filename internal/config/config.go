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
	DestSelf         = "me"
	DestSelfAlt      = "self"
	MinPhoneLen      = 6
	// Gemma 4 inherits the Gemma free-tier caps (30 RPM / 14400 RPD), which
	// is what makes it usable for multi-thousand-message backfills. Gemini
	// 2.5 Flash Lite gives stricter JSON but only ~1000 RPD on free tier —
	// override GEMINI_MODEL when you need that.
	defaultModel     = "gemma-4-26b-a4b-it"
	defaultGeminiRPM = 25 // safe margin below the 30 RPM free-tier cap for Gemma
	backfillDateFmt  = "2006-01-02"
)

// Config holds all runtime settings.
type Config struct {
	AppID       int
	AppHash     string
	Phone       string
	SessionPath string

	// SourceChannels contains unmarked (positive) channel IDs that the bot watches.
	SourceChannels map[int64]struct{}

	// Destination is either "me"/"self" or a username (with or without @).
	Destination string

	GeminiAPIKey string
	GeminiModel  string

	// GeminiRPM caps the request rate. Gemini free tier is currently 15 RPM.
	// Zero disables rate limiting.
	GeminiRPM int

	// MaxMessageAge is the cutoff applied to incoming posts; older messages are
	// skipped so a backlog is not re-processed after downtime. Zero disables the check.
	MaxMessageAge time.Duration

	// BackfillSince, when non-zero, triggers a one-time history sweep of every
	// source channel starting from this date. Matches are sent to the
	// destination just like live updates.
	BackfillSince time.Time

	// BackfillStatePath stores per-channel progress so restarts don't
	// re-analyse messages already sent through Gemini.
	BackfillStatePath string

	// MatchLogPath is the JSONL file where every match=true verdict is
	// appended. Empty disables the log.
	MatchLogPath string

	LogLevel slog.Level
}

// Load reads configuration from a local .env (if present) plus process env.
// Missing required fields return a descriptive error.
func Load() (*Config, error) {
	// .env is optional — production deployments inject real env vars.
	_ = godotenv.Load()

	cfg := &Config{
		SessionPath:       getenvDefault("SESSION_PATH", "session.json"),
		GeminiModel:       getenvDefault("GEMINI_MODEL", defaultModel),
		GeminiRPM:         parsePositiveInt(os.Getenv("GEMINI_RPM"), defaultGeminiRPM),
		Destination:       strings.TrimPrefix(strings.TrimSpace(os.Getenv("DESTINATION")), "@"),
		LogLevel:          parseLogLevel(os.Getenv("LOG_LEVEL")),
		MaxMessageAge:     parseDurationSeconds(os.Getenv("MAX_MESSAGE_AGE_SECONDS"), 15*time.Minute),
		BackfillStatePath: getenvDefault("BACKFILL_STATE_PATH", "backfill_state.json"),
		MatchLogPath:      getenvDefault("MATCH_LOG_PATH", "matches.jsonl"),
	}

	if sinceRaw := strings.TrimSpace(os.Getenv("BACKFILL_SINCE")); sinceRaw != "" {
		t, err := time.ParseInLocation(backfillDateFmt, sinceRaw, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("BACKFILL_SINCE must be YYYY-MM-DD: %w", err)
		}
		cfg.BackfillSince = t
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
