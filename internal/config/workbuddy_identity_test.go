package config

import "testing"

func TestWorkBuddyIdentityConfigAndEnv(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"upstream":{"workbuddy_identity":"desktop-5.9.3","user_agent":"custom","proxy_url":"http://127.0.0.1:7890"}}`)
	c, err := Load(path)
	if err != nil || c.Upstream.WorkBuddyIdentity != "desktop-5.9.3" || c.Upstream.UserAgent != "custom" || c.Upstream.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("file config not applied: %v", err)
	}
	t.Setenv("RELAYPOOL_WORKBUDDY_IDENTITY", "cli")
	c, err = Load(path)
	if err != nil || c.Upstream.WorkBuddyIdentity != "cli" {
		t.Fatalf("env override not applied: %v", err)
	}
	t.Setenv("RELAYPOOL_WORKBUDDY_IDENTITY", "typo")
	if _, err = Load(path); err == nil {
		t.Fatal("invalid identity accepted")
	}
}
