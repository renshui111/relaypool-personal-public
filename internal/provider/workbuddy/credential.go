package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// Credential is the decoded WorkBuddy login.
//
// The JSON tags define the canonical storage shape. Three input shapes are
// accepted on read because the upstream export format has changed over time and
// credentials captured earlier must keep working.
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is a Unix timestamp in SECONDS (not milliseconds): the
	// upstream sends seconds and treating it as millis puts the expiry in 1970,
	// which would force a refresh on every single request.
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// toProvider converts to the neutral credential form.
func (c Credential) toProvider(accountID string) provider.Credential {
	raw, _ := c.Encode()
	return provider.Credential{
		AccountID: accountID,
		Provider:  Family,
		Region:    c.Region(),
		Raw:       raw,
		Fields: map[string]string{
			"access_token":  c.AccessToken,
			"refresh_token": c.RefreshToken,
			"uid":           c.UID,
			"enterprise_id": c.EnterpriseID,
			"domain":        c.Domain,
			"nickname":      c.Nickname,
		},
		ExpiresAt: c.Expiry(),
	}
}

// Encode renders the canonical storage form.
func (c Credential) Encode() ([]byte, error) {
	return json.Marshal(c)
}

// Expiry converts the stored Unix-seconds field to a time.
func (c Credential) Expiry() time.Time {
	if c.ExpiresAt <= 0 {
		return time.Time{}
	}
	// Guard against a millisecond value having been stored by an older
	// version: anything beyond year 33658 in seconds is really millis.
	seconds := c.ExpiresAt
	if seconds > 1e12 {
		seconds /= 1000
	}
	return time.Unix(seconds, 0).UTC()
}

// Ready reports whether the credential can be used at all.
//
// A UID is required in addition to a token: the upstream rejects chat requests
// without X-User-Id, so a token-only credential would fail every request with
// an unhelpful error. Requiring both here makes an unusable account visibly
// "not ready" instead.
func (c Credential) Ready() bool {
	return strings.TrimSpace(c.AccessToken) != "" && strings.TrimSpace(c.UID) != ""
}

// IsGlobal reports whether this credential targets the international site.
func (c Credential) IsGlobal() bool { return c.Region() == provider.RegionGlobal }

// Region determines the deployment from the credential's domain.
//
// The comparison is on the PARSED HOSTNAME with an exact-or-suffix match, not a
// substring search. A substring check would classify "evil-codebuddy.cn.attacker
// .com" as CN, and would also mis-handle the several near-miss domains
// (codebuddy.ai vs workbuddy.ai) that belong to different deployments.
func (c Credential) Region() provider.Region {
	domain := strings.ToLower(strings.TrimSpace(c.Domain))
	if domain == "" {
		return provider.RegionCN
	}
	if !strings.Contains(domain, "://") {
		domain = "https://" + domain
	}
	parsed, err := url.Parse(domain)
	if err != nil {
		return provider.RegionCN
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if host == "" {
		return provider.RegionCN
	}
	for _, entry := range []struct {
		domain string
		region provider.Region
	}{
		{DomainCN, provider.RegionCN},
		{"workbuddy.cn", provider.RegionCN},
		{DomainGlobal, provider.RegionGlobal},
		{"codebuddy.ai", provider.RegionGlobal},
		{"workbuddy.com", provider.RegionGlobal},
	} {
		if host == entry.domain || strings.HasSuffix(host, "."+entry.domain) {
			return entry.region
		}
	}
	return provider.RegionCN
}

// ChatBase returns the chat host for this credential's region.
func (c Credential) ChatBase() string {
	if c.IsGlobal() {
		return ChatBaseGlobal
	}
	return ChatBaseCN
}

// BillingBase returns the billing host for this credential's region.
func (c Credential) BillingBase() string {
	if c.IsGlobal() {
		return ChatBaseGlobal
	}
	return BillingBaseCN
}

// catalogPath returns the model catalogue path, which differs by region.
func (c Credential) catalogPath() string {
	if c.IsGlobal() {
		return pathModelsGlobal
	}
	return pathModelsCN
}

// origin returns the Origin/Referer pair for this region.
//
// These must match the region exactly. Sending a Global Origin with a CN host is
// rejected, so this is derived from the credential rather than configured.
func (c Credential) origin() string {
	if c.IsGlobal() {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// ClientCredential exposes the decoded credential to callers that need to build
// requests using this package's header helpers.
//
// It exists because the header setters take the concrete Credential type (they
// read region, domain and token fields), while the Provider interface traffics
// in the neutral provider.Credential. A caller outside this package that builds
// its own request -- a diagnostic tool, or a future test harness -- needs a way
// across that boundary without duplicating the decode logic.
func ClientCredential(raw []byte) (Credential, error) {
	return DecodeCredential(raw)
}

// DecodeCredential parses a stored blob.
//
// Three shapes are accepted, in order of specificity:
//
//  1. the nested export form: {"account":{...},"auth":{...}}
//  2. the canonical flat snake_case form written by Encode
//  3. a flat camelCase form as the upstream's own API returns it
//
// Shape 3 matters because an operator pasting a raw upstream response should
// work without hand-editing keys.
func DecodeCredential(payload []byte) (Credential, error) {
	if len(payload) == 0 {
		return Credential{}, fmt.Errorf("workbuddy: credential is empty")
	}

	// Shape 1: nested export.
	var nested struct {
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(payload, &nested); err == nil {
		if nested.Auth.AccessToken != "" || nested.Account.UID != "" {
			return Credential{
				AccessToken:  nested.Auth.AccessToken,
				RefreshToken: nested.Auth.RefreshToken,
				ExpiresAt:    nested.Auth.ExpiresAt,
				Domain:       nested.Auth.Domain,
				UID:          nested.Account.UID,
				EnterpriseID: nested.Account.EnterpriseID,
				Nickname:     nested.Account.Nickname,
			}, nil
		}
	}

	// Shapes 2 and 3 share a field set, differing only in key style.
	var flat struct {
		AccessToken   string `json:"access_token"`
		RefreshToken  string `json:"refresh_token"`
		ExpiresAt     int64  `json:"expires_at"`
		Domain        string `json:"domain"`
		UID           string `json:"uid"`
		EnterpriseID  string `json:"enterprise_id"`
		Nickname      string `json:"nickname"`
		AccessTokenC  string `json:"accessToken"`
		RefreshTokenC string `json:"refreshToken"`
		ExpiresAtC    int64  `json:"expiresAt"`
		EnterpriseIDC string `json:"enterpriseId"`
	}
	if err := json.Unmarshal(payload, &flat); err != nil {
		return Credential{}, fmt.Errorf("workbuddy: credential is not valid JSON: %w", err)
	}

	cred := Credential{
		AccessToken:  firstNonEmpty(flat.AccessToken, flat.AccessTokenC),
		RefreshToken: firstNonEmpty(flat.RefreshToken, flat.RefreshTokenC),
		ExpiresAt:    firstNonZero(flat.ExpiresAt, flat.ExpiresAtC),
		Domain:       flat.Domain,
		UID:          flat.UID,
		EnterpriseID: firstNonEmpty(flat.EnterpriseID, flat.EnterpriseIDC),
		Nickname:     flat.Nickname,
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return Credential{}, fmt.Errorf("workbuddy: credential requires an access token")
	}
	return cred, nil
}

// ValidateCredential checks an import before it is stored.
//
// A token alone is accepted here (unlike Ready) so an operator can import and
// then diagnose a partially captured credential, rather than being silently
// refused with no way to see what arrived.
func ValidateCredential(payload []byte) error {
	_, err := DecodeCredential(payload)
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// isCLIAgent reports whether an agent name refers to a CLI client. Used to
// decide whether CLI-channel headers are appropriate.
func isCLIAgent(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cli", "codebuddy", "workbuddy":
		return true
	default:
		return false
	}
}
