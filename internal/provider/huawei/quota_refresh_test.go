package huawei

import (
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func TestQuotaRefreshUsesSeparateOfficeSTSExpiry(t *testing.T) {
	for _, family := range []string{CodeArts, OfficeAce} {
		c := adapter(family)
		cred := provider.Credential{Fields: map[string]string{"refresh_token": "test", "dpop_private_jwk": "test", "sts_expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
		if family == CodeArts {
			cred.ExpiresAt = time.Now().Add(-time.Hour)
		}
		if !c.NeedsQuotaRefresh(cred) {
			t.Fatal("expired quota credential not refreshed")
		}
		if family == OfficeAce && c.NeedsRefresh(cred) {
			t.Fatal("Office chat incorrectly uses STS expiry")
		}
		cred.ExpiresAt = time.Now().Add(time.Hour)
		cred.Fields["sts_expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		if c.NeedsQuotaRefresh(cred) {
			t.Fatal("fresh quota token rotated")
		}
		cred.ExpiresAt = time.Now().Add(-time.Hour)
		cred.Fields["sts_expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
		delete(cred.Fields, "refresh_token")
		if c.NeedsQuotaRefresh(cred) {
			t.Fatal("attempted refresh without refresh material")
		}
	}
}
