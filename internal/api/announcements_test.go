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
	var release struct {
		Release     string                                `json:"release"`
		PublishedOn string                                `json:"published_on"`
		Title       string                                `json:"title"`
		Summary     string                                `json:"summary"`
		Changes     []struct{ Title, Description string } `json:"changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &release); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse("2006-01-02", release.PublishedOn); err != nil {
		t.Fatal(err)
	}
	if release.Release == "" || release.Title == "" || release.Summary == "" || len(release.Changes) == 0 {
		t.Fatal("release content is incomplete")
	}
	for _, change := range release.Changes {
		if change.Title == "" || change.Description == "" {
			t.Fatal("empty release entry")
		}
	}
}
