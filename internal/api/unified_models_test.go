package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type exactModelProvider struct {
	*stubProvider
	want string
	seen []string
	fail bool
}

func (p *exactModelProvider) check(req *provider.ChatRequest) error {
	p.seen = append(p.seen, req.Model)
	if req.Model != p.want {
		return fmt.Errorf("wrong native model")
	}
	if p.fail {
		return &provider.Failure{Status: 429, KindHint: "rate_limit", Message: "busy"}
	}
	return nil
}
func (p *exactModelProvider) ChatNonStream(_ context.Context, _ provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	if err := p.check(req); err != nil {
		return nil, err
	}
	return &provider.ChatResult{Model: req.Model, Message: provider.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}, nil
}
func (p *exactModelProvider) ChatStream(_ context.Context, _ provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	if err := p.check(req); err != nil {
		return nil, err
	}
	return &provider.Stream{Model: req.Model, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
}

func unifiedFixture(t *testing.T) (*Server, *exactModelProvider, *exactModelProvider) {
	s := newTestServerNoAdapter(t)
	a := &exactModelProvider{stubProvider: &stubProvider{family: "workbuddy"}, want: "GLM-5-2"}
	b := &exactModelProvider{stubProvider: &stubProvider{family: "officeace"}, want: "glm.5.2"}
	for _, p := range []*exactModelProvider{a, b} {
		s.registry.Register(p)
		addAccount(t, s, p.family, p.family)
		s.pool.MergeModels(p.family, []string{p.want})
	}
	return s, a, b
}

func TestUnifiedModelsDeduplicateAndFilterVariants(t *testing.T) {
	s, _, _ := unifiedFixture(t)
	catalog := s.unifiedModelCatalog(store.DefaultKeyPermissions())
	if len(catalog) != 1 || catalog[0]["id"] != "glm-5.2" || len(catalog[0]["variants"].([]modelVariant)) != 2 {
		t.Fatalf("catalog not unified: %v", catalog)
	}
	policy := store.KeyPermissions{AllAccounts: true, Models: []string{"workbuddy/GLM_5_2"}}
	catalog = s.unifiedModelCatalog(policy)
	if len(catalog) != 1 || len(catalog[0]["variants"].([]modelVariant)) != 1 || catalog[0]["owned_by"] != "workbuddy" {
		t.Fatal("catalog exposed unauthorized provider")
	}
	s.pool.MergeModels("officeace", []string{"glm.5.2", "glm-5.2-harmony", "qwen3-32b", "auto"})
	catalog = s.unifiedModelCatalog(store.DefaultKeyPermissions())
	if len(catalog) != 4 {
		t.Fatal("merged model variants or omitted policy alias")
	}
}

func TestUnifiedModelRoutesNativeNamesAndFailsOver(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, protocol := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
			t.Run(protocol+fmt.Sprint(stream), func(t *testing.T) {
				s, a, b := unifiedFixture(t)
				a.fail = true
				body := fmt.Sprintf(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":%t}`, stream)
				if protocol == "/v1/responses" {
					body = fmt.Sprintf(`{"model":"glm-5.2","input":"hi","stream":%t}`, stream)
				}
				req := authRequest(t, "POST", protocol, body)
				req.Header.Set("X-RelayPool-Account", "workbuddy")
				out := httptest.NewRecorder()
				s.Handler().ServeHTTP(out, req)
				if out.Code != 200 || len(a.seen) != 1 || len(b.seen) != 1 || a.seen[0] != a.want || b.seen[0] != b.want {
					t.Fatalf("native failover failed: status %d seen %v %v", out.Code, a.seen, b.seen)
				}
			})
		}
	}
}

func TestUnifiedModelCannotBypassProviderPermissions(t *testing.T) {
	s, a, b := unifiedFixture(t)
	a.fail = true
	key, _, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "restricted", store.KeyScopeInference, store.KeyPermissions{AllAccounts: true, Models: []string{"workbuddy/GLM_5_2"}})
	if err != nil {
		t.Fatal(err)
	}
	out := permissionCall(t, s, key, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code == 200 || len(a.seen) != 1 || len(b.seen) != 0 {
		t.Fatal("unified failover escaped provider allowlist")
	}
	out = permissionCall(t, s, key, "POST", "/v1/chat/completions", `{"model":"officeace/glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code != 403 || len(b.seen) != 0 {
		t.Fatal("explicit provider escaped allowlist")
	}
}
