package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Setting keys. These are stored in the settings table so a runtime toggle
// survives a restart without a schema change per option.
const (
	// SettingArmorEnabled is the global armor switch ("true"/"false").
	//
	// WHY GLOBAL AND NOT PER ACCOUNT: armor is selected from the requested MODEL,
	// and one account serves many models. An account-level switch would therefore
	// enable the preset for models it was never chosen for. The operator wants a
	// single "破甲 on/off" decision, so that is what is stored.
	SettingArmorEnabled = "armor_enabled"
)

// ErrSettingNotFound is returned by GetSetting for an absent key. Callers treat
// a missing setting as "not configured" rather than as an error.
var ErrSettingNotFound = errors.New("store: setting not found")

// GetSetting returns a setting value, or ErrSettingNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSettingNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting writes a setting, replacing any previous value.
//
// The upsert is a single statement so two concurrent writers cannot interleave
// into a duplicate key; SQLite applies one of them and the last wins, which is
// the correct semantics for a toggle.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: set setting %s: %w", key, err)
	}
	return nil
}

// BoolSetting reads a setting as a boolean, returning def when it is absent or
// unparseable. A malformed row must not break startup: falling back to the
// default keeps the gateway serving.
func (s *Store) BoolSetting(ctx context.Context, key string, def bool) bool {
	value, err := s.GetSetting(ctx, key)
	if err != nil {
		return def
	}
	switch value {
	case "true", "1", "on", "yes":
		return true
	case "false", "0", "off", "no":
		return false
	default:
		return def
	}
}
