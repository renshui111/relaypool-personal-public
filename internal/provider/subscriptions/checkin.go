package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func (c *Client) Checkin(ctx context.Context, cred provider.Credential) (provider.CheckinResult, error) {
	if !provider.CheckinFor(c.family, string(cred.Region)).Supported {
		return provider.CheckinResult{}, provider.ErrUnsupported
	}
	if cred.Field("access_token") == "" {
		return provider.CheckinResult{}, fmt.Errorf("签到需要有效登录态，请先授权或刷新凭证")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if c.family == "qoder" {
		return c.claimQoderCampaign(ctx, cred)
	}
	return c.checkinTrae(ctx, cred)
}

// 不把上游正文交给页面或日志，避免失败响应回显令牌和设备资料。
func (c *Client) checkinJSON(ctx context.Context, cred provider.Credential, method, endpoint string, h http.Header) (map[string]any, error) {
	var body []byte
	if method == http.MethodPost {
		body = []byte("{}")
	}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, cred, method, endpoint, body, h, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("签到响应无法读取或超过限制")
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, fmt.Errorf("签到响应格式无效")
	}
	if code, ok := m["code"]; ok && fmt.Sprint(code) != "0" {
		return nil, fmt.Errorf("签到服务拒绝本次请求，请核对登录态、资格和原设备信息")
	}
	if data, ok := m["data"].(map[string]any); ok {
		return data, nil
	}
	return m, nil
}

func (c *Client) checkinTrae(ctx context.Context, cred provider.Credential) (provider.CheckinResult, error) {
	device := cred.Field("device_id")
	if device == "" {
		return provider.CheckinResult{}, fmt.Errorf("Trae 签到缺少原登录设备编号")
	}
	// 数字设备编号必须保留；其他客户端编号按已有协议加前缀，不替换设备。
	if !strings.HasPrefix(device, "aha-") && !regexp.MustCompile(`^\d+$`).MatchString(device) {
		device = "aha-" + device
	}
	h := http.Header{"Authorization": {"Cloud-IDE-JWT " + cred.Field("access_token")}, "X-User-Region": {"CN"}, "X-Device-Id": {device}, "User-Agent": {"Trae/" + cred.Field("ide_version")}}
	const base = "https://api.trae.cn/trae/api/v2/ug/checkin_credits/"
	probe := func() (map[string]any, error) {
		return c.checkinJSON(ctx, cred, http.MethodPost, base+"status", h.Clone())
	}
	before, err := probe()
	if err != nil {
		return provider.CheckinResult{}, err
	}
	checked, valid := before["checked_in"].(bool)
	if !valid {
		return provider.CheckinResult{}, fmt.Errorf("Trae 未返回可确认的签到状态")
	}
	if checked {
		return provider.CheckinResult{Status: provider.CheckinAlready, Message: "平台确认今日已领取积分"}, nil
	}
	if enabled, ok := before["enable"].(bool); !ok {
		return provider.CheckinResult{}, fmt.Errorf("Trae 未返回领取资格")
	} else if !enabled {
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "当前账号不具备领取资格"}, nil
	}
	if _, err := c.checkinJSON(ctx, cred, http.MethodPost, base+"claim", h.Clone()); err != nil {
		return provider.CheckinResult{}, err
	}
	after, err := probe()
	if err != nil {
		return provider.CheckinResult{}, err
	}
	if checked, _ := after["checked_in"].(bool); !checked {
		return provider.CheckinResult{}, fmt.Errorf("领取后状态未更新，平台尚未确认发奖，请稍后重试")
	}
	return provider.CheckinResult{Status: provider.CheckinSuccess, Message: "已领取每日积分，并经平台复核确认"}, nil
}

var campaignIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (c *Client) claimQoderCampaign(ctx context.Context, cred provider.Credential) (provider.CheckinResult, error) {
	h := http.Header{"Authorization": {"Bearer " + cred.Field("access_token")}, "Cosy-ClientType": {"5"}, "Cosy-Version": {"0.4.3"}, "User-Agent": {"Qoder"}}
	for field, header := range map[string]string{"machine_id": "Cosy-MachineId", "machine_token": "Cosy-MachineToken", "machine_type": "Cosy-MachineType"} {
		if value := cred.Field(field); value != "" {
			h.Set(header, value)
		}
	}
	base := cred.Field("api_host") + "/sash/api/v1/me/campaigns"
	m, err := c.checkinJSON(ctx, cred, http.MethodGet, base, h.Clone())
	if err != nil {
		return provider.CheckinResult{}, err
	}
	list, valid := m["campaigns"].([]any)
	if !valid {
		return provider.CheckinResult{}, fmt.Errorf("Qoder 未返回可确认的活动列表")
	}
	if len(list) > 128 {
		return provider.CheckinResult{}, fmt.Errorf("活动数量超过处理限制")
	}
	now := float64(time.Now().Unix())
	claimed, already := 0, 0
	for _, entry := range list {
		x, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		benefit, _ := x["benefit"].(map[string]any)
		if x["actionType"] != "CLAIM_BENEFIT" || benefit["kind"] != "CREDITS" {
			continue
		}
		if start, ok := x["startAt"].(float64); ok && start > now {
			continue
		}
		if end, ok := x["endAt"].(float64); ok && end <= now {
			continue
		}
		if x["claimStatus"] == "CLAIMED" {
			already++
			continue
		}
		if x["claimStatus"] != "CLAIMABLE" {
			continue
		}
		id, ok := x["campaignId"].(string)
		if !ok || !campaignIDPattern.MatchString(id) {
			continue
		}
		result, err := c.checkinJSON(ctx, cred, http.MethodPost, base+"/"+id+"/claim", h.Clone())
		if err != nil {
			return provider.CheckinResult{}, err
		}
		if result["status"] != "CLAIMED" {
			return provider.CheckinResult{}, fmt.Errorf("Qoder 尚未确认发放活动奖励，请核对活动资格和原客户端设备信息")
		}
		if replayed, _ := result["replayed"].(bool); replayed {
			already++
		} else {
			claimed++
		}
	}
	if claimed > 0 {
		return provider.CheckinResult{Status: provider.CheckinSuccess, Message: fmt.Sprintf("平台确认已领取 %d 项积分活动奖励", claimed)}, nil
	}
	if already > 0 {
		return provider.CheckinResult{Status: provider.CheckinAlready, Message: "平台确认当前积分活动奖励已领取"}, nil
	}
	return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "当前没有可领取的积分活动；可能未满足活动或设备资格"}, nil
}
