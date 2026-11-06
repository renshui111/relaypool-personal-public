package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHuaweiChannelRoutingAcrossProtocols(t *testing.T) {
	for _, family := range []string{"codearts", "officeace"} {
		t.Run(family, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if family == "officeace" {
					if key, secret, ok := r.BasicAuth(); !ok || key != "mock-app" || secret != "mock-secret" {
						t.Error("office gateway auth missing")
					}
				} else {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
						t.Error("CodeArts signature missing")
					}
				}
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request["model"] != "test-model" {
					t.Error("routing model lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"test\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			s := newTestServerNoAdapter(t)
			credential := map[string]any{"access_key": "mock-ak", "secret_key": "mock-sk", "credential_kind": "aksk", "base_url": upstream.URL + "/api/v2", "models": []string{"test-model"}}
			if family == "officeace" {
				credential = map[string]any{"model_app_key": "mock-app", "model_app_secret": "mock-secret", "model_base_url": upstream.URL, "models": []string{"test-model"}}
			}
			data := map[string]any{"name": "Huawei synthetic", "provider": family, "region": "cn", "auth_type": "oauth", "enabled": true, "credential": string(mustJSON(credential)), "drop_system_prompt": false}
			created := callComplete(t, s, "POST", "/admin/accounts", string(mustJSON(data)))
			if created.Code != 201 {
				t.Fatal(created.Body.String())
			}
			for _, stream := range []bool{false, true} {
				for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
					body := map[string]any{"model": family + "/test-model", "stream": stream, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
					if path == "/v1/responses" {
						delete(body, "messages")
						delete(body, "max_tokens")
						body["input"] = "hi"
						body["max_output_tokens"] = 16
					}
					result := callComplete(t, s, "POST", path, string(mustJSON(body)))
					if result.Code != 200 || !strings.Contains(result.Body.String(), "hello") {
						t.Errorf("%s stream=%v: %d %s", path, stream, result.Code, result.Body.String())
					}
				}
			}
			list := callComplete(t, s, "GET", "/admin/providers", "")
			if !strings.Contains(list.Body.String(), `"id":"`+family+`"`) {
				t.Fatal("Huawei provider absent")
			}
			if !s.quotaSupported(family) {
				t.Fatal("Huawei quota capability absent")
			}
		})
	}
}

func TestCodeArtsBrowserLoginLifecycleWithoutExternalRequests(t *testing.T) {
	s := newTestServerNoAdapter(t)
	started := callComplete(t, s, "POST", "/admin/accounts/login/start", `{"provider":"codearts","region":"cn","name":"synthetic-browser"}`)
	if started.Code != 201 {
		t.Fatal(started.Body.String())
	}
	var flow map[string]any
	_ = json.Unmarshal(started.Body.Bytes(), &flow)
	id, _ := flow["login_id"].(string)
	if id == "" || flow["mode"] != "callback" || strings.Contains(started.Body.String(), "dpop_private") || strings.Contains(started.Body.String(), "code_verifier") {
		t.Fatal("invalid browser response or private login data leaked")
	}
	pending := callComplete(t, s, "GET", "/admin/accounts/login/"+id, "")
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), `"pending"`) {
		t.Fatal("expected local pending flow")
	}
	complete := callComplete(t, s, "POST", "/admin/accounts/login/"+id+"/complete", `{"callback_url":"http://127.0.0.1:12800/oauth/callback?secret=x&redirect=https%3A%2F%2Fauth.huaweicloud.com%2Fauthui%2Flogin.html"}`)
	if complete.Code != 200 || !strings.Contains(complete.Body.String(), `"continue_url"`) {
		t.Fatal("portal continuation unavailable")
	}
	invalid := callComplete(t, s, "POST", "/admin/accounts/login/"+id+"/complete", `{"callback_url":"http://127.0.0.1:12800/oauth/callback?code=x&ticket_id=wrong"}`)
	if invalid.Code != 400 {
		t.Fatal("callback ticket mismatch accepted")
	}
	cancelled := callComplete(t, s, "POST", "/admin/accounts/login/"+id+"/cancel", `{}`)
	if cancelled.Code != 204 {
		t.Fatal("cancel failed")
	}
	if _, err := s.store.GetAccount(t.Context(), id); err == nil {
		t.Fatal("cancel left an incomplete account")
	}
}
