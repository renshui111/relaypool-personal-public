package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The settings table backs the global armor switch, so its behaviour decides
// whether an operator's choice survives a restart.

func TestSettingRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SetSetting(ctx, SettingArmorEnabled, "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err := st.GetSetting(ctx, SettingArmorEnabled)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "true" {
		t.Errorf("value = %q, want true", got)
	}

	// Overwriting must replace, not duplicate. A second row would make the
	// reader's result depend on query order.
	if err := st.SetSetting(ctx, SettingArmorEnabled, "false"); err != nil {
		t.Fatalf("SetSetting (overwrite): %v", err)
	}
	got, _ = st.GetSetting(ctx, SettingArmorEnabled)
	if got != "false" {
		t.Errorf("value after overwrite = %q, want false", got)
	}

	var rows int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, SettingArmorEnabled).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("settings has %d rows for one key, want 1; the upsert is not replacing", rows)
	}
}

// An absent setting is a distinct outcome from an empty one, so callers can tell
// "never configured" apart from "configured to empty".
func TestSettingAbsentIsDistinctFromEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.GetSetting(ctx, "never_set"); err == nil {
		t.Error("reading an unset key must return an error, not an empty value")
	}

	if err := st.SetSetting(ctx, "set_empty", ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err := st.GetSetting(ctx, "set_empty")
	if err != nil {
		t.Fatalf("GetSetting after writing empty: %v", err)
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}

// BoolSetting must fall back to the default rather than failing, because a
// malformed row must not stop the gateway from serving.
func TestBoolSettingFallsBackOnGarbage(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Absent: the default applies.
	if got := st.BoolSetting(ctx, "absent", true); got != true {
		t.Error("an absent setting must return the caller's default")
	}

	// Unparseable: the default applies rather than an error.
	if err := st.SetSetting(ctx, "garbage", "maybe"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := st.BoolSetting(ctx, "garbage", true); got != true {
		t.Error("an unparseable value must fall back to the default")
	}

	// Each accepted spelling must work, because the value may be written by a
	// config file rather than by the console.
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"true", true}, {"1", true}, {"on", true}, {"yes", true},
		{"false", false}, {"0", false}, {"off", false}, {"no", false},
	} {
		if err := st.SetSetting(ctx, "spelling", tc.raw); err != nil {
			t.Fatalf("SetSetting(%q): %v", tc.raw, err)
		}
		if got := st.BoolSetting(ctx, "spelling", !tc.want); got != tc.want {
			t.Errorf("BoolSetting(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// The setting must survive a reopen, which is the whole point of persisting it:
// a switch that resets on restart would silently stop armoring traffic.
func TestSettingSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relaypool.db")
	ctx := context.Background()

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSetting(ctx, SettingArmorEnabled, "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	st.Close()

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := reopened.BoolSetting(ctx, SettingArmorEnabled, false); !got {
		t.Error("the armor switch did not survive a reopen; it would silently reset on restart")
	}
}

// Migration 004 must apply on a database created before it existed, which is the
// real upgrade path for a running deployment.
func TestSettingsMigrationAppliesToAnOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relaypool.db")
	ctx := context.Background()

	// Simulate an older database: create the schema, then drop the settings
	// table and its ledger row so the migration looks pending again.
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TABLE IF EXISTS settings`); err != nil {
		t.Fatalf("drop settings: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE filename = '004_settings.sql'`); err != nil {
		t.Fatalf("clear ledger row: %v", err)
	}
	st.Close()

	// Reopening must re-apply 004 and leave the table usable.
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen after simulating an older database: %v", err)
	}
	defer upgraded.Close()

	if err := upgraded.SetSetting(ctx, SettingArmorEnabled, "true"); err != nil {
		t.Fatalf("the settings table is not usable after the migration: %v", err)
	}
	if got := upgraded.BoolSetting(ctx, SettingArmorEnabled, false); !got {
		t.Error("the value did not round-trip after the migration")
	}
}
