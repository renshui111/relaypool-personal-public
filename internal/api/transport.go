package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// Protocol identifiers, used in logs and to pick the response envelope.
const (
	protocolChatCompletions = "chat.completions"
	protocolMessages        = "messages"
	protocolResponses       = "responses"
)

// maxRequestBody caps a client request body.
//
// The cap is not optional: this endpoint is reachable by any local process
// holding the key, and an unbounded body would let one request exhaust memory.
const maxRequestBody = 32 << 20

// providerUsage aliases the provider usage type so the envelope files do not
// each need the provider import for one field.
type providerUsage = provider.Usage

// protocolEnvelope converts a neutral result back to a wire shape.
type protocolEnvelope interface {
	// renderNonStream builds the complete response body.
	renderNonStream(result *executor.Result, requestedModel string) ([]byte, error)
	// renderStreamChunk wraps one upstream SSE frame for this protocol.
	renderStreamChunk(frame []byte, state *streamState) ([]byte, bool)
	// renderStreamStart emits any protocol-specific preamble.
	renderStreamStart(state *streamState) []byte
	// renderStreamEnd emits the terminal frames.
	renderStreamEnd(state *streamState) []byte
	// renderError builds an error body for this protocol.
	renderError(status int, code, message string) []byte
	contentType() string
}

// streamState carries per-stream protocol bookkeeping.
type streamState struct {
	// 同协议事件直接回传，保留原生内容块、签名、加密推理项和事件编号。
	NativeProtocol string
	// ResponseID is the completion ID, learned from the first upstream frame.
	ResponseID string
	Model      string
	Created    int64
	// Started is when the stream began, for latency accounting.
	Started time.Time
	// SentRole tracks whether the assistant role has been emitted, so the
	// protocol adapter does not repeat it.
	SentRole bool
	// Finished guards against emitting a terminal frame twice.
	Finished bool
	// Usage accumulates token counts reported by the upstream.
	Usage provider.Usage
	// FinishReason is the upstream's own reason for stopping, captured from the
	// stream so the terminal frame can report it faithfully.
	//
	// WHY THIS EXISTS: the Anthropic stream used to derive stop_reason solely
	// from whether a tool block had been opened, so a TRUNCATED answer
	// (finish_reason "length") was reported as "end_turn" -- or as "tool_use"
	// when a call happened to be in flight. A client cannot distinguish a
	// truncated response from a complete one in that case, so it neither retries
	// nor warns. That is the silent-data-loss shape this project treats as the
	// worst failure mode. The non-streaming path already mapped this correctly
	// through anthropicStopReason; streaming now uses the same mapping.
	FinishReason string
	// TextBlockOpen records whether the text block was opened. A stream that
	// carries only tool calls never opens it, so closing it unconditionally
	// would emit a content_block_stop for a block the client never saw.
	TextBlockOpen bool
	// Tools tracks the lifecycle of streamed tool calls. See toolStreamState.
	Tools toolStreamState
}

// openTextBlock marks the text block as open and reports whether this was the
// first time, so a renderer emits content_block_start exactly once.
func (s *streamState) openTextBlock() (first bool) {
	if s.TextBlockOpen {
		return false
	}
	s.TextBlockOpen = true
	return true
}

// toolStreamState tracks which streamed tool-call blocks have been opened.
//
// WHY THIS EXISTS: the upstream sends ONE logical tool call across MANY frames.
// The first frame carries id + name + an empty-or-partial arguments string;
// every later frame carries only an arguments fragment. A renderer that treats
// each frame as a new call opens N blocks for one call and never closes them.
// That was a real defect: an Anthropic client received THREE content_block_start
// events for a single tool call, all on the same index, with id and name empty
// from the second frame on -- and the stream ended having closed only index 0.
//
// Both envelopes route tool handling through this so the lifecycle is reasoned
// about in exactly one place. Keeping a copy per protocol is what let the two
// diverge: one had a tool branch and the other had none at all, so one protocol
// duplicated every tool call while another silently dropped them.
type toolStreamState struct {
	// upstreamOrder lists upstream indices in first-seen order, so terminal
	// frames close blocks in the order they were opened.
	upstreamOrder []int
	// blockOf maps an upstream delta index to the output block index allocated
	// to it.
	blockOf map[int]int
	// nextBlock is the next output block index to hand out.
	nextBlock int
}

// openTool reports whether this is the FIRST frame for the given upstream tool
// index, and gives the output block index to use.
//
// The upstream `index` is a small integer starting at 0 that identifies the call
// WITHIN one stream. It cannot be used as the output block index directly,
// because block 0 is reserved for text in both protocols; this method owns that
// mapping so no caller invents one.
//
// first == false means "continuation": emit a delta, not a start.
//
// A negative index reports (-1, false): it has no block. toolIndex never
// produces one (it clamps to 0), so this is defensive, but it keeps the
// contract "a non-negative index gets a real block, anything else gets none"
// true for every caller rather than only for today's.
func (t *toolStreamState) openTool(upstreamIndex int) (block int, first bool) {
	if upstreamIndex < 0 {
		return -1, false
	}
	if t.blockOf == nil {
		t.blockOf = map[int]int{}
		// Block 0 is the text part in both protocols, so tool blocks start at 1.
		t.nextBlock = 1
	}
	if existing, ok := t.blockOf[upstreamIndex]; ok {
		return existing, false
	}
	block = t.nextBlock
	t.nextBlock++
	t.blockOf[upstreamIndex] = block
	t.upstreamOrder = append(t.upstreamOrder, upstreamIndex)
	return block, true
}

// blockFor reports the output block index already allocated to an upstream tool
// index, or -1 when it was never opened.
func (t *toolStreamState) blockFor(upstreamIndex int) int {
	if t.blockOf == nil {
		return -1
	}
	if block, ok := t.blockOf[upstreamIndex]; ok {
		return block
	}
	return -1
}

// openedUpstreamIndices returns the upstream tool indices that have been opened,
// in first-seen order, so a terminal renderer can close each exactly once.
func (t *toolStreamState) openedUpstreamIndices() []int {
	return t.upstreamOrder
}

// opened returns whether any tool block was opened.
func (t *toolStreamState) opened() bool { return len(t.upstreamOrder) > 0 }

// toolIndex extracts the upstream index of a streamed tool-call delta.
//
// A missing or malformed index is treated as 0 rather than as a new call: some
// upstreams omit it on the opening frame, and inventing a fresh index per frame
// would recreate the repeated-start defect for those upstreams.
func toolIndex(call any) int {
	entry, ok := call.(map[string]any)
	if !ok {
		return 0
	}
	switch v := entry["index"].(type) {
	case float64:
		if v >= 0 {
			return int(v)
		}
	case int:
		if v >= 0 {
			return v
		}
	case int64:
		if v >= 0 {
			return int(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= 0 {
			return int(n)
		}
	}
	return 0
}

// serveChat runs one request through the executor and writes the response.
//
// The HTTP layer's whole job is translation: parse the wire shape, hand the
// neutral request to the executor, and render whatever comes back. All routing
// and failure policy lives in the executor, which is what keeps the two
// concerns from tangling.
func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, protocol string) {
	envelope, err := envelopeFor(protocol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	requestID := requestIDFrom(r.Context())
	model, providerFilter := s.modelRoute(req.Model)
	allowedAccounts, ok := s.permittedRouteAccounts(w, r, model, providerFilter)
	if !ok {
		return
	}
	policy := keyFromRequest(r).Permissions
	pinned, _ := r.Context().Value(pinnedAccountContextKey{}).(string)
	preferred := strings.TrimSpace(r.Header.Get("X-RelayPool-Account"))
	for _, id := range []string{pinned, preferred} {
		if id == "" {
			continue
		}
		item, exists := s.pool.ByID(id)
		if !policy.AllowsAccount(id) || (exists && !s.keyAllowsModel(policy, model, item.Provider)) {
			writeError(w, 403, "key_permission_denied", "当前密钥未获准调用指定账号或型号")
			return
		}
	}
	req.Model = model

	// Resolve the armor before dispatch so the substitution is visible to the
	// routing layer and to the response headers. The preset is chosen from the
	// requested model, so the same request always sends the same prompt no
	// matter which account the scheduler picks.
	armorChannel := providerFilter
	if id, ok := r.Context().Value(pinnedAccountContextKey{}).(string); ok {
		if item, exists := s.pool.ByID(id); exists {
			armorChannel = item.Provider
		}
	}
	armorModel := armorModelForChannel(model, armorChannel)
	armorDecision := s.armorDecision(r, armorModel)
	armorApplied := applyArmor(req, armorDecision)
	execReq := executor.Request{
		ID:                   requestID,
		Chat:                 req,
		PreserveSystemPrompt: armorApplied,
		SessionKey:           keySessionFor(r, req),
		PreferAccount:        strings.TrimSpace(r.Header.Get("X-RelayPool-Account")),
		ProviderFilter:       providerFilter,
		AllowedAccounts:      allowedAccounts,
		Stream:               req.Stream,
	}
	if accountID, ok := r.Context().Value(pinnedAccountContextKey{}).(string); ok {
		execReq.OnlyAccount = accountID
	}

	// Record the request before dispatching so a long stream is visible in the
	// console while it is still running.
	logEntry := s.beginRequest(r, requestID, protocol, req, model)
	// Carry the armor decision so the error path can report it too.
	decisionCopy := armorDecision
	logEntry.armorDecision = &decisionCopy

	if req.Stream {
		s.serveStream(w, r, execReq, envelope, logEntry, model, armorDecision, armorApplied)
		return
	}
	s.serveNonStream(w, r, execReq, envelope, logEntry, model, armorDecision, armorApplied)
}

// serveNonStream handles the buffered response path.
func (s *Server) serveNonStream(w http.ResponseWriter, r *http.Request, execReq executor.Request, envelope protocolEnvelope, logEntry *requestLogEntry, model string, armorDecision armor.Decision, armorApplied bool) {
	result, err := s.executor.ChatNonStream(r.Context(), execReq)
	if err != nil {
		s.failRequest(w, logEntry, envelope, err)
		return
	}

	body, err := envelope.renderNonStream(result, model)
	if err != nil {
		log.Printf("render response for %s: %v", execReq.ID, err)
		if logEntry != nil {
			logEntry.upstreamCompleted = true
			if result.Chat != nil {
				logEntry.upstreamUsage = &result.Chat.Usage
			}
		}
		s.failRequest(w, logEntry, envelope, &executor.FailureError{
			Status: 500, Code: "render_failed",
			Message: "failed to render the response",
			Cause:   err,
		})
		return
	}

	// Reflect the account that served the request: it is the single most
	// useful piece of debugging information for a pool.
	setRelayHeaders(w, result.AccountID, result.Provider, result.AttemptCount)
	// The armor header is set on EVERY response, including "none", so a caller
	// that asked for the substitution can tell "not applied" from "the header
	// was stripped by a proxy".
	setArmorHeaders(w, armorDecision)

	w.Header().Set("Content-Type", envelope.contentType())
	w.WriteHeader(http.StatusOK)
	// 即使客户端断开，也要完成请求日志记录。
	defer s.finishRequest(logEntry, result, nil)
	if _, err := w.Write(body); err != nil {
		log.Printf("write response for %s: %v", execReq.ID, err)
		return
	}
}

// setRelayHeaders advertises which account served a request.
func setRelayHeaders(w http.ResponseWriter, accountID, providerName string, attempts int) {
	if accountID != "" {
		w.Header().Set("X-RelayPool-Account", accountID)
	}
	if providerName != "" {
		w.Header().Set("X-RelayPool-Provider", providerName)
	}
	if attempts > 0 {
		w.Header().Set("X-RelayPool-Attempts", itoa(attempts))
	}
}

// serveStream handles the SSE path.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, execReq executor.Request, envelope protocolEnvelope, logEntry *requestLogEntry, model string, armorDecision armor.Decision, armorApplied bool) {
	_, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, a "streamed" response arrives all at once, which
		// defeats the point. Fail loudly rather than silently buffering.
		s.failRequest(w, logEntry, envelope, &executor.FailureError{Status: http.StatusInternalServerError, Code: "streaming_unsupported", Message: "当前连接不支持流式响应"})
		return
	}

	result, err := s.executor.ChatStream(r.Context(), execReq)
	if err != nil {
		// Nothing has been written yet, so a normal error response is still
		// possible. This is the window in which failover happened.
		s.failRequest(w, logEntry, envelope, err)
		return
	}
	defer result.Stream.Close()
	timeout := s.streamWriteTimeout
	if timeout <= 0 {
		timeout = defaultStreamWriteTimeout
	}
	writer := &streamResponseWriter{ResponseWriter: w, controller: http.NewResponseController(w), timeout: timeout}
	defer func() { _ = writer.controller.SetWriteDeadline(time.Time{}) }()

	// Commit to streaming: headers and the preamble go out now, after which
	// the status code can no longer change.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Tell buffering proxies not to accumulate the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	setRelayHeaders(w, result.AccountID, result.Provider, result.AttemptCount)
	setArmorHeaders(w, armorDecision)
	w.WriteHeader(http.StatusOK)

	state := &streamState{
		ResponseID: "chatcmpl-" + execReq.ID,
		Model:      model,
		Created:    time.Now().Unix(),
		Started:    time.Now(),
	}
	switch envelope.(type) {
	case anthropicEnvelope:
		if result.Stream.NativeProtocol == "anthropic" {
			state.NativeProtocol = "anthropic"
		}
	case responsesEnvelope:
		if result.Stream.NativeProtocol == "responses" {
			state.NativeProtocol = "responses"
		}
	}
	if preamble := envelope.renderStreamStart(state); state.NativeProtocol == "" && len(preamble) > 0 {
		if err := writeStreamChunk(writer, writer, preamble); err != nil {
			s.finishRequest(logEntry, result, err)
			return
		}
	}

	err = s.pipeStream(r.Context(), writer, writer, result.Stream, envelope, state, logEntry)
	if err != nil {
		// The status is already committed, so the only way to signal a problem
		// is to stop writing. Closing the stream without a terminal frame
		// tells a correct client the answer was truncated.
		log.Printf("stream %s ended early: %v", execReq.ID, err)
		s.finishRequest(logEntry, result, err)
		return
	}

	if trailer := envelope.renderStreamEnd(state); state.NativeProtocol == "" && len(trailer) > 0 {
		if err := writeStreamChunk(writer, writer, trailer); err != nil {
			s.finishRequest(logEntry, result, err)
			return
		}
	}
	if r.Context().Err() == nil {
		s.pool.MarkAttemptOK(result.AttemptItem, model)
	}
	s.finishRequest(logEntry, result, nil)
}

// pipeStream forwards sanitized upstream frames, translating each one.
func (s *Server) pipeStream(ctx context.Context, w io.Writer, flusher httpFlusher, stream *provider.Stream, envelope protocolEnvelope, state *streamState, logEntry *requestLogEntry) error {
	reader := newSSEFrameReader(stream.Body)

	for {
		if err := ctx.Err(); err != nil {
			// The client went away. Stop reading so the upstream call can be
			// abandoned rather than draining a stream nobody wants.
			return err
		}

		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if (state.NativeProtocol != "" && !state.Finished) || (state.NativeProtocol == "" && state.FinishReason == "") {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		if frame == nil {
			continue
		}
		if frame.IsDone {
			if state.NativeProtocol != "" && !state.Finished {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if event, ok := frame.Parsed["_relaypool_native_event"].(map[string]any); ok {
			if state.NativeProtocol != "" {
				kind, _ := event["type"].(string)
				payload, err := json.Marshal(event)
				if err != nil {
					return err
				}
				if err = writeStreamChunk(w, flusher, sseEvent(kind, payload)); err != nil {
					return err
				}
				if logEntry != nil && logEntry.ttfbMs == nil {
					ms := int(time.Since(state.Started).Milliseconds())
					logEntry.ttfbMs = &ms
				}
				if kind == "message_stop" || kind == "response.completed" || kind == "response.incomplete" {
					state.Finished = true
				}
			}
			continue
		}

		// Learn the response identity from the first frame that carries it.
		if frame.ID != "" {
			state.ResponseID = frame.ID
		}
		if frame.Model != "" {
			state.Model = frame.Model
			if logEntry != nil {
				logEntry.streamModel = frame.Model
			}
		}
		if frame.Created != 0 {
			state.Created = frame.Created
		}
		if frame.Usage != nil {
			state.Usage = *frame.Usage
			if logEntry != nil {
				usage := *frame.Usage
				if hasReportedTokenUsage(frame.Parsed) {
					logEntry.streamUsage = &usage
					logEntry.streamTokensReported = true
				} else if usage.Credits > 0 {
					// 后续空用量帧不能抹掉先前有效的 Token 计数。
					if logEntry.streamUsage == nil {
						logEntry.streamUsage = &provider.Usage{}
					}
					logEntry.streamUsage.Credits = usage.Credits
				}
			}
		}
		// Keep the last finish reason the upstream stated. The terminal frame
		// needs it to report truncation honestly rather than inferring a reason
		// from the shape of the content.
		if frame.FinishReason != "" {
			state.FinishReason = frame.FinishReason
		}

		if state.NativeProtocol != "" {
			continue
		}
		out, keep := envelope.renderStreamChunk(frame.Raw, state)
		if !keep || len(out) == 0 {
			continue
		}
		if err := writeStreamChunk(w, flusher, out); err != nil {
			return err
		}

		// TTFB is the time to the first frame the client can act on.
		if logEntry != nil && logEntry.ttfbMs == nil {
			ms := int(time.Since(state.Started).Milliseconds())
			logEntry.ttfbMs = &ms
		}
	}
}

// httpFlusher is the subset of http.Flusher this file needs.
type httpFlusher interface {
	Flush()
}

// failRequest writes a classified error response.
func (s *Server) failRequest(w http.ResponseWriter, logEntry *requestLogEntry, envelope protocolEnvelope, err error) {
	status, code, message, retryAfter := classifyForClient(err)

	// The armor header is reported on the error path too. The substitution had
	// already happened by the time the request failed, so a caller that asked
	// for the armor still needs to know what was sent -- and reporting it only
	// on success would make the header's absence ambiguous.
	if logEntry != nil && logEntry.armorDecision != nil {
		setArmorHeaders(w, *logEntry.armorDecision)
	}

	if retryAfter > 0 {
		// Round up so a sub-second remainder never becomes "Retry-After: 0",
		// which a client reads as "retry immediately".
		seconds := int(retryAfter.Seconds())
		if retryAfter%time.Second != 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", itoa(seconds))
	}

	w.Header().Set("Content-Type", envelope.contentType())
	w.WriteHeader(status)
	if _, writeErr := w.Write(envelope.renderError(status, code, message)); writeErr != nil {
		log.Printf("write error response: %v", writeErr)
	}
	s.finishRequest(logEntry, nil, err)
}

// classifyForClient maps an executor error onto a client-facing status.
//
// The distinction that matters: a 4xx from a bad request must stay a 4xx so the
// client fixes its request rather than retrying, while a genuine upstream
// outage becomes a 5xx so a retry is appropriate.
func classifyForClient(err error) (status int, code, message string, retryAfter time.Duration) {
	var failure *executor.FailureError
	if errors.As(err, &failure) {
		status = failure.Status
		if status < 400 {
			status = 502
		}
		code = failure.Code
		message = failure.Message
		retryAfter = failure.RetryAfter
		if code == "" {
			code = "upstream_error"
		}
		if message == "" {
			message = "the upstream request failed"
		}
		return status, code, message, retryAfter
	}

	// A bare pool error still carries a classification worth surfacing.
	if kind := poolErrorKind(err); kind != "" {
		status = 503
		if kind == pool.KindRateLimit || kind == pool.KindQuota {
			status = 429
		}
		return status, kind, err.Error(), 0
	}

	return 502, "upstream_error", err.Error(), 0
}

// poolErrorKind extracts a pool kind from an error chain if present.
func poolErrorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "request_canceled"
	}
	return ""
}

// sessionKeyFor resolves the sticky-routing key for a request.
//
// Precedence: an explicit header wins, then a content fingerprint. The header
// exists for clients that can supply a conversation ID; the fingerprint makes
// stickiness work with no client change at all.
func sessionKeyFor(r *http.Request, req *provider.ChatRequest) string {
	if explicit := strings.TrimSpace(r.Header.Get("X-RelayPool-Session")); explicit != "" {
		return explicit
	}
	// The Anthropic header is accepted too, so Claude Code works unchanged.
	if explicit := strings.TrimSpace(r.Header.Get("X-Claude-Session-Id")); explicit != "" {
		return explicit
	}
	if !sessionsEnabled {
		return ""
	}
	return executor.ContentSessionKey(sessionSeedFrom(req.Messages))
}

// sessionsEnabled is a package-level switch so the API layer need not thread
// the config through every helper. It is set once at construction.
var sessionsEnabled = true

// envelopeFor returns the protocol adapter for a name.
func envelopeFor(protocol string) (protocolEnvelope, error) {
	switch protocol {
	case protocolChatCompletions:
		return chatCompletionsEnvelope{}, nil
	case protocolMessages:
		return anthropicEnvelope{}, nil
	case protocolResponses:
		return responsesEnvelope{}, nil
	default:
		return nil, errors.New("unsupported protocol: " + protocol)
	}
}

// itoa renders a non-negative int without importing strconv into this file's
// call sites.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// marshalJSON encodes without HTML escaping so IDs and model names survive
// verbatim.
func marshalJSON(v any) ([]byte, error) {
	buf := &strings.Builder{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}

// readBody reads and caps a request body.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("request body is empty")
	}
	limited := http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("request body is too large or unreadable")
	}
	if len(body) == 0 {
		return nil, errors.New("request body is empty")
	}
	return body, nil
}
