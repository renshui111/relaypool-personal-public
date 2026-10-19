package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

func quotaEnvelope(accounts string) string {
	return `{"code":0,"data":{"Response":{"Data":{"Accounts":` + accounts + `}}}}`
}

func TestQuotaResourceValuesAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		remaining          float64
		fail, unknownTotal bool
	}{
		{"周期和固定包合并", quotaEnvelope(`[{"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800,"CapacityRemain":9999},{"CapacitySize":500,"CapacityRemain":300,"CapacityUsed":200}]`), 1500, false, false},
		{"真实零余额", quotaEnvelope(`[{"CapacityRemain":0,"CapacitySize":100,"CapacityUsed":100}]`), 0, false, false},
		{"空积分包", quotaEnvelope(`[]`), 0, false, false},
		{"仅上报剩余", quotaEnvelope(`[{"CapacityRemain":12.5}]`), 12.5, false, true},
		{"总额减已用", quotaEnvelope(`[{"CapacitySize":100,"CapacityUsed":30}]`), 70, false, false},
		{"周期优先", quotaEnvelope(`[{"CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleCapacityUsed":100,"CapacityRemain":999}]`), 0, false, false},
		{"缺剩余", quotaEnvelope(`[{}]`), 0, true, false},
		{"缺数组", `{"code":0,"data":{}}`, 0, true, false},
		{"空值", quotaEnvelope(`null`), 0, true, false},
		{"错误业务码", `{"code":99,"message":"synthetic-secret"}`, 0, true, false},
		{"错误页面", `<html>synthetic-secret</html>`, 0, true, false},
		{"缺业务码", `{"data":{"Response":{"Data":{"Accounts":[]}}}}`, 0, true, false},
		{"超大计数", quotaEnvelope(`[{"CapacityRemain":1e16}]`), 0, true, false},
		{"合计溢出", quotaEnvelope(`[{"CapacityRemain":1e15},{"CapacityRemain":1e15}]`), 0, true, false},
	} {
		for _, domain := range []string{DomainCN, DomainGlobal} {
			t.Run(domain+"/"+tc.name, func(t *testing.T) {
				c := New(Config{})
				c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, pathUserResource) || r.Header.Get("Authorization") == "" {
						t.Fatal("未使用实际额度协议和鉴权")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})
				got, err := c.FetchQuota(context.Background(), provider.Credential{Raw: []byte(fmt.Sprintf(`{"access_token":"synthetic-secret","uid":"test-user","domain":%q}`, domain))})
				if tc.fail {
					if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
						t.Fatal("无效额度仍被接受或泄露凭证")
					}
					return
				}
				if err != nil || got.State != "ok" || len(got.Amounts) != 1 || got.Amounts[0].Remaining == nil || *got.Amounts[0].Remaining != tc.remaining || got.CheckedAt.IsZero() {
					t.Fatalf("额度解析错误: %#v, %v", got, err)
				}
				if tc.unknownTotal && got.Amounts[0].Total != nil {
					t.Fatal("未知总额被补零")
				}
			})
		}
	}
}

func TestQuotaPaginationAndRedirect(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		c := New(Config{})
		calls := 0
		c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			var body struct{ PageNumber, PageSize int }
			json.NewDecoder(r.Body).Decode(&body)
			if body.PageNumber != calls || body.PageSize != 100 {
				t.Fatal("分页参数错误")
			}
			items := []string{`{"CapacityRemain":5}`}
			if calls == 1 || repeat {
				items = make([]string, 100)
				for i := range items {
					items[i] = `{"CapacityRemain":2}`
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(quotaEnvelope("[" + strings.Join(items, ",") + "]"))), Header: http.Header{}}, nil
		})
		got, err := c.FetchQuota(t.Context(), provider.Credential{Raw: []byte(`{"access_token":"test-key","uid":"user","domain":"codebuddy.cn"}`)})
		if repeat {
			if err == nil {
				t.Fatal("重复页未拒绝")
			}
		} else if err != nil || calls != 2 || *got.Amounts[0].Remaining != 205 {
			t.Fatal("分页未完整合并")
		}
	}
	c := New(Config{})
	calls := 0
	c.http.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://untrusted.example/balance"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	_, err := c.FetchQuota(t.Context(), provider.Credential{Raw: []byte(`{"access_token":"test-key","uid":"user","domain":"codebuddy.cn"}`)})
	if err == nil || calls != 1 {
		t.Fatal("额度请求跟随了跨站重定向")
	}
}
