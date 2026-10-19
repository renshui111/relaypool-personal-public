package api

import (
	"encoding/json"
	"github.com/relaypool/relaypool/internal/store"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The global armor switch: one on/off decision, applied per requested model.
//
// These tests exist because the switch is the ONLY thing standing between a
// normal request and a substituted system prompt. Armor changes what the model
// is told, so "it turned itself on" would be a correctness failure, not a
// cosmetic one.

// armorProbe sends one chat request for `model`, optionally with an armor
// header, and reports what armor did.
//
// It reads the response header rather than the substituted prompt, because the
// header is the contract a caller actually observes: X-RelayPool-Armor-Applied
// is present on every request, including ones where nothing was applied.
func armorProbe(t *testing.T, srvURL, model, armorHeader string) (sentModel, preset string, applied bool) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srvURL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	if armorHeader != "" {
		req.Header.Set("X-RelayPool-Armor", armorHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	preset = resp.Header.Get("X-RelayPool-Armor-Applied")
	// The header is always present; "none" is how "nothing was applied" is
	// reported, which is what lets a caller tell it apart from a stripped header.
	if preset == "" {
		t.Fatal("X-RelayPool-Armor-Applied was absent; it must be present on every response")
	}
	return model, preset, preset != "none"
}

// armorSwitchGet reads the switch state.
func armorSwitchGet(t *testing.T, srvURL string) (enabled bool, presets int, families []string) {
	t.Helper()
	req := authRequest(t, http.MethodGet, srvURL+"/v1/key/permissions", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/key/permissions: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/key/permissions = %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Enabled  bool     `json:"enabled"`
		Presets  int      `json:"presets"`
		Families []string `json:"families"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode switch state: %v", err)
	}
	var view struct {
		Armor keyArmorStateView `json:"armor"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	return view.Armor.Enabled, view.Armor.Presets, view.Armor.Families
}

// armorSwitchPut sets the switch and returns the HTTP status.
func armorSwitchPut(t *testing.T, srvURL, body string) (int, string) {
	t.Helper()
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	patch, id := body, "missing"
	if json.Unmarshal([]byte(body), &input) == nil && input.Enabled != nil {
		current := authRequest(t, http.MethodGet, srvURL+"/v1/key/permissions", "")
		response, err := http.DefaultClient.Do(current)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			ID          string               `json:"id"`
			Permissions store.KeyPermissions `json:"permissions"`
		}
		if err := json.NewDecoder(response.Body).Decode(&record); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		record.Permissions.ArmorAllowed = *input.Enabled
		record.Permissions.ArmorEnabled = *input.Enabled
		id = record.ID
		raw, _ := json.Marshal(map[string]any{"permissions": record.Permissions})
		patch = string(raw)
	}
	req := authRequest(t, http.MethodPatch, srvURL+"/admin/keys/"+id, patch)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/key/permissions: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// The switch must start off. Armor substitutes the system prompt, so a default
// of on would change every request without the operator asking.
func TestArmorSwitchDefaultsOff(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	enabled, _, _ := armorSwitchGet(t, srv.URL)
	if enabled {
		t.Error("armor must default to OFF: it substitutes the system prompt, which " +
			"must not happen unless the operator turns it on")
	}
}

// Flipping the switch must be observable, and reported back.
func TestArmorSwitchToggles(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	status, body := armorSwitchPut(t, srv.URL, `{"enabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("enabling armor = %d: %s", status, body)
	}
	if enabled, _, _ := armorSwitchGet(t, srv.URL); !enabled {
		t.Error("the switch reported off immediately after being turned on")
	}

	status, body = armorSwitchPut(t, srv.URL, `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("disabling armor = %d: %s", status, body)
	}
	if enabled, _, _ := armorSwitchGet(t, srv.URL); enabled {
		t.Error("the switch reported on immediately after being turned off")
	}
}

// A malformed body must be rejected, NOT treated as "off". Silently disabling
// armor for an operator who meant to enable it is the worse failure.
func TestArmorSwitchRejectsMalformedBodies(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	// Turn it on first, so a body that defaulted to false would be detectable.
	if status, body := armorSwitchPut(t, srv.URL, `{"enabled":true}`); status != http.StatusOK {
		t.Fatalf("setup: enabling armor = %d: %s", status, body)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"wrong type", `{"enabled":"yes"}`},
		{"not json", `enabled=true`},
		{"array", `[true]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := armorSwitchPut(t, srv.URL, tc.body)
			if status != http.StatusBadRequest {
				t.Errorf("body %s = %d, want 400; a malformed body must not be "+
					"interpreted as 'turn it off'", tc.body, status)
			}
			if enabled, _, _ := armorSwitchGet(t, srv.URL); !enabled {
				t.Errorf("body %s changed the switch: a rejected request must have no effect", tc.body)
			}
		})
	}
}

// The state must report what the switch would affect, so an operator can see
// the consequences before enabling it.
func TestArmorSwitchReportsAffectedFamilies(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))

	srvURL := srv.URL
	_, presets, families := armorSwitchGet(t, srvURL)
	if presets == 0 {
		t.Fatal("the fixture loaded no presets, so this test cannot check the report")
	}
	if len(families) == 0 {
		t.Error("no auto-selectable families were reported, but armor-on would use them")
	}
	// The list is a JSON array, never null: the console iterates it directly.
	req := authRequest(t, http.MethodGet, srvURL+"/v1/key/permissions", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), `"families":null`) {
		t.Error("families must be an empty array, not null, so the console can iterate it")
	}
}

// With the switch ON, a request must be armored according to its MODEL, without
// the caller sending any header. This is the whole point of the switch.
func TestArmorSwitchOnArmorsByRequestedModel(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	withArmorDir(t, server, promptDir(t))

	// Before: no header, no armor.
	if _, _, applied := armorProbe(t, srv.URL, "glm-5.3", ""); applied {
		t.Fatal("armor was applied before the switch was turned on")
	}

	if status, body := armorSwitchPut(t, srv.URL, `{"enabled":true}`); status != http.StatusOK {
		t.Fatalf("enable armor: %d %s", status, body)
	}

	// After: still no header, but the model now selects a preset.
	model, preset, applied := armorProbe(t, srv.URL, "glm-5.3", "")
	if !applied {
		t.Fatal("armor was not applied with the switch on and no header sent")
	}
	if model != "glm-5.3" {
		t.Errorf("probe sent model %q", model)
	}
	if !strings.Contains(preset, "glm") {
		t.Errorf("a glm request was armored with preset %q, which is not the glm preset; "+
			"selection must follow the requested model", preset)
	}
}

// A model whose family is unknown must get NOTHING even with the switch on.
// Guessing would send a prompt written for a different model.
func TestArmorSwitchOnDoesNotGuessUnknownFamilies(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	withArmorDir(t, server, promptDir(t))

	if status, body := armorSwitchPut(t, srv.URL, `{"enabled":true}`); status != http.StatusOK {
		t.Fatalf("enable armor: %d %s", status, body)
	}

	if _, preset, applied := armorProbe(t, srv.URL, "some-unknown-model-9", ""); applied {
		t.Errorf("an unknown model was armored with %q; the selection must not guess", preset)
	}
}

// Turning the switch back off must stop substitution immediately.
func TestArmorSwitchOffStopsSubstitution(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	withArmorDir(t, server, promptDir(t))

	if status, _ := armorSwitchPut(t, srv.URL, `{"enabled":true}`); status != http.StatusOK {
		t.Fatal("could not enable armor for the setup")
	}
	if _, _, applied := armorProbe(t, srv.URL, "glm-5.3", ""); !applied {
		t.Fatal("armor did not apply with the switch on")
	}

	if status, _ := armorSwitchPut(t, srv.URL, `{"enabled":false}`); status != http.StatusOK {
		t.Fatal("could not disable armor")
	}
	if _, _, applied := armorProbe(t, srv.URL, "glm-5.3", ""); applied {
		t.Error("armor still applied after the switch was turned off")
	}
}
