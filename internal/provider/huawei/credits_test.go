package huawei

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func officeCreditCredential(t *testing.T, c *Client, base string) provider.Credential {
	t.Helper()
	data := map[string]any{"model_base_url": base, "model_app_key": "app", "model_app_secret": "secret", "access_key": "test-ak", "secret_key": "test-sk", "security_token": "test-sts", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
	raw, _ := json.Marshal(data)
	cred, err := c.DecodeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestV11SigningGolden(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v1/subscription/bonus/claim", nil)
	r.Header.Set("Content-Type", "application/json;charset=utf8")
	r.Header.Set("X-Sdk-Date", "20261106T040000Z")
	r.Header.Set("X-Subscription-Type", "v2")
	cred := provider.Credential{Fields: map[string]string{"access_key": "test-ak", "secret_key": "test-sk", "security_token": "test-sts"}}
	if err := signV11(r, []byte(`{}`), cred); err != nil {
		t.Fatal(err)
	}
	// Independently computed with Node's crypto HMAC primitives.
	if !strings.HasSuffix(r.Header.Get("Authorization"), "Signature=c603739254c264154a3b150f3f598e3ff88d33b9697a6d27d3e157c2a1469b49") {
		t.Fatal("V11 signature differs from independent vector")
	}
}

func TestCodeArtsCreditNumbers(t *testing.T) {
	for _, tt := range []struct {
		fields    string
		state     string
		remaining float64
	}{
		{`"package_credit_amount":1000,"package_credit_used":200,"package_credit_remain":800`, "ok", 800},
		{`"package_credit_amount":"1000.5","package_credit_used":"200"`, "ok", 800.5},
		{`"package_credit_remain":0`, "ok", 0},
		{`"package_credit_remain":null`, "unknown", 0},
		{`"package_credit_used":100`, "unknown", 0},
		{`"package_credit_remain":"NaN"`, "unknown", 0},
		{`"package_credit_remain":-1`, "unknown", 0},
	} {
		t.Run(tt.fields, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/snap-manager/v1/statistics/plugin" || !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
					t.Error("incorrect quota request")
				}
				fmt.Fprintf(w, `{"metrics":[{"name":"usageTotalPackageCredit",%s}]}`, tt.fields)
			}))
			defer srv.Close()
			c := adapter(CodeArts)
			c.snap = srv.URL
			q, err := c.FetchQuota(t.Context(), credentials(t, c, srv.URL))
			if err != nil || q.State != tt.state {
				t.Fatalf("%+v %v", q, err)
			}
			if tt.state == "ok" && (len(q.Amounts) != 1 || q.Amounts[0].Remaining == nil || *q.Amounts[0].Remaining != tt.remaining) {
				t.Fatalf("bad balance %+v", q)
			}
			if tt.state != "ok" && len(q.Amounts) != 0 {
				t.Fatal("unknown displayed as a number")
			}
		})
	}
}

func TestOfficeCreditExpiryAndMissingFields(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, body, state string
		balance           float64
	}{
		{"active and expired", `{"skus":[{"quotas":[{"sku_attr_code":"officeace_points","sku_value":"1000","current_value":100},{"sku_attr_code":"online_search_count","sku_value":5000,"current_value":0}]}],"bonus_skus":[{"points":200,"current_value":50,"expired_time":"2026-11-06T23:59:59+08:00"},{"points":9999,"current_value":0,"expired_time":"2026-11-06T00:00:00+08:00"},{"points":8888,"current_value":0,"effective_time":"2027-01-01T00:00:00Z"}]}`, "ok", 1050},
		{"zero", `{"skus":[],"bonus_skus":[{"points":200,"current_value":200}]}`, "ok", 0},
		{"missing usage", `{"skus":[],"bonus_skus":[{"points":200}]}`, "unknown", 0},
		{"missing arrays", `{}`, "unknown", 0},
		{"unlimited", `{"skus":[{"quotas":[{"sku_attr_code":"officeace_points","sku_value":-1,"current_value":0}]}],"bonus_skus":[]}`, "unknown", 0},
		{"invalid expiry", `{"skus":[],"bonus_skus":[{"points":100,"current_value":0,"expired_time":"invalid"}]}`, "unknown", 0},
		{"frozen", `{"subscribe_status":"FROZEN","skus":[],"bonus_skus":[{"points":100,"current_value":0}]}`, "unknown", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sub officeSubscription
			if err := json.Unmarshal([]byte(tt.body), &sub); err != nil {
				t.Fatal(err)
			}
			q := sub.snapshot(now)
			if q.State != tt.state {
				t.Fatalf("%+v", q)
			}
			if tt.state == "ok" && *q.Amounts[0].Remaining != tt.balance {
				t.Fatalf("unexpected balance: %+v", q.Amounts)
			}
		})
	}
}

func TestCodeArtsDailyClaimVerification(t *testing.T) {
	for _, mode := range []string{"success", "already", "none", "business-error", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			posts := 0
			keys := []string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Agent-Type") != "PromptCenter" {
					t.Error("missing welfare header")
				}
				if r.Method == "GET" {
					if r.URL.RequestURI() != "/v1/ops/delivery?channel=IDE" {
						t.Error("bad delivery path")
					}
					status, eligible := "ELIGIBLE", true
					if mode == "already" || (posts > 0 && mode == "success") {
						status, eligible = "CLAIMED", false
					}
					if mode == "none" {
						eligible = false
					}
					fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","status":%q,"claimable":%t},{"campaignId":2,"type":"STUDENT_CERTIFIED","claimable":true},{"campaignId":3,"type":"INVITE_USER","claimable":true}]}}`, status, eligible)
					return
				}
				posts++
				var body struct {
					ID      int    `json:"campaignId"`
					Key     string `json:"idempotentKey"`
					Channel string `json:"channel"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path != "/v1/ops/claim" || body.ID != 1 || body.Channel != "IDE" || len(body.Key) != 36 {
					t.Error("incorrect claim")
				}
				keys = append(keys, body.Key)
				if mode == "business-error" {
					io.WriteString(w, `{"code":40001,"message":"test-ak"}`)
				} else {
					io.WriteString(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := adapter(CodeArts)
			c.snap = srv.URL
			cred := credentials(t, c, srv.URL)
			result, err := c.Checkin(t.Context(), cred)
			want := map[string]string{"success": provider.CheckinSuccess, "already": provider.CheckinAlready, "none": provider.CheckinSkipped, "unconfirmed": provider.CheckinSkipped}[mode]
			if mode == "business-error" {
				if err == nil || strings.Contains(err.Error(), "test-ak") {
					t.Fatal("business failure accepted or leaked")
				}
				return
			}
			if err != nil || result.Status != want {
				t.Fatalf("%+v %v", result, err)
			}
			if (mode == "already" || mode == "none") && posts != 0 {
				t.Fatal("ineligible reward claimed")
			}
			if mode == "unconfirmed" {
				_, _ = c.Checkin(t.Context(), cred)
				if len(keys) != 2 || keys[0] != keys[1] {
					t.Fatal("retry idempotency changed")
				}
			}
		})
	}
}

func TestOfficeClaimMustShowNewReward(t *testing.T) {
	effective := time.Now().Format(time.RFC3339)
	for _, mode := range []string{"new", "no-change", "business-error", "already"} {
		t.Run(mode, func(t *testing.T) {
			posts := 0
			gets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "V11-HMAC-SHA256 Credential=") || r.Header.Get("X-Subscription-Type") != "v2" || r.Header.Get("X-Security-Token") != "test-sts" {
					t.Error("incorrect subscription authentication")
				}
				if r.Method == "POST" {
					posts++
					raw, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/v1/subscription/bonus/claim" || string(raw) != "{}" || r.Header.Get("Content-Type") != "application/json;charset=utf8" {
						t.Error("incorrect claim request")
					}
					if mode == "business-error" {
						io.WriteString(w, `{"error_code":"denied","error_msg":"test-sts"}`)
					} else {
						io.WriteString(w, `{"bonus_skus":[]}`)
					}
					return
				}
				gets++
				if r.URL.Path != "/v1/subscription" {
					t.Error("bad subscription path")
				}
				if (mode == "new" && posts > 0) || mode == "already" {
					fmt.Fprintf(w, `{"skus":[],"bonus_skus":[{"activity_id":"daily","activity_name":"每日签到","points":150,"current_value":0,"effective_time":%q}]}`, effective)
				} else {
					io.WriteString(w, `{"skus":[],"bonus_skus":[]}`)
				}
			}))
			defer srv.Close()
			c := adapter(OfficeAce)
			cred := officeCreditCredential(t, c, srv.URL)
			result, err := c.Checkin(t.Context(), cred)
			if mode == "business-error" {
				if err == nil || gets != 1 || strings.Contains(err.Error(), "test-sts") {
					t.Fatal("business failure accepted or leaked")
				}
				return
			}
			want := map[string]string{"new": provider.CheckinSuccess, "no-change": provider.CheckinSkipped, "already": provider.CheckinAlready}[mode]
			if err != nil || result.Status != want || posts != 1 || gets != 2 {
				t.Fatalf("%+v %v posts=%d gets=%d", result, err, posts, gets)
			}
			if mode == "new" && !strings.Contains(result.Message, "150") {
				t.Fatal("reward amount missing")
			}
		})
	}
}

func TestCreditCredentialBoundaries(t *testing.T) {
	c := adapter(OfficeAce)
	cred := credentials(t, c, "https://example.com")
	q, err := c.FetchQuota(t.Context(), cred)
	if err != nil || q.State != "unknown" || len(q.Amounts) != 0 {
		t.Fatal("app credentials should not show a balance")
	}
	r, err := c.Checkin(t.Context(), cred)
	if err != nil || r.Status != provider.CheckinSkipped {
		t.Fatal("app credentials claimed a reward")
	}
	for _, base := range []string{"https://evil.example/v2", "https://modelgw.officeace.cn-southwest-2.huaweicloud-agentarts.com.evil.example/v2", "https://modelgw.officeace.cn-southwest-2.huaweicloud-agentarts.com:444/v2", "https://modelgw.officeace.cn-southwest-2.huaweicloud-agentarts.com/custom/v2"} {
		cred = officeCreditCredential(t, c, base)
		if _, err := officeCreditBase(cred); err == nil {
			t.Fatal("STS allowed to untrusted gateway")
		}
	}
	cred = officeCreditCredential(t, c, "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com")
	if _, err := officeCreditBase(cred); err != nil {
		t.Fatal(err)
	}
	cred.Fields["sts_expires_at"] = "2020-01-01T00:00:00Z"
	if c.creditAccess(cred) == "" || c.NeedsRefresh(cred) {
		t.Fatal("control expiry must not expire inference credentials")
	}
}

func TestCreditRedirectDoesNotForwardSecrets(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer srv.Close()
	for _, family := range []string{CodeArts, OfficeAce} {
		c := adapter(family)
		c.snap = srv.URL
		cred := credentials(t, c, srv.URL)
		if family == OfficeAce {
			cred = officeCreditCredential(t, c, srv.URL)
		}
		if _, err := c.FetchQuota(t.Context(), cred); err == nil {
			t.Fatal("redirect accepted")
		}
	}
	if leaked {
		t.Fatal("signed request followed redirect")
	}
}

func TestOfficeRefreshPreservesAppCredentials(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "pdp5_for_agentarts" {
			t.Error("wrong Office client ID")
		}
		io.WriteString(w, `{"credentials":{"access_key_id":"new-ak","secret_access_key":"new-sk","security_token":"new-sts","expiration":"2099-01-01T00:00:00Z"},"refresh_token":"new-refresh"}`)
	}))
	defer srv.Close()
	c := adapter(OfficeAce)
	c.token = srv.URL
	cred := officeCreditCredential(t, c, srv.URL)
	var data map[string]any
	_ = json.Unmarshal(cred.Raw, &data)
	jwk, _ := newJWK()
	data["dpop_private_jwk"] = json.RawMessage(jwk)
	data["refresh_token"] = "old-refresh"
	data["code_verifier"] = "verifier"
	raw, _ := json.Marshal(data)
	cred, _ = c.DecodeCredential(raw)
	updated, err := c.Refresh(t.Context(), cred)
	if err != nil || updated.Field("access_key") != "new-ak" || updated.Field("app_key") != "app" || updated.Field("app_secret") != "secret" || !updated.ExpiresAt.IsZero() || string(updated.PreviousRaw) != string(cred.Raw) {
		t.Fatalf("refresh did not preserve independent inference credentials: %v", err)
	}
	_, err = c.Refresh(t.Context(), updated)
	if err != nil || calls != 1 {
		t.Fatal("fresh control credential was rotated unnecessarily")
	}
}
