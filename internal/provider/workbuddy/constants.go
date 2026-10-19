// Package workbuddy implements the WorkBuddy / CodeBuddy in-process adapter.
//
// Protocol constants live ONLY in this package. Every value here was taken from
// a deployment that is demonstrably working (5268 successful completions), so
// the comments record WHY each constant has the shape it does rather than
// guessing at the upstream's intent.
package workbuddy

import "time"

// Family is the provider family name.
const Family = "workbuddy"

// CredentialFormat identifies the stored blob schema. It is versioned because
// the export shape has already changed once upstream.
const CredentialFormat = "workbuddy-oauth-v1"

// Domains per region. These decide which deployment a credential talks to.
const (
	DomainCN     = "codebuddy.cn"
	DomainGlobal = "workbuddy.ai"
)

// Hosts per region.
//
// CN chat goes to copilot.tencent.com while Global uses its own apex domain;
// the billing host differs again for CN. These are NOT interchangeable.
const (
	ChatBaseCN     = "https://copilot.tencent.com"
	ChatBaseGlobal = "https://www.workbuddy.ai"
	BillingBaseCN  = "https://www.codebuddy.cn"
)

// Client identity.
//
// CLIVersion must match a real released @tencent-ai/codebuddy-code build: the
// upstream validates the pairing of User-Agent with the IDE version headers.
// The dual-token form ("CLI/x CodeBuddy/x") is what WorkBuddy chat accepts.
const (
	CLIVersion = "2.139.0"
	UserAgent  = "CLI/" + CLIVersion + " CodeBuddy/" + CLIVersion
)

// Product/agent identifiers sent on the chat channel.
const (
	productTypeCLI     = "CLI"
	agentIntentDefault = "craft"
	agentTypeMain      = "main"
	productName        = "SaaS"
)

// Upstream paths.
const (
	pathAuthState    = "/v2/plugin/auth/state"
	pathAuthToken    = "/v2/plugin/auth/token"
	pathAuthAccount  = "/v2/plugin/login/account"
	pathTokenRefresh = "/v2/plugin/auth/token/refresh"
	pathChat         = "/v2/chat/completions"

	// The catalogue path DIFFERS by region, and this is a real trap.
	//
	// CN still serves the console catalogue to a Bearer token. For Global that
	// same path is an OIDC page: unauthenticated it 302s to Keycloak, and
	// authenticated it returns a 500 HTML body. Global must use the plugin JSON
	// catalogue instead.
	pathModelsCN     = "/console/enterprises/personal/models"
	pathModelsGlobal = "/v2/enterprises/personal/models"

	pathUserResource = "/v2/billing/meter/get-user-resource"
	pathDailyCheckin = "/v2/billing/meter/daily-checkin"
)

// Upstream error codes that need special handling.
const (
	// codeSessionDead means the login session is gone and the credential must
	// be refreshed or the account re-authenticated.
	codeSessionDead = 12153
	// textSessionDead accompanies codeSessionDead when the code is omitted.
	textSessionDead = "Offline user session not found"

	// 国际接口要求首条为系统消息，补充时还必须符合非空正文要求。
	codeMissingSystemPrompt = 11128
	textMissingSystemPrompt = "first message is not system prompt"
	// 实号 Kimi 请求确认：空正文属于请求错误，不应冷却正常账号。
	codeEmptyMessage = 11151
	textEmptyMessage = "a message has empty content"

	// codeRateLimit marks a usage limit whose response carries an ABSOLUTE
	// reset timestamp. Cooling for the generic fallback would retry seconds
	// later and burn more of an allowance that is already spent.
	codeRateLimit = 6004
)

// Timeouts.
const (
	// catalogTimeout bounds the catalogue fetch. It is shorter than a chat
	// timeout because a catalogue is a background refresh: giving up keeps the
	// account usable with an unknown catalogue, which fails open.
	catalogTimeout = 15 * time.Second
	// checkinTimeout bounds a check-in attempt.
	checkinTimeout = 20 * time.Second
	// refreshTimeout bounds a token refresh.
	refreshTimeout = 20 * time.Second
	// maxErrorBody caps how much of an upstream error body is read into memory
	// and passed to the classifier.
	maxErrorBody = 64 << 10
)

// quotaResetLocation is the fixed zone WorkBuddy uses when its CN usage-limit
// message says the limit resets. FixedZone cannot be a const because it returns
// a *time.Location.
var quotaResetLocation = time.FixedZone("UTC+8", 8*60*60)
