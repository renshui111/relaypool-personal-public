package workbuddy

import "net/http"

// DesktopIdentity593 is the opt-in header profile supplied for compatibility
// testing. It does not launch the official app, change TLS, or promise any
// particular upstream client classification. Endpoints remain unchanged.
const DesktopIdentity593 = "desktop-5.9.3"

// applyIdentityProfile runs after the legacy UA override so an explicitly
// selected desktop profile cannot accidentally retain a conflicting CLI UA.
// Only CN chat/catalog calls use it; login, refresh and billing retain their
// separate existing protocol until those endpoints have independent evidence.
func (c *Client) applyIdentityProfile(h http.Header, cred Credential) {
	if c.cfg.IdentityProfile != DesktopIdentity593 || cred.IsGlobal() {
		return
	}
	h.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.161.0")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Version", "5.9.3")
	h.Set("X-Product-Version", "5.9.3")
	h.Set("X-Domain", "workbuddy.cn")
	h.Del("X-No-Department-Info")
	h.Set("Origin", "https://www.workbuddy.cn")
	h.Set("Referer", "https://www.workbuddy.cn/")
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("X-Agent-Type", "main")
	h.Set("X-Agent-Runtime", "local-agent")
}
