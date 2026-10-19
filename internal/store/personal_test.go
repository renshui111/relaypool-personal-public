package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPersonalSchemaAndSecretIsolation(t *testing.T) {
	st, err := OpenWithCredentialKey(t.Context(), filepath.Join(t.TempDir(), "test.db"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err := st.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND (name LIKE 'portal_%' OR name='model_prices')").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("retired tables remain: %d", count)
	}
	sealed, err := st.encryptSecret("bootstrap_api_key", "test-value")
	if err != nil {
		t.Fatal(err)
	}
	value, err := st.decryptSecret("bootstrap_api_key", sealed)
	if err != nil || value != "test-value" {
		t.Fatalf("secret roundtrip failed: %v", err)
	}
	if _, err := st.decryptSecret("other", sealed); err == nil {
		t.Fatal("secret accepted with wrong associated name")
	}
}
