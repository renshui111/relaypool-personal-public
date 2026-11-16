package api

import (
	_ "embed"
	"net/http"
)

// Version-controlled release notes are embedded with the running binary.
// Updating this file requires no database migration or browser storage reset.
//
//go:embed announcements.json
var announcementJSON []byte

func (s *Server) handleAnnouncements(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(announcementJSON)
}
