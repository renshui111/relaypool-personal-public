// Package provider defines the contract every upstream channel implements.
//
// The central design decision here is the Kind split. Channels differ in RUNTIME
// SHAPE, not just in protocol details, and the difference is not cosmetic:
//
//   - WorkBuddy and Trae are plain HTTP + SSE. One http.Client, stateless,
//     cheap. Many accounts can share the process.
//   - 当前 Qoder 适配器直接调用桌面 HTTP 协议，签名会话按账号隔离。
//     使用官方 CLI/WASM 的其他适配器仍须独立子进程和 HOME。
//
// Folding those two into one code path produces conditionals scattered through
// the executor. Making the distinction an explicit field instead keeps the
// branch in one place and makes the reason readable at the call site.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Kind is the runtime shape of a channel.
type Kind int

const (
	// KindInProcess is a stateless HTTP/SSE adapter served from this process.
	KindInProcess Kind = iota
	// KindSubprocess requires one child process with its own HOME per account.
	KindSubprocess
)

// String renders the kind for logs.
func (k Kind) String() string {
	switch k {
	case KindSubprocess:
		return "subprocess"
	default:
		return "in-process"
	}
}

// Region identifies which upstream deployment an account belongs to.
//
// CN and Global are genuinely different hosts with different paths, headers and
// origins. They must never be mixed: sending a Global Origin to the CN host (or
// the CN catalogue path to Global) is an immediate rejection.
type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"
)

// NormalizeRegion canonicalizes a region string.
func NormalizeRegion(region string) Region {
	if strings.EqualFold(strings.TrimSpace(region), string(RegionGlobal)) {
		return RegionGlobal
	}
	return RegionCN
}

// Errors the registry and adapters return.
var (
	// ErrUnsupported means the channel does not implement this operation.
	// Checkin is the common case: not every upstream has a daily check-in.
	ErrUnsupported = errors.New("provider: operation not supported")
	// ErrNotReady means the account has no usable credential yet.
	ErrNotReady = errors.New("provider: account is not ready")
	// ErrUnknownProvider means no adapter is registered for a family name.
	ErrUnknownProvider = errors.New("provider: unknown provider family")
)

// Credential is a decoded provider credential.
//
// The raw bytes are kept alongside the parsed form because each provider owns
// its own on-disk schema and the store must be able to round-trip a blob it
// does not understand.
type Credential struct {
	// AccountID ties the credential back to its pool entry.
	AccountID string
	// Provider is the family name, e.g. "workbuddy".
	Provider string
	// Region is the deployment the credential authenticates against.
	Region Region
	// Raw is the stored blob, exactly as it was persisted.
	Raw []byte
	// PreviousRaw 供刷新写回时比较，防止旧刷新结果覆盖用户刚替换的凭证。
	PreviousRaw []byte
	// Fields holds provider-specific decoded values.
	Fields map[string]string
	// ExpiresAt is when the access token stops being valid. Zero means
	// unknown, which adapters must treat as "try it and see" rather than
	// "expired".
	ExpiresAt time.Time
}

// Field reads a decoded provider-specific value.
func (c Credential) Field(name string) string {
	if c.Fields == nil {
		return ""
	}
	return strings.TrimSpace(c.Fields[name])
}

// Ready reports whether the credential carries enough to attempt a request.
//
// A per-provider "ready" verdict lives in the adapter (which knows its own
// minimum requirements); this base check answers the weaker question the
// executor needs: is there anything here at all to try? A credential that fails
// this must not be used, and the executor treats that as an account-level
// failure so the request moves on to another account.
func (c Credential) Ready() bool {
	if token := c.Field("access_token"); token != "" {
		return true
	}
	return len(c.Raw) > 0
}

// ModelInfo describes one model advertised by one account.
type ModelInfo struct {
	// NativeModel is the exact ID to send upstream. Case matters here.
	NativeModel string
	// PublicModel is the ID clients may request. Usually identical, but the
	// split exists because some channels need a prefixed public alias.
	PublicModel string
	DisplayName string

	ContextWindow int
	MaxOutput     int

	// Capability flags. Tools is tri-state in practice (unknown/enabled) but
	// is modelled as a bool because every WorkBuddy model supports tools.
	Tools     bool
	Images    bool
	Reasoning bool
	// ReasoningOptions lists accepted effort levels, lowest to highest, when
	// Reasoning is true. "none" means thinking can be disabled.
	ReasoningOptions []string
	// ReasoningDefault is the level used when a request does not specify one.
	ReasoningDefault string
	// CanDisableThinking reports whether "none" is acceptable. Some models
	// always think, and sending "none" to them is an error.
	CanDisableThinking bool
}

// Usage reports token accounting for one completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// Credits is the upstream's own accounting unit, when it reports one.
	Credits float64
}

// ChatRequest is the provider-neutral chat request.
//
// It is deliberately close to the OpenAI shape rather than to any one upstream:
// the adapters translate outward from here, so adding a channel never requires
// touching the gateway. Payload carries the caller's original JSON so an adapter
// can pass through fields the neutral struct does not model.
type ChatRequest struct {
	Model    string
	Messages []Message

	// Tools and ToolChoice are passed through verbatim; the adapters
	// normalize them to whatever shape their upstream accepts.
	Tools      json.RawMessage
	ToolChoice json.RawMessage

	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	// StrictTokenLimit 用于短消息检查，禁止适配器自动扩大输出预算。
	StrictTokenLimit bool
	Stop             []string

	// ReasoningEffort is the requested thinking level, already normalized.
	// Empty means "use the account default".
	ReasoningEffort string
	// DisableThinking requests that reasoning be turned off entirely.
	DisableThinking bool

	// Stream asks for SSE. Adapters that only support streaming upstream
	// ignore this on the wire and the executor adapts the result.
	Stream bool

	// Raw is the original request body, used for pass-through fields.
	Raw json.RawMessage
	// NativeInput 保留 Responses 的原生输入项；转换时仍须剔除旧系统消息。
	NativeInput json.RawMessage
}

// Message is one conversation turn. Content may be a plain string or a
// structured parts array, so it is carried as raw JSON.
type Message struct {
	Role    string
	Content json.RawMessage
	// NativeContent 保留原生内容块及签名，供同协议适配器无损回传。
	NativeContent    json.RawMessage
	ReasoningContent string
	// Name is the optional tool/function name on a tool result message.
	Name string
	// ToolCalls carries assistant tool invocations.
	ToolCalls json.RawMessage
	// ToolCallID links a tool result back to its invocation.
	ToolCallID string
}

// ContentText extracts the plain-text form of a message, which is what prompt
// sanitizers and content fingerprinting need.
func (m Message) ContentText() string {
	if len(m.Content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return text
	}
	// Structured parts: concatenate the text parts in order.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ChatResult is a completed non-streaming completion.
type ChatResult struct {
	ID      string
	Model   string
	Created int64
	Message Message
	// FinishReason is the OpenAI-style reason: stop, length, tool_calls.
	FinishReason string
	Usage        Usage
	// NativeOutput 保留 Responses 原生输出，包含工具项和加密推理回放数据。
	NativeOutput json.RawMessage
}

// Stream is an open SSE response from an upstream.
//
// The adapter is responsible for sanitizing the stream before it reaches the
// client: upstreams emit placeholder fields that OpenAI clients render as a
// flood of empty chunks, so cleaning has to happen in the adapter that knows
// the quirk.
type Stream struct {
	// Body must be closed by the caller.
	Body io.ReadCloser
	// ContentType is echoed to the client.
	ContentType string
	// Model is the upstream model that actually served the request, when the
	// adapter learns it from response headers.
	Model string
	// NativeProtocol 指明同协议客户端可接收原生事件，避免签名或加密项丢失。
	NativeProtocol string
}

// Close releases the stream.
func (s *Stream) Close() error {
	if s == nil || s.Body == nil {
		return nil
	}
	return s.Body.Close()
}

// CheckinResult reports the outcome of a daily check-in.
type CheckinResult struct {
	// Status is "success", "already" or "error".
	Status  string
	Message string
}

// Checkin statuses.
const (
	CheckinSuccess = "success"
	CheckinAlready = "already"
	CheckinError   = "error"
	CheckinSkipped = "skipped"
)

// Provider is one upstream channel family.
//
// Implementations must be safe for concurrent use: the executor calls them from
// many goroutines, one per in-flight request.
type Provider interface {
	// Family is the stable name used in configuration and routing, e.g.
	// "workbuddy". It must be lowercase and URL-safe.
	Family() string

	// Kind reports the runtime shape, which decides how the executor manages
	// the account's lifecycle.
	Kind() Kind

	// DecodeCredential parses a stored blob. It must validate enough to catch
	// a wrong-provider blob while staying lenient about extra fields, because
	// upstreams add fields without notice.
	DecodeCredential(raw []byte) (Credential, error)

	// FetchCatalog lists the models this account can serve. A failure here
	// must NOT make the account unusable: the caller treats an error as
	// "catalogue unknown", and unknown fails open.
	FetchCatalog(ctx context.Context, cred Credential) ([]ModelInfo, error)

	// ChatNonStream performs a completion and returns it whole.
	ChatNonStream(ctx context.Context, cred Credential, req *ChatRequest) (*ChatResult, error)

	// ChatStream opens a streaming completion.
	ChatStream(ctx context.Context, cred Credential, req *ChatRequest) (*Stream, error)

	// Checkin attempts the daily check-in. Providers without one return
	// ErrUnsupported so the scheduler can skip them cleanly.
	Checkin(ctx context.Context, cred Credential) (CheckinResult, error)

	// Refresh renews an expiring credential, returning the updated blob.
	// Providers without refresh support return ErrUnsupported.
	Refresh(ctx context.Context, cred Credential) (Credential, error)
}

// RefreshPolicy 允许渠道在首次使用或令牌临近过期时声明刷新需求。
type RefreshPolicy interface {
	NeedsRefresh(Credential) bool
}

// Failure is an error that carries a machine-readable failure kind.
//
// Adapters classify their own protocol errors because they know their own
// codes; the executor then maps the kind onto cooldown policy without parsing
// upstream prose. KindHint uses the pool's Kind* constants.
type Failure struct {
	// Status is the HTTP status to report to the client.
	Status int
	// KindHint is the pool failure kind, e.g. "quota".
	KindHint string
	// Code is the upstream's own error code, kept for diagnostics.
	Code string
	// Type is the upstream's error type.
	Type string
	// Message is the human-readable text.
	Message string
	// RetryAfter is an optional hint, already parsed.
	RetryAfter time.Duration
	// NoRetry 表示上游任务已经受理或结果不确定，禁止自动换号重复创建任务。
	NoRetry bool
	// Body is the raw upstream body, capped, for the classifier to inspect
	// when the adapter's own hint is not enough.
	Body string
	// Err is the wrapped cause, if any.
	Err error
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	msg := f.Message
	if msg == "" && f.Err != nil {
		msg = f.Err.Error()
	}
	if msg == "" {
		msg = "upstream error"
	}
	if f.Code != "" {
		return fmt.Sprintf("%s (code %s)", msg, f.Code)
	}
	return msg
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// AsFailure extracts a *Failure from an error chain, if present.
func AsFailure(err error) (*Failure, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// NewFailure builds a Failure with a cause.
func NewFailure(status int, kindHint, code, message string, cause error) *Failure {
	return &Failure{
		Status:   status,
		KindHint: kindHint,
		Code:     code,
		Message:  message,
		Err:      cause,
	}
}

// Registry holds the registered provider families.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds a provider. Registering the same family twice overwrites, which
// keeps test setup simple.
func (r *Registry) Register(p Provider) {
	if r.providers == nil {
		r.providers = map[string]Provider{}
	}
	r.providers[strings.ToLower(strings.TrimSpace(p.Family()))] = p
}

// Lookup finds a provider by family name.
func (r *Registry) Lookup(family string) (Provider, error) {
	if r == nil {
		return nil, ErrUnknownProvider
	}
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(family))]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, family)
	}
	return p, nil
}

// Families lists the registered family names, sorted for stable output.
func (r *Registry) Families() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	// Insertion sort keeps this dependency-free; the list is tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Has reports whether a family is registered.
func (r *Registry) Has(family string) bool {
	if r == nil {
		return false
	}
	_, ok := r.providers[strings.ToLower(strings.TrimSpace(family))]
	return ok
}
