package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type inspectionProvider struct {
	*stubProvider
	calls   atomic.Int32
	fail    bool
	catalog func(context.Context, provider.Credential) ([]provider.ModelInfo, error)
	quota   func(context.Context, provider.Credential) (provider.QuotaSnapshot, error)
}

func (*inspectionProvider) SupportsQuota() bool { return true }
func (p *inspectionProvider) FetchQuota(ctx context.Context, cred provider.Credential) (provider.QuotaSnapshot, error) {
	if p.quota != nil {
		return p.quota(ctx, cred)
	}
	p.calls.Add(1)
	time.Sleep(10 * time.Millisecond)
	if p.fail {
		return provider.QuotaSnapshot{}, errors.New("synthetic-secret")
	}
	zero := 0.0
	return provider.QuotaSnapshot{State: "ok", Amounts: []provider.QuotaAmount{{Label: "测试积分", Unit: "积分", Remaining: &zero}}}, nil
}
func (p *inspectionProvider) FetchCatalog(ctx context.Context, c provider.Credential) ([]provider.ModelInfo, error) {
	return p.catalog(ctx, c)
}

func TestAccountQuotaCachingPermissionAndCredentialIsolation(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	p := &inspectionProvider{stubProvider: newStubProvider()}
	s.registry.Register(p)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
			if r.Code != 200 || !strings.Contains(r.Body.String(), `"remaining":0`) {
				t.Error("真实零值丢失")
			}
		}()
	}
	wg.Wait()
	if p.calls.Load() != 1 {
		t.Fatal("同账号并发查询没有合并")
	}
	callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota?refresh=1", "")
	if p.calls.Load() != 1 {
		t.Fatal("重复点击绕过了最小间隔")
	}
	record, err := s.store.GetCredential(t.Context(), "acc_test")
	if err != nil {
		t.Fatal(err)
	}
	record.Payload = []byte(`{"access_token":"replacement"}`)
	if err = s.store.UpsertCredential(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
	if p.calls.Load() != 2 {
		t.Fatal("凭证更新仍复用旧余额")
	}
	s.inspectionMu.Lock()
	s.quotaCache["acc_test"].expires = time.Now().Add(-time.Second)
	s.inspectionMu.Unlock()
	p.fail = true
	r := callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"error"`) || strings.Contains(r.Body.String(), "synthetic-secret") || strings.Contains(r.Body.String(), `"remaining":0`) {
		t.Fatal("错误伪装为零余额或泄露上游信息")
	}
	for _, path := range []string{"/admin/accounts/acc_test/inspect/quota", "/admin/accounts/acc_test/inspect/catalog"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != 401 {
			t.Fatal("匿名可查询账号")
		}
		key, _, err := s.store.CreateAPIKeyWithScope(t.Context(), "仅调用", store.KeyScopeInference)
		if err != nil {
			t.Fatal(err)
		}
		req = authRequest(t, "GET", path, "")
		req.Header.Set("Authorization", "Bearer "+key)
		out = httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != 403 {
			t.Fatal("调用密钥可查询管理数据")
		}
	}
}

func TestAccountCatalogBypassesConfiguredModelsAndPreservesFailedDirectory(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	p := &inspectionProvider{stubProvider: newStubProvider()}
	p.catalog = func(_ context.Context, c provider.Credential) ([]provider.ModelInfo, error) {
		if c.Field("models") != "[]" {
			t.Fatal("没有绕过手填目录")
		}
		return []provider.ModelInfo{{NativeModel: "upstream-model"}, {NativeModel: "upstream-model"}}, nil
	}
	s.registry.Register(p)
	r := callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/catalog", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"source":"upstream"`) {
		t.Fatal("上游目录来源未确认")
	}
	var result struct{ Models []string }
	json.Unmarshal(r.Body.Bytes(), &result)
	if len(result.Models) != 1 || result.Models[0] != "upstream-model" {
		t.Fatal("目录未去重")
	}
	p.catalog = func(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
		return nil, errors.New("synthetic-secret")
	}
	r = callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/catalog", "")
	if !strings.Contains(r.Body.String(), `"state":"error"`) || strings.Contains(r.Body.String(), "synthetic-secret") {
		t.Fatal("目录失败处理错误")
	}
	r = callComplete(t, s, "GET", "/admin/accounts/acc_test", "")
	if !strings.Contains(r.Body.String(), "upstream-model") {
		t.Fatal("查询失败删除旧型号")
	}
}

func TestAccountInspectionUnsupportedMissingAndDisabled(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	r := callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
	if !strings.Contains(r.Body.String(), `"state":"unsupported"`) {
		t.Fatal("不支持余额的渠道假报零")
	}
	p := &inspectionProvider{stubProvider: newStubProvider()}
	s.registry.Register(p)
	if callComplete(t, s, "POST", "/admin/accounts/acc_test/enabled", `{"enabled":false}`).Code != http.StatusOK {
		t.Fatal("未成功停用测试账号")
	}
	callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
	a, _ := s.store.GetAccount(t.Context(), "acc_test")
	if a.Enabled || p.calls.Load() != 1 {
		t.Fatal("停用账号未查询或被自动启用")
	}
	if err := s.store.DeleteCredential(t.Context(), "acc_test"); err != nil {
		t.Fatal(err)
	}
	r = callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/quota", "")
	if !strings.Contains(r.Body.String(), `"state":"unknown"`) {
		t.Fatal("缺凭证假报零")
	}
	if callComplete(t, s, "GET", "/admin/accounts/missing/inspect/quota", "").Code != 404 {
		t.Fatal("不存在的账号未返回404")
	}
}

// 刷新期间更换凭证，旧账号的余额和型号均不能写回新凭证的界面或号池。
func TestAccountInspectionRejectsChangedCredentialInFlight(t *testing.T) {
	for _, kind := range []string{"quota", "catalog"} {
		t.Run(kind, func(t *testing.T) {
			_, s := newTestServer(t, newStubProvider())
			p := &inspectionProvider{stubProvider: newStubProvider()}
			started, released := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(released) }) })
			p.quota = func(ctx context.Context, _ provider.Credential) (provider.QuotaSnapshot, error) {
				close(started)
				select {
				case <-released:
				case <-ctx.Done():
					return provider.QuotaSnapshot{}, ctx.Err()
				}
				value := 99.0
				return provider.QuotaSnapshot{State: "ok", Amounts: []provider.QuotaAmount{{Remaining: &value}}}, nil
			}
			p.catalog = func(ctx context.Context, _ provider.Credential) ([]provider.ModelInfo, error) {
				close(started)
				select {
				case <-released:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return []provider.ModelInfo{{NativeModel: "old-credential-model"}}, nil
			}
			s.registry.Register(p)
			result := make(chan string, 1)
			go func() {
				result <- callComplete(t, s, "GET", "/admin/accounts/acc_test/inspect/"+kind, "").Body.String()
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("查询未开始")
			}
			record, err := s.store.GetCredential(t.Context(), "acc_test")
			if err != nil {
				t.Fatal(err)
			}
			record.Payload = []byte(`{"access_token":"changed"}`)
			if err = s.store.UpsertCredential(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			releaseOnce.Do(func() { close(released) })
			select {
			case body := <-result:
				if !strings.Contains(body, `"state":"unknown"`) || strings.Contains(body, "old-credential-model") || strings.Contains(body, `"remaining":99`) {
					t.Fatal("旧凭证结果写回新账号")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("查询未结束")
			}
			if strings.Contains(callComplete(t, s, "GET", "/admin/accounts/acc_test", "").Body.String(), "old-credential-model") {
				t.Fatal("旧目录写入号池")
			}
		})
	}
}
