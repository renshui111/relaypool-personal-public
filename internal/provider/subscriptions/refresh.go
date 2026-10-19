package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// 相同账号和旧令牌只刷新一次；成功结果短期复用，防止旋转令牌并发失效。
func (c *Client) Refresh(ctx context.Context, cred provider.Credential) (provider.Credential, error) {
	if cred.Field("refresh_token") == "" && cred.Field("personal_token") == "" {
		return provider.Credential{}, provider.ErrUnsupported
	}
	key := credentialKey(cred)
	now := time.Now()
	c.mu.Lock()
	for k, call := range c.refreshes {
		if !call.finished.IsZero() && now.Sub(call.finished) > 5*time.Minute {
			delete(c.refreshes, k)
		}
	}
	if call, ok := c.refreshes[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return provider.Credential{}, ctx.Err()
		case <-call.done:
			updated := call.credential
			updated.AccountID = cred.AccountID
			return updated, call.err
		}
	}
	if len(c.refreshes) >= 128 {
		// 已完成的结果只是短期缓存，不能让前 128 个账号阻塞后续账号五分钟。
		var oldest string
		var finished time.Time
		for k, cached := range c.refreshes {
			if !cached.finished.IsZero() && (oldest == "" || cached.finished.Before(finished)) {
				oldest, finished = k, cached.finished
			}
		}
		if oldest != "" {
			delete(c.refreshes, oldest)
		} else {
			c.mu.Unlock()
			return provider.Credential{}, failure(503, "当前凭证刷新过多，请稍后重试")
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	c.refreshes[key] = call
	c.mu.Unlock()
	updated, err := c.refresh(ctx, cred)
	c.mu.Lock()
	call.credential, call.err, call.finished = updated, err, time.Now()
	if err != nil {
		delete(c.refreshes, key)
	}
	close(call.done)
	c.mu.Unlock()
	return updated, err
}

func (c *Client) refresh(ctx context.Context, cred provider.Credential) (provider.Credential, error) {
	path := "/api/v1/jobToken/refresh"
	body := map[string]any{"refresh_token": cred.Field("refresh_token")}
	headers := http.Header{"Content-Type": []string{"application/json"}, "Accept": []string{"application/json"}}
	patExchange := false
	if c.family == "qoder" {
		if cred.Field("personal_token") != "" && (cred.Field("access_token") == "" || cred.Field("refresh_token") == "") {
			path = "/api/v1/jobToken/exchange"
			body = map[string]any{"personal_token": cred.Field("personal_token")}
			patExchange = true
		} else if strings.HasPrefix(cred.Field("refresh_token"), "drt-") {
			path = "/api/v1/deviceToken/refresh"
		}
	} else {
		path = "/cloudide/api/v3/trae/oauth/ExchangeToken"
		body = map[string]any{"ClientID": cred.Field("refresh_client_id"), "RefreshToken": cred.Field("refresh_token"), "ClientSecret": "-", "UserID": ""}
		headers.Set("User-Agent", "Trae/"+cred.Field("ide_version"))
	}
	encoded, _ := json.Marshal(body)
	response, err := c.do(ctx, cred, "POST", cred.Field("api_host")+path, encoded, headers, false)
	if err != nil {
		if f, ok := provider.AsFailure(err); ok && c.family == "qoder" && !patExchange && cred.Field("personal_token") != "" && (f.Status == 401 || f.Status == 403) {
			// 只在旧刷新令牌被认证拒绝时交换本人 PAT；不重复网络故障或限流请求。
			fallback := cred
			fallback.Fields = make(map[string]string, len(cred.Fields))
			for key, value := range cred.Fields {
				fallback.Fields[key] = value
			}
			fallback.Fields["access_token"] = ""
			return c.refresh(ctx, fallback)
		}
		return provider.Credential{}, err
	}
	defer response.Body.Close()
	var result map[string]any
	if err := decodeLimited(response.Body, &result); err != nil {
		return provider.Credential{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(marshal(result))))
	decoder.UseNumber()
	_ = decoder.Decode(&result)
	values := result
	if c.family == "trae" || c.family == "trae-intl" {
		values = object(result["Result"])
	}
	access := lookup(values, "token", "device_token", "access_token")
	if access == "" {
		return provider.Credential{}, failure(401, "订阅凭证刷新失败，请重新导入本人登录态")
	}
	if c.family == "qoder" && strings.HasPrefix(cred.Field("refresh_token"), "drt-") && lookup(values, "refresh_token") == "" {
		return provider.Credential{}, failure(502, "Qoder 旋转刷新响应缺少新 refresh_token，保留原凭证")
	}
	// 用标准字段保存更新，移除旧嵌套令牌，保留手动型号和导入的设备信息。
	var original map[string]any
	_ = json.Unmarshal(cred.Raw, &original)
	if original == nil {
		original = map[string]any{}
	}
	delete(original, "auth")
	delete(original, "tokens")
	delete(original, "account")
	for name, value := range cred.Fields {
		// accessToken 等别名不能和新的 access_token 共存，否则解码会随机读到旧值。
		for key := range original {
			if fieldKey(key) == fieldKey(name) {
				delete(original, key)
			}
		}
		if name == "models" {
			var models []string
			_ = json.Unmarshal([]byte(value), &models)
			original[name] = models
		} else if value != "" {
			original[name] = value
		}
	}
	original["access_token"] = access
	if refreshed := lookup(values, "refresh_token"); refreshed != "" {
		original["refresh_token"] = refreshed
	}
	expires := lookup(values, "expires_at", "token_expire_at")
	if expires == "" {
		seconds := lookup(values, "expires_in", "token_expire_duration")
		if seconds != "" {
			unit := "s"
			if c.family == "qoder" {
				unit = "ms"
			}
			duration, err := time.ParseDuration(seconds + unit)
			if err == nil && duration > 0 {
				expires = time.Now().Add(duration).UTC().Format(time.RFC3339)
			}
		}
	}
	if expires == "" {
		return provider.Credential{}, failure(502, "刷新响应缺少有效过期时间，未写入新令牌")
	}
	original["expires_at"] = expires
	if patExchange {
		userHeaders := http.Header{"Authorization": []string{"Bearer " + access}, "Accept": []string{"application/json"}}
		userResponse, err := c.do(ctx, cred, "GET", cred.Field("api_host")+"/api/v1/userinfo", nil, userHeaders, false)
		if err != nil {
			return provider.Credential{}, err
		}
		defer userResponse.Body.Close()
		var user map[string]any
		if err := decodeLimited(userResponse.Body, &user); err != nil {
			return provider.Credential{}, err
		}
		// Qoder userinfo 使用平铺 snake_case 账户信息。
		for key, aliases := range map[string][]string{"uid": {"uid", "id"}, "nickname": {"nickname", "name"}, "enterprise_id": {"enterprise_id", "organization_id"}, "user_type": {"user_type"}} {
			if v := lookup(user, aliases...); v != "" {
				original[key] = v
			}
		}
	}
	updated, err := c.DecodeCredential(marshal(original))
	if err != nil {
		return provider.Credential{}, failure(502, "更新后的订阅凭证无效，保留原凭证")
	}
	updated.AccountID, updated.PreviousRaw = cred.AccountID, append([]byte{}, cred.Raw...)
	return updated, nil
}

func marshal(value any) []byte { encoded, _ := json.Marshal(value); return encoded }
