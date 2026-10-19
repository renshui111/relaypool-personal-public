package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tool arguments must survive every shape an upstream can send.
//
// THE CONTRACT, which this file pins precisely:
//
//   - `toolInputObject` (Anthropic's `input`) must ALWAYS be a JSON object,
//     because that protocol types the field as an object.
//   - `argumentsString` (Responses' `arguments`) must always be a STRING and
//     must never lose information, because that protocol types the field as a
//     string. It is NOT contracted to make the text parse as JSON: a bare word
//     like "not json" is a legal value for a string field, and re-encoding it
//     would change what the upstream sent.
//
// An earlier version of this test asserted that both outputs parse as JSON. That
// was wrong for `argumentsString`, and the mistake is worth recording: the
// assertion described a guarantee the protocol does not make, so it would have
// forced a change that corrupted the value.

func TestArgumentsStringNeverLosesContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// A JSON object is passed through as its serialised text, which is what
		// this protocol expects in a string field.
		{name: "object", in: `{"city":"SF"}`, want: `{"city":"SF"}`},
		// A JSON string is UNWRAPPED, so the client sees the argument text and
		// not a doubly-encoded one.
		{name: "json string holding an object", in: `"{\"city\":\"SF\"}"`, want: `{"city":"SF"}`},
		// A bare word is forwarded as-is: the field is a string.
		{name: "plain string", in: `not json`, want: `not json`},
		// A truncated fragment is forwarded as-is; inventing braces would
		// fabricate content the upstream never sent.
		{name: "truncated object", in: `{"a":`, want: `{"a":`},
		{name: "empty becomes an empty object", in: ``, want: `{}`},
		{name: "number", in: `42`, want: `42`},
		{name: "array", in: `[1,2]`, want: `[1,2]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argumentsString(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("argumentsString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A JSON string that already holds an object must not be emitted twice-encoded,
// which is the specific corruption that would break a client parsing the field.
func TestArgumentsStringDoesNotDoubleEncode(t *testing.T) {
	got := argumentsString(json.RawMessage(`"{\"a\":1}"`))
	if strings.Contains(got, `\"`) {
		t.Errorf("argumentsString double-encoded the value: %q", got)
	}
	if got != `{"a":1}` {
		t.Errorf("argumentsString = %q, want {\"a\":1}", got)
	}
}

// `toolInputObject` backs Anthropic's `input`, which IS typed as an object, so
// every shape must come out as a JSON object.
func TestToolInputObjectIsAlwaysAnObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantContains is checked against the decoded object.
		wantKey   string
		wantValue string
	}{
		{name: "object", in: `{"city":"SF"}`, wantKey: "city", wantValue: "SF"},
		{name: "json string holding an object", in: `"{\"city\":\"SF\"}"`, wantKey: "city", wantValue: "SF"},
		{name: "plain string", in: `not json`, wantKey: "_raw", wantValue: "not json"},
		{name: "number", in: `42`, wantKey: "_raw", wantValue: "42"},
		{name: "truncated object", in: `{"a":`, wantKey: "_raw", wantValue: `{"a":`},
		{name: "array is wrapped, not passed as a bare array", in: `[1,2]`, wantKey: "_items"},
		{name: "empty becomes an empty object", in: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolInputObject(json.RawMessage(tc.in))

			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatalf("toolInputObject(%q) = %q, which is not a JSON object: %v", tc.in, got, err)
			}
			if tc.wantKey == "" {
				if len(obj) != 0 {
					t.Errorf("toolInputObject(%q) = %q, want an empty object", tc.in, got)
				}
				return
			}
			value, ok := obj[tc.wantKey]
			if !ok {
				t.Errorf("toolInputObject(%q) = %q, missing key %q", tc.in, got, tc.wantKey)
				return
			}
			if tc.wantValue != "" {
				if s, _ := value.(string); s != tc.wantValue {
					t.Errorf("toolInputObject(%q) = %q, key %q = %v, want %q",
						tc.in, got, tc.wantKey, value, tc.wantValue)
				}
			}
		})
	}
}
