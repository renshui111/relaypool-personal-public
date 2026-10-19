package api

import (
	"net/http"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

// trimSpace is a local alias so the hot validation path reads cleanly.
func trimSpace(s string) string { return strings.TrimSpace(s) }

// Protocol dispatch.
//
// Each handler's whole job is: parse the wire shape into the neutral request,
// hand it to the transport, and let the transport render. Keeping the handlers
// this thin is what stops three protocols from growing three copies of the
// routing and failure logic.

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if err := validateChatRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.serveChat(w, r, req, protocolChatCompletions)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req, err := parseAnthropicRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if err := validateChatRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.serveChat(w, r, req, protocolMessages)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req, err := parseResponsesRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if err := validateChatRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.serveChat(w, r, req, protocolResponses)
}

// validateChatRequest rejects requests that cannot possibly succeed.
//
// Catching these locally matters: an empty message list sent upstream would
// come back as an opaque error, and because we classify upstream errors, a
// vague rejection risks being misread as a transient failure and re-tried
// against every account.
func validateChatRequest(req *provider.ChatRequest) error {
	if req == nil {
		return errString("request is empty")
	}
	if trimSpace(req.Model) == "" {
		return errString("model is required")
	}
	if len(req.Messages) == 0 {
		return errString("messages must not be empty")
	}
	for _, message := range req.Messages {
		if trimSpace(message.Role) == "" {
			return errString("every message requires a role")
		}
	}
	return nil
}

// errString is a tiny error type so this file needs no fmt import for one call.
type errString string

func (e errString) Error() string { return string(e) }
