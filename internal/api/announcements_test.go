package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAnnouncementsRequireAdminAndServeValidRelease(t *testing.T) {
	s := newTestServerNoAdapter(t)
	created := callComplete(t, s, "POST", "/admin/keys", `{"name":"announcement inference"}`)
	var key struct{ Key string }
	if err := json.Unmarshal(created.Body.Bytes(), &key); err != nil || key.Key == "" {
		t.Fatal("could not create inference key")
	}
	if w := permissionCall(t, s, key.Key, "GET", "/admin/announcements", ""); w.Code != 403 {
		t.Fatalf("inference key received status %d", w.Code)
	}
	w := callComplete(t, s, "GET", "/admin/announcements", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected response: %d %v", w.Code, w.Header())
	}
	type releaseNote struct {
		Release     string                                `json:"release"`
		PublishedOn string                                `json:"published_on"`
		Title       string                                `json:"title"`
		Summary     string                                `json:"summary"`
		Changes     []struct{ Title, Description string } `json:"changes"`
		History     []releaseNote                         `json:"history"`
	}
	var release releaseNote
	if err := json.Unmarshal(w.Body.Bytes(), &release); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, note := range append([]releaseNote{release}, release.History...) {
		if _, err := time.Parse("2006-01-02", note.PublishedOn); err != nil {
			t.Fatal(err)
		}
		if note.Release == "" || note.Title == "" || note.Summary == "" || len(note.Changes) == 0 {
			t.Fatal("release content is incomplete")
		}
		if seen[note.Release] {
			t.Fatalf("duplicate release %s", note.Release)
		}
		seen[note.Release] = true
		for _, change := range note.Changes {
			if change.Title == "" || change.Description == "" {
				t.Fatal("empty release entry")
			}
		}
	}
	for _, archived := range []string{"2026.11.16", "2026.11.16.1", "2026.11.16.2"} {
		if !seen[archived] || release.Release == archived {
			t.Fatalf("missing archived release %s", archived)
		}
	}
	previousDate := release.PublishedOn
	for _, note := range release.History {
		if note.PublishedOn > previousDate || len(note.History) != 0 {
			t.Fatal("history must be flat and ordered newest first")
		}
		previousDate = note.PublishedOn
	}
}
