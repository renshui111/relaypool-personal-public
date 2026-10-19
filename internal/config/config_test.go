package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// clearEnv removes the variables Load consults so a test's expectations are not
// affected by the ambient environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"RELAYPOOL_LISTEN", "RELAYPOOL_DATA_DIR", "RELAYPOOL_RUNTIME_DIR",
		"RELAYPOOL_API_KEY", "RELAYPOOL_ROUTING", "RELAYPOOL_PROXY_URL",
		"RELAYPOOL_ARMOR_DIR", "RELAYPOOL_ARMOR_DEFAULT",
		"RELAYPOOL_ACCESS_MODE", "RELAYPOOL_LAN_BIND", "RELAYPOOL_PUBLIC_BIND",
		"RELAYPOOL_ALLOW_NON_LOOPBACK", "HTTPS_PROXY", "HTTP_PROXY",
	} {
		t.Setenv(name, "")
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.Access.Mode != AccessLoopback {
		t.Errorf("access.mode=%q, want loopback by default", cfg.Access.Mode)
	}
	if !IsLoopbackListen(cfg.Listen) {
		t.Errorf("listen=%q, want loopback by default", cfg.Listen)
	}
	if cfg.Prompts.Mode != "passthrough" {
		t.Errorf("prompts.mode=%q, want passthrough (no substitution by default)", cfg.Prompts.Mode)
	}
}

// ---- access modes ----

// Public exposure without a key is refused outright, not merely warned about:
// an unauthenticated public gateway lets anyone spend the operator's quota.
func TestPersonalRejectsNetworkModes(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAYPOOL_ACCESS_MODE", "lan")
	if _, err := Load(""); err == nil {
		t.Fatal("personal edition accepted LAN mode")
	}
	path := writeConfig(t, `{"access":{"mode":"public"},"api_key":"test"}`)
	t.Setenv("RELAYPOOL_ACCESS_MODE", "")
	if _, err := Load(path); err == nil {
		t.Fatal("personal edition accepted public mode")
	}
}

func TestLANRequiresAPIKey(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"access":{"mode":"lan"}}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("lan mode without an api_key must be refused")
	}
}

func TestAccessModeRejectsUnknown(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"access":{"mode":"galaxy"}}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("an unknown access mode must be rejected")
	}
}

// A typo in a trusted proxy entry must fail loudly: silently trusting the
// wrong range, or none at all, is a security-relevant surprise.

// Loopback mode must keep the operator's own address, not silently widen it.
func TestLoopbackModeKeepsConservativeBind(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	addr, err := cfg.ListenAddress()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("listen=%q, want loopback", addr)
	}
}

func TestListenAddressRejectsBadPort(t *testing.T) {
	clearEnv(t)
	cfg := Default()
	cfg.Listen = "no-port-here"
	if _, err := cfg.ListenAddress(); err == nil {
		t.Fatalf("a listen value without a port must fail")
	}
}

// ---- armor config ----

func TestArmorDefaultsToPerRequestAllowed(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Armor.AllowPerRequest {
		t.Errorf("per-request opt-in should be allowed by default")
	}
	// The feature is still off in practice: no presets means nothing to apply.
	if cfg.Armor.Dir != "prompts" {
		t.Errorf("armor.dir=%q, want the bundled prompts directory", cfg.Armor.Dir)
	}
}

// An explicit empty dir must turn the feature off even though the default is a
// real directory, so an operator can disable it from a config file.
func TestArmorDirCanBeClearedExplicitly(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"armor":{"dir":""}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Armor.Dir != "" {
		t.Errorf("armor.dir=%q, want it cleared", cfg.Armor.Dir)
	}
}

func TestArmorDefaultRequiresDir(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"armor":{"dir":"","default":"infinite-gen-4"}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("a default preset with no directory must be rejected")
	}
}

func TestArmorFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAYPOOL_ARMOR_DIR", "/custom/prompts")
	t.Setenv("RELAYPOOL_ARMOR_DEFAULT", "custom")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Armor.Dir != "/custom/prompts" {
		t.Errorf("armor.dir=%q", cfg.Armor.Dir)
	}
	if cfg.Armor.Default != "custom" {
		t.Errorf("armor.default=%q", cfg.Armor.Default)
	}
}

// ---- round trip ----

func TestConfigFileRoundTrip(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{
		"listen": "127.0.0.1:9999",
		"routing": "fill-first",
		"armor": {"dir": "presets", "default": "alpha", "allow_per_request": false},
		"access": {"mode": "loopback"},
		"pool": {"breaker_cooldown": "45m"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen=%q", cfg.Listen)
	}
	if cfg.Routing != "fill-first" {
		t.Errorf("routing=%q", cfg.Routing)
	}
	if cfg.Armor.Dir != "presets" || cfg.Armor.Default != "alpha" {
		t.Errorf("armor=%+v", cfg.Armor)
	}
	if cfg.Armor.AllowPerRequest {
		t.Errorf("allow_per_request should have been set to false")
	}
	if cfg.Pool.BreakerCooldown.String() != "45m0s" {
		t.Errorf("breaker cooldown=%s, want 45m", cfg.Pool.BreakerCooldown)
	}
}

// The shipped example must at least parse, so it cannot rot unnoticed.
func TestExampleConfigParses(t *testing.T) {
	clearEnv(t)
	path := filepath.Join("..", "..", "deploy", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no example config at %s", path)
	}
	// A JSON comment would break this, so the example must be pure JSON.
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("the example config is not valid JSON: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
}
