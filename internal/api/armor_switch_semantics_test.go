// Probes the interaction between the global armor switch and request headers.
//
// The question: is the global switch a MASTER switch (off means off, no header
// can override) or a DEFAULT that a caller may opt into? The code currently
// ORs them, and the commit message called the switch "the ONLY thing that
// decides whether a request is armored", so one of the two is wrong.
package api

import (
	"io"
	"net/http"
	"testing"
)

// armorApplied sends one request and reports which preset the gateway says it
// applied. "none" means nothing was substituted.
//
// It reads the response header rather than the substituted prompt, because the
// header is the contract a caller observes and is present even when nothing was
// applied.
func armorApplied(t *testing.T, srvURL, model, enableHeader string) string {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srvURL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	if enableHeader != "" {
		req.Header.Set("X-RelayPool-Armor", enableHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	applied := resp.Header.Get("X-RelayPool-Armor-Applied")
	if applied == "" {
		// An absent header would make every assertion below silently compare
		// against the empty string, so fail loudly instead.
		t.Fatal("X-RelayPool-Armor-Applied was absent; it must be present on every response")
	}
	return applied
}

// The full truth table of (global switch, request header).
//
// This asserts every cell. An earlier version only logged them, which meant the
// test could never fail and contributed nothing to the suite while looking like
// coverage -- exactly the kind of test this project has been burned by.
func TestArmorSwitchVersusHeaderMatrix(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))

	// A model whose family resolves, so "applied" is meaningful rather than
	// always none.
	const model = "deepseek-anything"

	set := func(on bool) {
		v := "false"
		if on {
			v = "true"
		}
		if status, body := armorSwitchPut(t, srv.URL, `{"enabled":`+v+`}`); status != http.StatusOK {
			t.Fatalf("set switch %v: %d %s", on, status, body)
		}
	}

	for _, tc := range []struct {
		name      string
		global    bool
		header    string
		wantApply bool
	}{
		// Switch off: the header can never turn armor on.
		{"off + absent", false, "", false},
		{"off + 1", false, "1", false},
		{"off + 0", false, "0", false},
		// Switch on: the model selects the preset, and the header may narrow.
		{"on + absent", true, "", true},
		{"on + 1", true, "1", true},
		{"on + 0", true, "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.global)
			applied := armorApplied(t, srv.URL, model, tc.header)

			if tc.wantApply && applied == "none" {
				t.Errorf("switch=%v header=%q: nothing was applied, but it should have been",
					tc.global, tc.header)
			}
			if !tc.wantApply && applied != "none" {
				t.Errorf("switch=%v header=%q: applied %q, but nothing should have been "+
					"(the switch is a master switch, and a header may only narrow)",
					tc.global, tc.header, applied)
			}
		})
	}
}

// The master-switch rule, in its strongest form: even an explicit preset NAME
// cannot turn armor on while the switch is off.
//
// Why this exists: the switch is the operator's decision about what the model is
// told, so it must not be overridable by a caller. Naming a preset is the most
// explicit way a caller can ask for armor, which makes it the strongest test of
// that rule.
//
// The full truth table lives in TestArmorSwitchVersusHeaderMatrix; this covers
// the case that matters most for the operator's authority.
func TestArmorSwitchIsAMasterSwitch(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	withArmorDir(t, server, promptDir(t))
	const model = "deepseek-anything"

	set := func(on bool) {
		v := "false"
		if on {
			v = "true"
		}
		if status, body := armorSwitchPut(t, srv.URL, `{"enabled":`+v+`}`); status != http.StatusOK {
			t.Fatalf("set switch %v: %d %s", on, status, body)
		}
	}

	// Switch OFF: nothing may be applied, whatever the header says.
	set(false)
	for _, header := range []string{"", "1", "0", "true", "yes"} {
		applied := armorApplied(t, srv.URL, model, header)
		if applied != "none" {
			t.Errorf("global switch OFF, header %q -> applied=%q; the switch must be a "+
				"master switch, so a caller cannot force armor on against the operator",
				header, applied)
		}
	}

	// Switch OFF: naming a preset is still not enough.
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-RelayPool-Armor-Preset", "infinite-gen-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if got := resp.Header.Get("X-RelayPool-Armor-Applied"); got != "none" {
		t.Errorf("naming a preset while the switch is off applied %q; the switch is the "+
			"operator's decision and a header must not override it", got)
	}

	// Switch ON: the header may narrow to less armor, which is always safe.
	set(true)
	if applied := armorApplied(t, srv.URL, model, "0"); applied != "none" {
		t.Errorf("global switch ON, header \"0\" -> applied=%q; a caller must be able to "+
			"narrow to less armor for one request", applied)
	}
}
