package subscriptions

import (
	"context"
	"github.com/relaypool/relaypool/internal/provider"
	"io"
	"net/http"
	"strings"
	"testing"
)

type checkinTransport func(*http.Request) (*http.Response, error)

func (f checkinTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func checkinResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func traeCheckinCredential() provider.Credential {
	return provider.Credential{Provider: "trae", Region: provider.RegionCN, Fields: map[string]string{"access_token": "test-token", "device_id": "123456", "ide_version": "3.3.62"}}
}
func TestTraeCheckinMustVerifyAfterClaim(t *testing.T) {
	for _, scenario := range []string{"success", "device_rejected", "no_state_change", "missing_status", "ineligible", "already"} {
		t.Run(scenario, func(t *testing.T) {
			calls, claims := 0, 0
			client := NewTrae(Config{})
			client.http.Transport = checkinTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.trae.cn" || r.Header.Get("Authorization") != "Cloud-IDE-JWT test-token" || r.Header.Get("X-Device-Id") != "123456" {
					t.Fatal("签到域、认证或设备编号错误")
				}
				if strings.HasSuffix(r.URL.Path, "/claim") {
					claims++
					if scenario == "device_rejected" {
						return checkinResponse(`{"code":9074,"message":"test-token"}`), nil
					}
					return checkinResponse(`{"code":0}`), nil
				}
				if scenario == "missing_status" {
					return checkinResponse(`{}`), nil
				}
				if scenario == "ineligible" {
					return checkinResponse(`{"checked_in":false,"enable":false}`), nil
				}
				if scenario == "already" || scenario == "success" && calls == 3 {
					return checkinResponse(`{"checked_in":true,"enable":true}`), nil
				}
				return checkinResponse(`{"checked_in":false,"enable":true}`), nil
			})
			result, err := client.Checkin(context.Background(), traeCheckinCredential())
			switch scenario {
			case "success":
				if err != nil || result.Status != provider.CheckinSuccess || calls != 3 {
					t.Fatal("未完成查询、领取和复核", err)
				}
			case "ineligible":
				if err != nil || result.Status != provider.CheckinSkipped || claims != 0 {
					t.Fatal("无资格仍领取")
				}
			case "already":
				if err != nil || result.Status != provider.CheckinAlready || claims != 0 {
					t.Fatal("重复领取")
				}
			default:
				if err == nil {
					t.Fatal("错误地把 HTTP 200 当成发奖成功")
				}
				if strings.Contains(err.Error(), "test-token") {
					t.Fatal("泄露令牌")
				}
			}
		})
	}
}

func TestQoderCampaignClaimabilityAndExplicitReward(t *testing.T) {
	for _, scenario := range []string{"success", "replayed", "no_campaign", "business_failure", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			claims := 0
			c := NewQoder(Config{})
			c.http.Transport = checkinTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Cosy-MachineToken") != "" {
					t.Fatal("未导入设备令牌却伪造设备身份")
				}
				if r.Method == "GET" {
					if scenario == "malformed" {
						return checkinResponse(`{}`), nil
					}
					if scenario == "no_campaign" {
						return checkinResponse(`{"campaigns":[]}`), nil
					}
					return checkinResponse(`{"campaigns":[{"campaignId":"advert","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS"}},{"campaignId":"future","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","startAt":9999999999,"benefit":{"kind":"CREDITS"}},{"campaignId":"credit-uuid","campaignKey":"act-20260923-556","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`), nil
				}
				claims++
				if !strings.HasSuffix(r.URL.Path, "/credit-uuid/claim") {
					t.Fatal("领取了不支持的活动")
				}
				if scenario == "business_failure" {
					return checkinResponse(`{"status":"REJECTED","errorMessage":"test-token"}`), nil
				}
				if scenario == "replayed" {
					return checkinResponse(`{"status":"CLAIMED","replayed":true}`), nil
				}
				return checkinResponse(`{"status":"CLAIMED","grantId":"test-grant"}`), nil
			})
			cred := provider.Credential{Provider: "qoder", Region: provider.RegionGlobal, Fields: map[string]string{"access_token": "test-token", "api_host": "https://openapi.qoder.sh", "machine_id": "original-id"}}
			r, err := c.Checkin(context.Background(), cred)
			switch scenario {
			case "success":
				if err != nil || r.Status != provider.CheckinSuccess || claims != 1 {
					t.Fatal("领取错误", err)
				}
			case "replayed":
				if err != nil || r.Status != provider.CheckinAlready {
					t.Fatal("重复发奖未区分")
				}
			case "no_campaign":
				if err != nil || r.Status != provider.CheckinSkipped || claims != 0 {
					t.Fatal("无活动仍领取")
				}
			default:
				if err == nil {
					t.Fatal("把未知或拒绝响应当成成功")
				}
				if strings.Contains(err.Error(), "test-token") {
					t.Fatal("泄露令牌")
				}
			}
		})
	}
}

func TestTraeIntlNeverCallsDomesticCheckin(t *testing.T) {
	c := NewTraeIntl(Config{})
	c.http.Transport = checkinTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("国际版调用了签到接口")
		return nil, nil
	})
	if _, err := c.Checkin(context.Background(), provider.Credential{Region: provider.RegionGlobal}); err != provider.ErrUnsupported {
		t.Fatal("未声明不支持")
	}
}
