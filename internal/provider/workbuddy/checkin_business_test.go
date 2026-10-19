package workbuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

type checkinBusinessTransport func(*http.Request) (*http.Response, error)

func (f checkinBusinessTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWorkbuddyHTTP200BusinessFailureIsNotSuccessfulCheckin(t *testing.T) {
	for _, body := range []string{`{"code":9074}`, `{"success":false}`, `{"error":{"message":"synthetic-secret"}}`} {
		t.Run(body, func(t *testing.T) {
			c := New(Config{})
			c.http.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			cred := provider.Credential{Raw: []byte(`{"access_token":"synthetic-secret","uid":"synthetic-user","domain":"copilot.tencent.com"}`)}
			result, err := c.Checkin(context.Background(), cred)
			if err == nil || result.Status == provider.CheckinSuccess {
				t.Fatal("业务失败仍报告签到成功")
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("错误信息泄露令牌")
			}
		})
	}
}

// 无效响应和无关的“already”提示不能导致调度器停止重试当天奖励。
func TestWorkbuddyCheckinRejectsInvalidAcknowledgement(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"空响应", 200, ""},
		{"代理错误页", 200, "<html>synthetic-secret</html>"},
		{"截断对象", 200, `{"code":0`},
		{"空值", 200, `null`},
		{"数组", 200, `[]`},
		{"标量", 200, `true`},
		{"超长响应", 200, `{"code":0}` + strings.Repeat(" ", maxErrorBody)},
		{"登录过期", 400, `{"error":{"message":"token already expired"}}`},
		{"已超额度", 400, `{"error":{"message":"今天已超额度"}}`},
	}
	for _, region := range []string{DomainCN, DomainGlobal} {
		for _, tt := range cases {
			t.Run(region+"/"+tt.name, func(t *testing.T) {
				c := New(Config{})
				c.http.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body)), Header: http.Header{}}, nil
				})
				cred := provider.Credential{Raw: []byte(`{"access_token":"test-access","uid":"test-user","domain":"` + region + `"}`)}
				result, err := c.Checkin(context.Background(), cred)
				if err == nil || result.Status == provider.CheckinSuccess || result.Status == provider.CheckinAlready {
					t.Fatal("未确认成功的响应仍被当作已领取")
				}
			})
		}
	}
}

type checkinUnreadableBody struct{}

func (checkinUnreadableBody) Read([]byte) (int, error) { return 0, errors.New("test-read-error") }

func TestWorkbuddyCheckinReadFailureIsNotSuccess(t *testing.T) {
	c := New(Config{})
	c.http.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(checkinUnreadableBody{}), Header: http.Header{}}, nil
	})
	_, err := c.Checkin(context.Background(), provider.Credential{Raw: []byte(`{"access_token":"test-access","uid":"test-user","domain":"codebuddy.cn"}`)})
	if err == nil {
		t.Fatal("响应读取失败仍报告成功")
	}
}

func TestWorkbuddyCheckinPreservesValidResults(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"code":0,"data":{}}`, provider.CheckinSuccess},
		{200, `{"success":true,"error":null}`, provider.CheckinSuccess},
		{400, `{"error":{"message":"今天已签到，请明天再来"}}`, provider.CheckinAlready},
		{200, `{"message":"already checked in today"}`, provider.CheckinAlready},
	} {
		c := New(Config{})
		c.http.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body)), Header: http.Header{}}, nil
		})
		got, err := c.Checkin(context.Background(), provider.Credential{Raw: []byte(`{"access_token":"test-access","uid":"test-user","domain":"codebuddy.cn"}`)})
		if err != nil || got.Status != tt.want {
			t.Fatalf("正常领奖结果被改变：%v，%v", got.Status, err)
		}
	}
}
