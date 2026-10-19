package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
)

// withArmorDir rebuilds the armor registry against an absolute preset path.
//
// The default config points armor.dir at the relative path "prompts", which is
// right for a binary run from the repo root but not for a test whose working
// directory is this package. Loading the real directory is what makes these
// tests evidence about the SHIPPED presets rather than a synthetic registry.
func withArmorDir(t *testing.T, srv *Server, dir string) {
	t.Helper()
	cfg := srv.cfg
	cfg.Armor.Dir = dir
	srv.cfg = cfg
	srv.loadArmor()
	if srv.armor == nil || srv.armor.Empty() {
		t.Fatalf("no armor presets loaded from %s", dir)
	}
}

// promptDir is the repository's bundled preset directory.
func promptDir(t *testing.T) string {
	t.Helper()
	if armor.DistributionProfile == "public" {
		// 公开版没有研究正文，用自有夹具验证相同开关与匹配行为。
		dir := t.TempDir()
		for _, family := range []string{"gpt", "claude", "deepseek", "gemini", "glm", "kimi", "qwen", "grok", "llama"} {
			text := fmt.Sprintf("<!-- armor\nfamily: %s\nauto: true\npriority: 100\n-->\n%s 系列自有模拟提示；只验证开关与替换流程。", family, family)
			if err := os.WriteFile(filepath.Join(dir, "fixture-"+family+".md"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	dir, err := filepath.Abs(filepath.Join("..", "..", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}
	return dir
}

// enableArmor flips the global armor switch on for a test.
//
// Tests that expect a substitution MUST call this. The switch is a master
// switch, so a request header alone no longer enables armor; a test that forgets
// this would still pass while accidentally asserting "nothing happened".
func enableArmor(t *testing.T, srvURL string) {
	t.Helper()
	if status, body := armorSwitchPut(t, srvURL, `{"enabled":true}`); status != http.StatusOK {
		t.Fatalf("enabling armor: %d %s", status, body)
	}
}

// Armor is selected by the REQUESTED MODEL, with a single on/off switch.
//
// These tests pin the behaviour from the outside, through the real HTTP
// surface, because the selection is what a caller observes: the same request
// must produce the same prompt regardless of which account serves it.

// A request that does not opt in gets no substitution, and says so.
func TestArmorOffByDefault(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	withArmorDir(t, server, promptDir(t))

	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// The header is always present, so a caller can tell "not applied" from
	// "the header was stripped by a proxy".
	if got := resp.Header.Get("X-RelayPool-Armor-Applied"); got != "none" {
		t.Errorf("X-RelayPool-Armor-Applied=%q, want none", got)
	}
}

// With the switch on, a model that has a preset receives that preset.
//
// The request still carries the enable header, which is now a narrowing control
// rather than an enabling one: the switch is what turns armor on.
func TestArmorAppliedByModel(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))
	enableArmor(t, srv.URL)

	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-RelayPool-Armor", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	applied := resp.Header.Get("X-RelayPool-Armor-Applied")
	if applied == "" || applied == "none" {
		t.Fatalf("a deepseek request with armor on applied nothing (header=%q)", applied)
	}
	preset := resp.Header.Get("X-RelayPool-Armor-Preset")
	if preset != applied {
		t.Errorf("preset header=%q differs from applied=%q", preset, applied)
	}
}

// When the armor is OFF, the same request must NOT be substituted. This is the
// other half of the on/off contract.
func TestArmorOffLeavesThePromptAlone(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	withArmorDir(t, server, promptDir(t))

	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"deepseek-v4-pro","messages":[{"role":"system","content":"original system"},{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-RelayPool-Armor", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("X-RelayPool-Armor-Applied"); got != "none" {
		t.Errorf("an explicit off must not substitute, header=%q", got)
	}
	// The adapter must have received the caller's own system message.
	if body := adapter.lastBody; body != nil {
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			first, _ := msgs[0].(map[string]any)
			if content, _ := first["content"].(string); !strings.Contains(content, "original system") {
				t.Errorf("the caller's system message was replaced: %q", content)
			}
		}
	}
}

// The resolve endpoint reports the mapping WITHOUT enabling anything, which is
// how an operator checks what a model would receive.
func TestArmorResolveReportsTheMapping(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))

	req := authRequest(t, http.MethodGet, srv.URL+"/admin/armor/resolve?model=glm-5.2", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, `"family":"glm"`) {
		t.Errorf("glm-5.2 should resolve to the glm family: %s", text)
	}
}

// Two different models must resolve to different presets. This is the core
// property of model-based selection, and the thing the old account-based design
// could not express.
func TestArmorResolveDiffersPerModel(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))

	pick := func(model string) string {
		t.Helper()
		req := authRequest(t, http.MethodGet, srv.URL+"/admin/armor/resolve?model="+model, "")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("resolve %s: %v", model, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	glm := pick("glm-5.2")
	ds := pick("deepseek-v4-pro")
	if glm == ds {
		t.Errorf("glm and deepseek resolved identically, so selection is not model-based:\n%s\n%s", glm, ds)
	}
}

// A model with no matching preset must apply NOTHING rather than borrowing an
// unrelated prompt, even when the armor is switched on.
//
// The switch MUST be on for this test to mean anything: with it off, "nothing
// was applied" is trivially true for the wrong reason and the test would pass
// even if unknown models were being handed a random preset.
func TestArmorUnknownModelAppliesNothing(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))
	enableArmor(t, srv.URL)

	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"totally-unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-RelayPool-Armor", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("X-RelayPool-Armor-Applied"); got != "none" {
		t.Errorf("an unrecognised model must not receive an unrelated preset, got %q", got)
	}
}

// The account JSON must no longer carry an armor preset, since there is nothing
// per-account to configure.
func TestAccountViewHasNoArmorPreset(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	req := authRequest(t, http.MethodGet, srv.URL+"/admin/accounts", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(body), "armor_preset") {
		t.Errorf("the account view still exposes armor_preset: %s", body)
	}
}

// The armor config must not offer an account-level opt-in any more.
func TestArmorConfigHasNoAccountOptIn(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))

	req := authRequest(t, http.MethodGet, srv.URL+"/admin/armor", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "allow_account_opt_in") {
		t.Errorf("/admin/armor still advertises allow_account_opt_in: %s", body)
	}
}
