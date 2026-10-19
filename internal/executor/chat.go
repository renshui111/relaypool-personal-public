package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// ChatNonStream runs a non-streaming completion with failover.
func (e *Executor) ChatNonStream(ctx context.Context, req Request) (*Result, error) {
	return e.execute(ctx, req, func(ctx context.Context, item pool.Item, p provider.Provider, _ int) (*Result, *pool.Classified, error) {
		cred, err := e.credentialForContext(ctx, item.ID)
		if err != nil {
			return nil, e.credentialFailure(item, req, err), err
		}

		chat, err := p.ChatNonStream(ctx, cred, chatForAccount(req, item))
		if err != nil {
			if req.OnUpstreamFailure != nil {
				req.OnUpstreamFailure(err)
			}
			return nil, e.classify(item, req, err), err
		}
		if chat == nil || (req.RecoveryProbe && strings.TrimSpace(chat.Message.ContentText()) == "") {
			err := errors.New("上游没有返回文本，无法确认账号可用")
			if req.OnUpstreamFailure != nil {
				req.OnUpstreamFailure(err)
			}
			return nil, e.classify(item, req, err), err
		}
		return &Result{Chat: chat, Model: chat.Model}, nil, nil
	})
}

// ChatStream runs a streaming completion with failover.
//
// Failover is only possible BEFORE the response headers reach the client. Once
// the gateway has started streaming it cannot silently switch accounts, because
// the client would receive two interleaved answers. The attempt callback
// therefore only fails over while the upstream call itself is still failing,
// which is exactly the window in which nothing has been written yet.
func (e *Executor) ChatStream(ctx context.Context, req Request) (*Result, error) {
	req.Stream = true
	return e.execute(ctx, req, func(ctx context.Context, item pool.Item, p provider.Provider, _ int) (*Result, *pool.Classified, error) {
		cred, err := e.credentialForContext(ctx, item.ID)
		if err != nil {
			return nil, e.credentialFailure(item, req, err), err
		}

		chatRequest := chatForAccount(req, item)
		if chatRequest != nil {
			copy := *chatRequest
			copy.Stream = true
			chatRequest = &copy
		}
		stream, err := p.ChatStream(ctx, cred, chatRequest)
		if err != nil {
			if req.OnUpstreamFailure != nil {
				req.OnUpstreamFailure(err)
			}
			// Nothing has been sent to the client yet, so failing over is
			// still safe.
			return nil, e.classify(item, req, err), err
		}
		return &Result{Stream: stream, Model: stream.Model}, nil, nil
	})
}

// 在每次选号后分别转换，故障换号不会继承上一个账号的提示清理结果。
func chatForAccount(req Request, item pool.Item) *provider.ChatRequest {
	if req.Chat == nil || !item.DropSystemPrompt || req.PreserveSystemPrompt {
		return req.Chat
	}
	copy := *req.Chat
	copy.Messages = make([]provider.Message, 0, len(req.Chat.Messages))
	for _, message := range req.Chat.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "system" && role != "developer" {
			copy.Messages = append(copy.Messages, message)
		}
	}
	// 原始提示字段必须同步清除，格式、工具、预算和签名等选项继续保留。
	var raw map[string]json.RawMessage
	if json.Unmarshal(copy.Raw, &raw) == nil {
		for _, key := range []string{"messages", "system", "instructions", "input"} {
			delete(raw, key)
		}
		copy.Raw, _ = json.Marshal(raw)
	} else {
		copy.Raw = nil
	}
	// 保留 Responses 原生工具及加密推理项，只剔除上层指令消息。
	if len(copy.NativeInput) > 0 {
		var items []json.RawMessage
		if json.Unmarshal(copy.NativeInput, &items) == nil {
			kept := make([]json.RawMessage, 0, len(items))
			for _, item := range items {
				var entry struct {
					Role string `json:"role"`
				}
				_ = json.Unmarshal(item, &entry)
				role := strings.ToLower(strings.TrimSpace(entry.Role))
				if role != "system" && role != "developer" {
					kept = append(kept, item)
				}
			}
			copy.NativeInput, _ = json.Marshal(kept)
		} else {
			copy.NativeInput = nil
		}
	}
	return &copy
}

// credentialFor loads the stored credential for an account.
func (e *Executor) credentialFor(accountID string) (provider.Credential, error) {
	return e.credentialForContext(context.Background(), accountID)
}

func (e *Executor) credentialForContext(ctx context.Context, accountID string) (provider.Credential, error) {
	if e.credentials == nil {
		return provider.Credential{}, errors.New("executor: no credential source configured")
	}
	var cred provider.Credential
	var err error
	if source, ok := e.credentials.(interface {
		CredentialForContext(context.Context, string) (provider.Credential, error)
	}); ok {
		cred, err = source.CredentialForContext(ctx, accountID)
	} else {
		cred, err = e.credentials.CredentialFor(accountID)
	}
	if err != nil {
		return provider.Credential{}, err
	}
	if !cred.Ready() {
		return provider.Credential{}, provider.ErrNotReady
	}
	return cred, nil
}

// credentialFailure classifies a credential problem.
//
// A missing or incomplete credential is an ACCOUNT problem, so it cools that
// account and the request tries another. It is scoped to the whole account
// rather than one model, because a broken credential affects every model.
func (e *Executor) credentialFailure(item pool.Item, req Request, err error) *pool.Classified {
	if _, ok := provider.AsFailure(err); ok {
		return e.classify(item, req, err)
	}
	kind := pool.KindAuth
	status := 403
	code := "credential_invalid"
	if errors.Is(err, provider.ErrNotReady) {
		code = "not_ready"
	}
	classified := pool.Classify(status, err.Error(), "", kind, pool.ClassifyOptions{
		DefaultCooldown:      e.opts.DefaultCooldown,
		MaxRetryAfter:        e.opts.MaxRetryAfter,
		MinRateLimitCooldown: e.opts.MinRateLimitCooldown,
		Model:                "",
	})
	classified.Kind = kind
	classified.Failover = true
	classified.Code = code
	if classified.Cooldown <= 0 {
		classified.Cooldown = e.opts.DefaultCooldown
	}
	classified.Message = err.Error()
	return &classified
}

// classify converts a provider error into a pool classification.
//
// The adapter's own kind hint is used when present, because the adapter knows
// its protocol's error codes; the generic classifier then refines it from the
// status and body. Routing both through the same table keeps ONE failure policy
// instead of an adapter-specific one and a gateway-specific one that drift.
func (e *Executor) classify(item pool.Item, req Request, err error) *pool.Classified {
	if err == nil {
		return nil
	}

	// An unsupported operation is a REQUEST problem: no other account will
	// behave differently.
	if errors.Is(err, provider.ErrUnsupported) {
		classified := pool.Classified{
			Kind: pool.KindInvalidRequest, Failover: false,
			Status:  400,
			Code:    "unsupported",
			Message: err.Error(),
		}
		return &classified
	}
	if errors.Is(err, provider.ErrNotReady) {
		return e.credentialFailure(item, req, err)
	}

	failure, ok := provider.AsFailure(err)
	if !ok {
		// A transport-level error with no HTTP response at all: there is no
		// body to classify, so treat it as a transient upstream problem.
		classified := pool.Classify(0, err.Error(), "",
			pool.KindUnavailable, e.classifyOptions(req))
		// 无响应的连接故障影响账号出网，不能只屏蔽当前模型。
		classified.Model = ""
		return &classified
	}

	options := e.classifyOptions(req)
	options.QuotaScope = item.QuotaScope
	if item.QuotaScope == "shared" && item.QuotaGroup != "" {
		// 已明确共享边界时，仅隔离故障组，允许换到独立额度组。
		options.QuotaScope = "account"
	}
	classified := pool.Classify(
		failure.Status,
		failure.Body,
		retryAfterString(failure.RetryAfter),
		failure.KindHint,
		options,
	)
	// The adapter's own message is more specific than what the generic
	// extractor can recover from an upstream body.
	if classified.Message == "" && failure.Message != "" {
		classified.Message = failure.Message
	}
	if failure.Code != "" {
		classified.Code = failure.Code
	}
	if failure.NoRetry {
		classified.Failover = false
		if failure.Message != "" {
			classified.Message = failure.Message
		}
	}
	// 上游提示已交给统一分类器，不能再次覆盖并绕过冷却上下限。
	return &classified
}

// classifyOptions builds the classifier tunables for a request.
func (e *Executor) classifyOptions(req Request) pool.ClassifyOptions {
	return pool.ClassifyOptions{
		DefaultCooldown:      e.opts.DefaultCooldown,
		MaxRetryAfter:        e.opts.MaxRetryAfter,
		MinRateLimitCooldown: e.opts.MinRateLimitCooldown,
		// Scoping the cooldown to the model is what keeps one model's failure
		// from taking the whole account offline.
		Model: modelOf(req),
	}
}

// retryAfterString renders a duration in a format the classifier parses.
func retryAfterString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
