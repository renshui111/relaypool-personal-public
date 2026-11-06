package huawei

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// Credit APIs can return numbers as JSON numbers or decimal strings. Absence,
// null, and malformed values remain unknown, never a fabricated zero balance.
type creditNumber struct {
	value float64
	valid bool
}

func (n *creditNumber) UnmarshalJSON(raw []byte) error {
	n.valid = false
	s := string(raw)
	if len(s) > 0 && s[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err == nil && !math.IsInf(v, 0) && !math.IsNaN(v) && v >= 0 {
		n.value, n.valid = v, true
	}
	return nil
}

func (c *Client) SupportsQuota() bool { return true }

func (c *Client) creditAccess(cred provider.Credential) string {
	if cred.Field("access_key") == "" || cred.Field("secret_key") == "" {
		return "积分和签到需要华为登录授权，请在账号池重新进行浏览器授权；应用密钥只能用于模型调用"
	}
	expires := cred.ExpiresAt
	if c.family == OfficeAce {
		var err error
		expires, err = time.Parse(time.RFC3339, cred.Field("sts_expires_at"))
		if err != nil || expires.IsZero() || cred.Field("security_token") == "" {
			return "果办积分权限不完整，请重新进行浏览器授权"
		}
	}
	if !expires.IsZero() && time.Until(expires) <= 0 {
		return "积分查询登录态已过期，请等待自动续期或重新进行浏览器授权"
	}
	return ""
}

func officeCreditBase(cred provider.Credential) (string, error) {
	u, err := url.Parse(cred.Field("base_url"))
	if err != nil {
		return "", fmt.Errorf("果办积分网关无效")
	}
	local := u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")
	trusted := u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".officeace.cn-southwest-2.huaweicloud-agentarts.com") && (u.Port() == "" || u.Port() == "443")
	if (!local && !trusted) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/v2") {
		return "", fmt.Errorf("积分查询需要账号下发的官方果办网关，请重新授权")
	}
	u.Path = ""
	return u.String(), nil
}

func (c *Client) creditRequest(ctx context.Context, cred provider.Credential, method, path string, body []byte, out any) error {
	base := c.snap
	if c.family == OfficeAce {
		var err error
		base, err = officeCreditBase(cred)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("无法构造积分请求")
	}
	if c.family == OfficeAce {
		r.Header.Set("x-subscription-type", "v2")
		if body != nil {
			r.Header.Set("Content-Type", "application/json;charset=utf8")
		}
		err = signV11(r, body, cred)
	} else {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Agent-Type", "PromptCenter")
		err = c.authenticate(r, cred, body)
	}
	if err != nil {
		return err
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("华为积分服务连接失败")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return fmt.Errorf("华为积分响应读取失败或过大")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("华为积分服务返回 HTTP %d", resp.StatusCode)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return fmt.Errorf("华为积分服务返回格式无效")
	}
	for _, key := range []string{"error", "error_code", "errorCode"} {
		if v, ok := envelope[key]; ok && string(v) != "null" && string(v) != `""` && string(v) != "0" {
			return fmt.Errorf("华为积分服务拒绝请求，请检查登录态和领取资格")
		}
	}
	if v, ok := envelope["code"]; ok && string(v) != "0" && string(v) != `"0"` {
		return fmt.Errorf("华为积分业务请求未成功，请检查登录态和领取资格")
	}
	if v, ok := envelope["success"]; ok && string(v) == "false" {
		return fmt.Errorf("华为积分业务请求未成功")
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("华为积分响应字段无效")
	}
	return nil
}

func unknownCredits(message string) provider.QuotaSnapshot {
	return provider.QuotaSnapshot{State: "unknown", CheckedAt: time.Now(), Message: message}
}

func creditSnapshot(total, used, remaining *float64) provider.QuotaSnapshot {
	return provider.QuotaSnapshot{State: "ok", CheckedAt: time.Now(), Amounts: []provider.QuotaAmount{{Label: "可用积分", Unit: "积分", Total: total, Used: used, Remaining: remaining}}}
}

func (c *Client) FetchQuota(ctx context.Context, cred provider.Credential) (provider.QuotaSnapshot, error) {
	if reason := c.creditAccess(cred); reason != "" {
		return unknownCredits(reason), nil
	}
	if c.family == OfficeAce {
		var sub officeSubscription
		if err := c.creditRequest(ctx, cred, "GET", "/v1/subscription", nil, &sub); err != nil {
			return provider.QuotaSnapshot{}, err
		}
		return sub.snapshot(time.Now()), nil
	}
	var response struct {
		Metrics []struct {
			Name      string       `json:"name"`
			Total     creditNumber `json:"package_credit_amount"`
			Used      creditNumber `json:"package_credit_used"`
			Remaining creditNumber `json:"package_credit_remain"`
		} `json:"metrics"`
	}
	if err := c.creditRequest(ctx, cred, "GET", "/snap-manager/v1/statistics/plugin", nil, &response); err != nil {
		return provider.QuotaSnapshot{}, err
	}
	for _, m := range response.Metrics {
		if m.Name != "usageTotalPackageCredit" {
			continue
		}
		var total, used, remaining *float64
		if m.Total.valid {
			total = &m.Total.value
		}
		if m.Used.valid {
			used = &m.Used.value
		}
		if m.Remaining.valid {
			remaining = &m.Remaining.value
		} else if total != nil && used != nil {
			v := math.Max(0, *total-*used)
			remaining = &v
		}
		if remaining != nil {
			return creditSnapshot(total, used, remaining), nil
		}
	}
	return unknownCredits("上游未返回可用积分，账号可能尚未开通积分套餐"), nil
}

type officeBonus struct {
	ActivityID json.RawMessage `json:"activity_id"`
	Name       string          `json:"activity_name"`
	Points     creditNumber    `json:"points"`
	Used       creditNumber    `json:"current_value"`
	Effective  string          `json:"effective_time"`
	Expires    string          `json:"expired_time"`
}
type officeSubscription struct {
	Status string `json:"subscribe_status"`
	Skus   []struct {
		Status    string `json:"subscribe_status"`
		Effective string `json:"effective_time"`
		Expires   string `json:"expired_time"`
		Quotas    []struct {
			Code    string       `json:"sku_attr_code"`
			Total   creditNumber `json:"sku_value"`
			Used    creditNumber `json:"current_value"`
			Expires string       `json:"expired_time"`
		} `json:"quotas"`
	} `json:"skus"`
	Bonuses []officeBonus `json:"bonus_skus"`
}

// Invalid dates cannot make an expired or future entitlement look spendable.
func creditWindow(start, end string, now time.Time) (active, valid bool) {
	for i, value := range []string{start, end} {
		if value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return false, false
		}
		if (i == 0 && now.Before(t)) || (i == 1 && !now.Before(t)) {
			return false, true
		}
	}
	return true, true
}

func (s officeSubscription) snapshot(now time.Time) provider.QuotaSnapshot {
	unknown := func() provider.QuotaSnapshot {
		return unknownCredits("上游积分信息不完整或权益不可用，请到果办查看套餐")
	}
	if s.Skus == nil || s.Bonuses == nil || s.Status == "FROZEN" {
		return unknown()
	}
	total, used, remaining := 0.0, 0.0, 0.0
	found := false
	add := func(t, u creditNumber) bool {
		if !t.valid || !u.valid {
			return false
		}
		total += t.value
		used += u.value
		remaining += math.Max(0, t.value-u.value)
		found = true
		return !math.IsInf(total, 0) && !math.IsInf(used, 0)
	}
	for _, sku := range s.Skus {
		active, valid := creditWindow(sku.Effective, sku.Expires, now)
		if !valid {
			return unknown()
		}
		if !active {
			continue
		}
		if sku.Status != "" && sku.Status != "SUBSCRIBED" {
			continue
		}
		for _, q := range sku.Quotas {
			if q.Code != "officeace_points" {
				continue
			}
			active, valid = creditWindow("", q.Expires, now)
			if !valid {
				return unknown()
			}
			if active && !add(q.Total, q.Used) {
				return unknown()
			}
		}
	}
	for _, b := range s.Bonuses {
		active, valid := creditWindow(b.Effective, b.Expires, now)
		if !valid {
			return unknown()
		}
		if active && !add(b.Points, b.Used) {
			return unknown()
		}
	}
	if !found {
		return unknownCredits("当前没有可核实的有效积分权益")
	}
	return creditSnapshot(&total, &used, &remaining)
}

type dailyCampaign struct {
	ID        int64  `json:"campaignId"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Claimable bool   `json:"claimable"`
	Extra     struct {
		TriggerMode string `json:"triggerMode"`
	} `json:"extra"`
}

func claimed(status string) bool {
	return status == "CLAIMED" || status == "CONFIRMED" || status == "CONSUMED"
}

func (c *Client) campaigns(ctx context.Context, cred provider.Credential) ([]dailyCampaign, error) {
	var response struct {
		Code *int `json:"code"`
		Data struct {
			Items []dailyCampaign `json:"items"`
		} `json:"data"`
	}
	err := c.creditRequest(ctx, cred, "GET", "/v1/ops/delivery?channel=IDE", nil, &response)
	if err != nil {
		return nil, err
	}
	if response.Code == nil || response.Data.Items == nil {
		return nil, fmt.Errorf("码道签到活动响应不完整")
	}
	return response.Data.Items, nil
}

func (c *Client) Checkin(ctx context.Context, cred provider.Credential) (provider.CheckinResult, error) {
	if reason := c.creditAccess(cred); reason != "" {
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: reason}, nil
	}
	if c.family == OfficeAce {
		return c.officeCheckin(ctx, cred)
	}
	items, err := c.campaigns(ctx, cred)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	ids := map[int64]bool{}
	already := false
	for _, item := range items {
		if item.Type != "USER_LOGIN" {
			continue
		}
		if claimed(item.Status) {
			already = true
			continue
		}
		if item.ID <= 0 || !item.Claimable || (item.Extra.TriggerMode != "" && item.Extra.TriggerMode != "MANUAL_CLAIM") {
			continue
		}
		// Stable per credential/account, campaign and Beijing day for safe retries.
		day := provider.CheckinPeriod(time.Now(), provider.CheckinFor(CodeArts, "cn"))
		identity := cred.AccountID
		if identity == "" {
			identity = cred.Field("access_key")
		}
		key := hash([]byte(identity + ":" + strconv.FormatInt(item.ID, 10) + ":" + day))[:32]
		key = key[:8] + "-" + key[8:12] + "-4" + key[13:16] + "-a" + key[17:20] + "-" + key[20:]
		body, _ := json.Marshal(map[string]any{"campaignId": item.ID, "idempotentKey": key, "channel": "IDE"})
		var reply struct {
			Code *int `json:"code"`
		}
		if err := c.creditRequest(ctx, cred, "POST", "/v1/ops/claim", body, &reply); err != nil {
			return provider.CheckinResult{}, err
		}
		if reply.Code == nil {
			return provider.CheckinResult{}, fmt.Errorf("码道未确认领取结果")
		}
		ids[item.ID] = true
	}
	if len(ids) == 0 {
		if already {
			return provider.CheckinResult{Status: provider.CheckinAlready, Message: "当日登录奖励已领取"}, nil
		}
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "当前没有可领取的每日登录奖励"}, nil
	}
	items, err = c.campaigns(ctx, cred)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	for _, item := range items {
		if item.Type == "USER_LOGIN" && claimed(item.Status) {
			delete(ids, item.ID)
		}
	}
	if len(ids) > 0 {
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "领取请求已提交，上游尚未确认到账，请稍后查询积分和重试"}, nil
	}
	return provider.CheckinResult{Status: provider.CheckinSuccess, Message: "每日登录奖励已领取并复核，请刷新积分查看余额"}, nil
}

func bonusKey(b officeBonus) string {
	return string(b.ActivityID) + "|" + b.Name + "|" + b.Effective + "|" + b.Expires
}

func (c *Client) officeCheckin(ctx context.Context, cred provider.Credential) (provider.CheckinResult, error) {
	var before, after officeSubscription
	if err := c.creditRequest(ctx, cred, "GET", "/v1/subscription", nil, &before); err != nil {
		return provider.CheckinResult{}, err
	}
	if before.Bonuses == nil {
		return provider.CheckinResult{}, fmt.Errorf("果办未返回奖励信息，未提交领取")
	}
	previous := map[string]float64{}
	for _, b := range before.Bonuses {
		if !b.Points.valid || !b.Used.valid {
			return provider.CheckinResult{}, fmt.Errorf("果办奖励数值不完整，未提交领取")
		}
		previous[bonusKey(b)] += math.Max(0, b.Points.value-b.Used.value)
	}
	if err := c.creditRequest(ctx, cred, "POST", "/v1/subscription/bonus/claim", []byte(`{}`), nil); err != nil {
		return provider.CheckinResult{}, err
	}
	if err := c.creditRequest(ctx, cred, "GET", "/v1/subscription", nil, &after); err != nil {
		return provider.CheckinResult{}, err
	}
	if after.Bonuses == nil {
		return provider.CheckinResult{}, fmt.Errorf("果办未返回领取后的奖励信息")
	}
	current := map[string]float64{}
	now := time.Now()
	already := false
	for _, b := range after.Bonuses {
		active, valid := creditWindow(b.Effective, b.Expires, now)
		if !valid || !b.Points.valid || !b.Used.valid {
			return provider.CheckinResult{}, fmt.Errorf("果办奖励信息不完整，无法核实到账")
		}
		if !active {
			continue
		}
		current[bonusKey(b)] += math.Max(0, b.Points.value-b.Used.value)
		t, err := time.Parse(time.RFC3339, b.Effective)
		if err == nil && b.Points.value > 0 && strings.Contains(b.Name, "每日") && (strings.Contains(b.Name, "签到") || strings.Contains(b.Name, "登录")) && provider.CheckinPeriod(t, provider.CheckinFor(OfficeAce, "cn")) == provider.CheckinPeriod(now, provider.CheckinFor(OfficeAce, "cn")) {
			already = true
		}
	}
	gain := 0.0
	for key, total := range current {
		gain += math.Max(0, total-previous[key])
	}
	if gain > 0 && !math.IsInf(gain, 0) {
		return provider.CheckinResult{Status: provider.CheckinSuccess, Message: fmt.Sprintf("已复核新增奖励 %g 积分，余额以上游查询为准", gain)}, nil
	}
	if already {
		return provider.CheckinResult{Status: provider.CheckinAlready, Message: "已核实今天的登录奖励，无新增积分"}, nil
	}
	return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "领取后未查询到新增奖励，可能已领取或当前无资格"}, nil
}
