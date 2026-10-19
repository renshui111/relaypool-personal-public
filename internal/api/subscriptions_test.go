package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

func subscriptionAccount(t *testing.T, s *Server, family, region, endpoint string) string {
	t.Helper()
	credential := map[string]any{"provider": family, "region": region, "access_token": "dt-old", "refresh_token": "drt-old", "uid": "synthetic-user", "models": []string{"actual-model"}, "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339), "base_url": endpoint, "api_host": endpoint, "machine_id": "original-machine", "device_id": "original-device"}
	if family == "trae" {
		credential["access_token"] = "trae-old"
		credential["refresh_token"] = "trae-old-refresh"
	}
	body := map[string]any{"name": "订阅测试", "provider": family, "region": region, "credential": string(mustJSON(credential))}
	result := callComplete(t, s, "POST", "/admin/accounts", string(mustJSON(body)))
	if result.Code != 201 {
		t.Fatal(result.Body.String())
	}
	var value map[string]any
	json.Unmarshal(result.Body.Bytes(), &value)
	return value["id"].(string)
}

func TestSubscriptionRequestRefreshUsesCASAndPreservesRegion(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		t.Run(family, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				if family == "qoder" {
					io.WriteString(w, `{"device_token":"dt-stale","refresh_token":"drt-stale","expires_in":3600000}`)
				} else {
					io.WriteString(w, `{"Result":{"Token":"trae-stale","RefreshToken":"trae-stale-refresh","TokenExpireDuration":3600}}`)
				}
			}))
			defer server.Close()
			region := "cn"
			if family == "qoder" {
				region = "global"
			}
			id := subscriptionAccount(t, s, family, region, server.URL)
			source := credentialSource{store: s.store, registry: s.registry}
			done := make(chan provider.Credential, 1)
			failures := make(chan error, 1)
			go func() { cred, err := source.CredentialForContext(t.Context(), id); done <- cred; failures <- err }()
			<-entered
			replacement := map[string]any{"provider": family, "region": region, "access_token": "dt-manual", "uid": "manual-user", "machine_id": "original-machine", "device_id": "original-device", "models": []string{"replacement-model"}}
			if err := s.store.UpsertCredential(context.Background(), store.Credential{AccountID: id, Format: "manual", Payload: mustJSON(replacement)}); err != nil {
				t.Fatal(err)
			}
			close(release)
			updated := <-done
			if err := <-failures; err != nil {
				t.Fatal(err)
			}
			if updated.AccountID != id || updated.Region != provider.Region(region) || updated.Field("access_token") != "dt-manual" {
				t.Fatal("请求使用了旧刷新结果或丢失地区")
			}
			record, _ := s.store.GetCredential(t.Context(), id)
			if strings.Contains(string(record.Payload), "stale") {
				t.Fatal("旧令牌覆盖用户新凭证")
			}
		})
	}
}

func TestSubscriptionImportRejectsForeignRegionWithoutCreatingAccount(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, test := range []struct {
		family     string
		credential map[string]any
		message    string
	}{
		{"qoder", map[string]any{"access_token": "dt-synthetic", "uid": "synthetic-user", "region": "global"}, "地区"},
		{"trae", map[string]any{"access_token": "synthetic", "uid": "synthetic-user"}, "原登录设备"},
	} {
		result := callComplete(t, s, "POST", "/admin/accounts", string(mustJSON(map[string]any{"provider": test.family, "region": "cn", "credential": string(mustJSON(test.credential))})))
		if result.Code != 400 || !strings.Contains(result.Body.String(), test.message) || strings.Contains(result.Body.String(), "dt-synthetic") {
			t.Fatal("导入错误提示不准确或回显凭证")
		}
	}
	listing := callComplete(t, s, "GET", "/admin/accounts", "")
	var value struct {
		Data []any `json:"data"`
	}
	json.Unmarshal(listing.Body.Bytes(), &value)
	if len(value.Data) != 0 {
		t.Fatal("失败导入留下可调度账号")
	}
}
