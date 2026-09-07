package telegram

import (
	"context"
	"fmt"
	"os"

	"github.com/gotd/td/session"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
)

// SessionSource names where the MTProto session came from. Reported by the
// doctor so it is obvious which credentials the run is actually using.
const (
	SessionFromFile        = "session file"
	SessionFromString      = "TG_STRING_SESSION"
	SessionFromInteractive = "interactive login"
)

// BuildSessionStorage picks the session source.
//
// TG_STRING_SESSION wins when set: it is the credential the operator chose for
// this deployment, whereas a session file on disk is an incidental local cache
// that may well be stale or revoked. It is kept in memory and never written to
// disk, so which credential a run used is never ambiguous.
//
// TG_SESSION_BASE64 is materialised onto SessionPath by config.Load, so it is
// covered by the file branch.
func BuildSessionStorage(ctx context.Context, cfg *config.Config) (session.Storage, string, error) {
	if cfg.StringSession != "" {
		data, err := session.TelethonSession(cfg.StringSession)
		if err != nil {
			return nil, "", fmt.Errorf("decode TG_STRING_SESSION: %w", err)
		}
		mem := &session.StorageMemory{}
		if err := (&session.Loader{Storage: mem}).Save(ctx, data); err != nil {
			return nil, "", fmt.Errorf("load TG_STRING_SESSION: %w", err)
		}
		return mem, SessionFromString, nil
	}

	if fileHasData(cfg.SessionPath) {
		return &session.FileStorage{Path: cfg.SessionPath}, SessionFromFile, nil
	}

	return &session.FileStorage{Path: cfg.SessionPath}, SessionFromInteractive, nil
}

func fileHasData(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
