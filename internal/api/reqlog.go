package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// requestLogEntry accumulates one request's outcome before it is written.
//
// The row is created BEFORE dispatch so a long stream is visible in the console
// while it is still running, then finalized once the outcome is known. That
// two-phase write is why the entry is a mutable struct rather than a value
// passed to the store twice.
type requestLogEntry struct {
	// 转换失败仍保留上游完成状态及实际用量，不能误按调用失败释放预算。
	upstreamCompleted bool
	upstreamUsage     *provider.Usage
	id                string
	protocol          string
	stream            bool

	requestedModel string
	mappedModel    string

	// ttfbMs is set on the first streamed frame the client can act on.
	ttfbMs *int
	// 保留流式上游实际报告的用量和型号，不能仅依赖非流式结果。
	streamUsage          *provider.Usage
	streamTokensReported bool
	streamModel          string

	// armorDecision records what the armor did for this request. It is kept
	// here so the error path can still report it: the substitution happens
	// before dispatch, so a failed request still needs to say what was sent.
	armorDecision *armor.Decision

	startedAt time.Time
}

// beginRequest inserts the initial row and returns the entry to finalize.
//
// A store failure is logged and swallowed: request logging must never be able
// to fail a completion, because the gateway's job is to serve traffic.
func (s *Server) beginRequest(r *http.Request, requestID, protocol string, req *provider.ChatRequest, model string) *requestLogEntry {
	entry := &requestLogEntry{
		id:             requestID,
		protocol:       protocol,
		stream:         req.Stream,
		requestedModel: req.Model,
		mappedModel:    model,
		startedAt:      time.Now(),
	}
	if s.store == nil || !s.cfg.Logging.RequestLogEnabled {
		return entry
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := s.store.StartRequest(ctx, store.RequestLog{
		ID:             requestID,
		APIKeyID:       keyFromRequest(r).ID,
		Protocol:       protocol,
		RequestedModel: req.Model,
		MappedModel:    model,
		Stream:         req.Stream,
		Status:         store.StatusStreaming,
	}); err != nil {
		log.Printf("api: start request log %s: %v", requestID, err)
	}
	return entry
}

// finishRequest writes the final outcome.
func (s *Server) finishRequest(entry *requestLogEntry, result *executor.Result, requestErr error) {
	if entry == nil || s.store == nil || !s.cfg.Logging.RequestLogEnabled {
		return
	}

	update := store.RequestLog{
		ID:             entry.id,
		Protocol:       entry.protocol,
		RequestedModel: entry.requestedModel,
		MappedModel:    entry.mappedModel,
		Stream:         entry.stream,
		FinishedAt:     time.Now().UTC(),
		TTFBMs:         entry.ttfbMs,
	}

	latency := int(time.Since(entry.startedAt).Milliseconds())
	update.LatencyMs = &latency
	if entry.armorDecision != nil {
		update.ArmorPreset = armorHeaderValue(*entry.armorDecision)
	}

	if result != nil {
		update.Status = store.StatusOK
		update.AccountID = result.AccountID
		update.Provider = result.Provider
		update.AttemptCount = result.AttemptCount
		if result.Chat != nil {
			if result.Chat.Model != "" {
				update.MappedModel = result.Chat.Model
			}
			update.PromptTokens = intPtrOrNil(result.Chat.Usage.PromptTokens)
			update.CompletionTokens = intPtrOrNil(result.Chat.Usage.CompletionTokens)
			update.TotalTokens = intPtrOrNil(result.Chat.Usage.TotalTokens)
			if result.Chat.Usage.Credits > 0 {
				credits := result.Chat.Usage.Credits
				update.Credits = &credits
			}
		}
	} else {
		update.Status = store.StatusError
		if requestErr != nil {
			status, _, message, _ := classifyForClient(requestErr)
			_ = status
			update.ErrorMessage = message
			if failure, ok := asFailureError(requestErr); ok {
				update.ErrorKind = failure.Kind
				update.ErrorCode = failure.Code
				update.AccountID = failure.LastAccountID
				// The provider is what the console filters on, so a failed
				// request must carry it: without this, filtering the log by
				// channel hides exactly the failures being investigated.
				update.Provider = failure.LastProvider
				if failure.Attempts > 0 {
					update.AttemptCount = failure.Attempts
				}
			}
		}
	}
	if requestErr != nil {
		// 已选中账号也可能发生断流或取消，不能因有 result 就记成成功。
		update.Status = store.StatusError
		_, _, update.ErrorMessage, _ = classifyForClient(requestErr)
		if update.ErrorKind == "" && entry.stream {
			update.ErrorKind = "stream_error"
		}
	}
	if entry.streamModel != "" {
		update.MappedModel = entry.streamModel
	}
	if entry.streamUsage != nil {
		if entry.streamTokensReported {
			// 已明确上报的零值与未上报不同；零值同样属于有效用量样本。
			prompt, completion, total := entry.streamUsage.PromptTokens, entry.streamUsage.CompletionTokens, entry.streamUsage.TotalTokens
			update.PromptTokens, update.CompletionTokens, update.TotalTokens = &prompt, &completion, &total
		}
		if entry.streamUsage.Credits > 0 {
			credits := entry.streamUsage.Credits
			update.Credits = &credits
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.FinishRequest(ctx, update); err != nil {
		log.Printf("api: finish request log %s: %v", entry.id, err)
	}
}

// asFailureError extracts an executor failure from an error chain.
func asFailureError(err error) (*executor.FailureError, bool) {
	var failure *executor.FailureError
	if errors.As(err, &failure) {
		return failure, true
	}
	return nil, false
}

// intPtrOrNil reports a token count only when it is known.
//
// A zero is deliberately NOT reported: the upstream omitting a count is
// different from the upstream reporting zero, and a fabricated zero would
// corrupt any usage accounting built on top of these logs.
func intPtrOrNil(value int) *int {
	if value <= 0 {
		return nil
	}
	v := value
	return &v
}
