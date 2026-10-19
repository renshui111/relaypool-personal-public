package workbuddy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// setCommonHeaders installs the headers every request needs.
//
// Origin and Referer are derived from the credential's region and must never be
// mixed: a Global Origin against the CN host is rejected outright.
func setCommonHeaders(header http.Header, cred Credential) {
	origin := cred.origin()
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/plain, */*")
	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("Origin", origin)
	header.Set("Referer", origin+"/")
	header.Set("User-Agent", UserAgent)
}

// setIdentityHeaders carries the account identity.
//
// The X-No-* sentinels are deliberate. The upstream distinguishes "header
// absent" from "header present but empty" and rejects some requests in the
// former case, so when we genuinely lack a value we must say so explicitly
// rather than omit the header.
func setIdentityHeaders(header http.Header, cred Credential) {
	setCommonHeaders(header, cred)

	if cred.AccessToken != "" {
		header.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		header.Set("X-No-Authorization", "1")
	}
	if cred.UID != "" {
		header.Set("X-User-Id", cred.UID)
	} else {
		header.Set("X-No-User-Id", "1")
	}
	if cred.EnterpriseID != "" {
		header.Set("X-Enterprise-Id", cred.EnterpriseID)
	} else {
		header.Set("X-No-Enterprise-Id", "1")
	}
	if cred.Domain != "" {
		header.Set("X-Domain", cred.Domain)
	} else {
		header.Set("X-No-Department-Info", "1")
	}
	header.Set("X-Product", productName)
}

// setCLIChannelHeaders installs the chat-only CLI channel headers.
//
// These are ONLY sent on the chat path, never on the catalogue path. The
// upstream uses them to attribute usage to a CLI client, and sending them
// elsewhere is at best ignored and at worst rejected.
//
// All five ID headers carry the SAME value. They are distinct correlation slots
// that a single CLI invocation fills identically, and the upstream appears to
// cross-check that they agree within one request.
func setCLIChannelHeaders(header http.Header) {
	requestID := newRequestID()
	header.Set("X-Request-ID", requestID)
	header.Set("X-Conversation-ID", requestID)
	header.Set("X-Session-ID", requestID)
	header.Set("X-Conversation-Request-ID", requestID)
	header.Set("X-Conversation-Message-ID", requestID)

	header.Set("X-Agent-Type", agentTypeMain)
	header.Set("X-Agent-Intent", agentIntentDefault)
	header.Set("X-IDE-Type", productTypeCLI)
	header.Set("X-IDE-Name", productTypeCLI)
	header.Set("X-IDE-Version", CLIVersion)
	header.Set("X-Product-Version", CLIVersion)
	// Private data must stay off: enabling it would let upstream training
	// capture the caller's prompts.
	header.Set("X-Private-Data", "false")
}

// SetCatalogHeaders installs headers for the model catalogue endpoint.
func SetCatalogHeaders(header http.Header, cred Credential) {
	setIdentityHeaders(header, cred)
}

// SetChatHeaders installs headers for the chat endpoint.
//
// Note what is absent: the refresh token. Only SetRefreshHeaders may carry it,
// which keeps the long-lived credential off the highest-traffic path.
func SetChatHeaders(header http.Header, cred Credential) {
	setIdentityHeaders(header, cred)
	setCLIChannelHeaders(header)
}

// SetRefreshHeaders installs headers for the token refresh endpoint.
//
// This is the ONLY path allowed to carry X-Refresh-Token.
func SetRefreshHeaders(header http.Header, cred Credential) {
	setCommonHeaders(header, cred)
	header.Set("X-Refresh-Token", cred.RefreshToken)
	if cred.EnterpriseID != "" {
		header.Set("X-Enterprise-Id", cred.EnterpriseID)
	}
	header.Set("X-Auth-Refresh-Source", "workbuddy")
}

// SetBillingHeaders installs headers for the billing/check-in hosts.
//
// The billing host additionally wants X-Tenant-Id, which the chat host does not.
func SetBillingHeaders(header http.Header, cred Credential) {
	setCommonHeaders(header, cred)
	if cred.AccessToken != "" {
		header.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	if cred.UID != "" {
		header.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID != "" {
		header.Set("X-Enterprise-Id", cred.EnterpriseID)
		header.Set("X-Tenant-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		header.Set("X-Domain", cred.Domain)
	}
}

// newRequestID returns a random 128-bit hex correlation ID.
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A predictable ID is worse than none for correlation but harmless for
		// correctness, so fall back rather than failing the request.
		return "relaypool-fallback"
	}
	return hex.EncodeToString(raw[:])
}
