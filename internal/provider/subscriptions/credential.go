package subscriptions

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

const (
	qoderChatPath   = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderModelsPath = "/algo/api/v2/model/list?Encode=1"
	traeChatPath    = "/api/agent/v3/llm_utils_chat"
	traeModelsPath  = "/api/ide/v1/get_detail_param"
)

func textValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}
func fieldKey(key string) string      { return strings.ToLower(strings.ReplaceAll(key, "_", "")) }
func object(value any) map[string]any { v, _ := value.(map[string]any); return v }
func lookup(root map[string]any, names ...string) string {
	for _, name := range names {
		for k, v := range root {
			if fieldKey(k) == fieldKey(name) {
				if s := textValue(v); s != "" {
					return s
				}
			}
		}
	}
	return ""
}
func importedField(root map[string]any, names ...string) string {
	if value := lookup(root, names...); value != "" {
		return value
	}
	for _, nested := range []string{"auth", "account", "tokens", "providerSpecificData", "provider_specific_data"} {
		if value := lookup(object(root[nested]), names...); value != "" {
			return value
		}
	}
	return ""
}

func (c *Client) DecodeCredential(raw []byte) (provider.Credential, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return provider.Credential{}, fmt.Errorf("登录态 JSON 为空或超过 1 MB")
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil || root == nil {
		return provider.Credential{}, fmt.Errorf("登录态必须是 JSON 对象")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return provider.Credential{}, fmt.Errorf("登录态 JSON 后存在多余内容")
	}
	// 有来源标记时严格检查；显式导入不扫描个人客户端目录。
	declared := strings.ToLower(lookup(root, "provider", "type"))
	if declared != "" && declared != c.family && !strings.HasPrefix(declared, c.family+"-") && !(c.family == "qoder" && declared == "qoderwork") && !(c.family == "trae-intl" && declared == "trae") {
		return provider.Credential{}, fmt.Errorf("登录态来源与所选平台不一致")
	}
	fields := map[string]string{}
	for _, name := range []string{"access_token", "refresh_token", "personal_token", "uid", "enterprise_id", "nickname", "expires_at", "domain", "region", "machine_id", "machine_token", "machine_type", "device_id", "ide_version", "ide_version_code", "refresh_client_id", "variant", "scene", "api_host", "base_url", "user_type"} {
		fields[name] = importedField(root, name)
	}
	if fields["access_token"] == "" {
		fields["access_token"] = importedField(root, "token")
	}
	if fields["personal_token"] == "" {
		fields["personal_token"] = importedField(root, "pat")
	}
	region := fields["region"]
	if c.family == "trae-intl" {
		for _, name := range []string{"web_id", "biz_user_id", "user_unique_id", "user_identity", "scope", "tenant", "upstream_region", "app_language", "app_version", "referer_origin", "timezone"} {
			fields[name] = importedField(root, name)
		}
		if fields["timezone"] == "" {
			fields["timezone"] = importedField(root, "user_timezone")
		}
		if region == "US-East" || region == "US" || region == "SG" {
			if fields["upstream_region"] == "" {
				fields["upstream_region"] = region
			}
			region = "global"
		}
	}
	domain := strings.ToLower(fields["domain"])
	switch region {
	case "intl":
		region = "global"
	case "", "cn", "global":
	default:
		return provider.Credential{}, fmt.Errorf("登录态地区只支持 cn 或 global")
	}
	if domain != "" {
		var domainRegion string
		if c.family == "qoder" {
			switch domain {
			case "qoder.cn", "qoder.com.cn":
				domainRegion = "cn"
			case "qoder.com", "qoder.sh":
				domainRegion = "global"
			default:
				return provider.Credential{}, fmt.Errorf("Qoder 登录态域名无效")
			}
		} else {
			switch domain {
			case "trae.cn", "www.trae.cn", "trae.com.cn":
				domainRegion = "cn"
			case "trae.ai", "www.trae.ai":
				domainRegion = "global"
			default:
				return provider.Credential{}, fmt.Errorf("Trae 登录态域名无效")
			}
		}
		if region != "" && region != domainRegion {
			return provider.Credential{}, fmt.Errorf("登录态地区和域名冲突")
		}
		region = domainRegion
	}
	if region == "" {
		region = "cn"
		if c.family == "trae-intl" {
			region = "global"
		}
	}
	fields["region"] = region
	base, authBase := "https://gateway.qoder.com.cn", "https://openapi.qoder.com.cn"
	if c.family == "qoder" {
		if region == "global" {
			base, authBase = "https://api3.qoder.sh", "https://openapi.qoder.sh"
		}
		if fields["access_token"] == "" && fields["personal_token"] == "" {
			return provider.Credential{}, fmt.Errorf("Qoder 需要 access_token 或本人 PAT personal_token")
		}
		if fields["access_token"] != "" && fields["uid"] == "" {
			return provider.Credential{}, fmt.Errorf("Qoder 登录态缺少 uid，请导入包含 account 与 auth 的完整 JSON")
		}
		if fields["refresh_token"] != "" && !strings.HasPrefix(fields["refresh_token"], "drt-") && !strings.HasPrefix(fields["refresh_token"], "jrt-") {
			return provider.Credential{}, fmt.Errorf("Qoder 刷新令牌需要 drt- 或 jrt- 类型")
		}
		if fields["user_type"] == "" {
			fields["user_type"] = "personal_standard"
		}
	} else if c.family == "trae-intl" {
		if region != "global" {
			return provider.Credential{}, fmt.Errorf("Trae 国际渠道只接受 global 地区，国内账号请选择 Trae 国内聊天")
		}
		base, authBase = "https://core-normal.trae.ai", "https://api.marscode.com"
		if fields["access_token"] == "" || fields["uid"] == "" {
			return provider.Credential{}, fmt.Errorf("Trae 国际登录态需要完整 access_token 与 uid")
		}
		fields["variant"], fields["scene"] = "intl", "remote"
		if fields["refresh_client_id"] == "" {
			fields["refresh_client_id"] = "ono9krqynydwx5"
		}
		if fields["refresh_client_id"] != "ono9krqynydwx5" && fields["refresh_client_id"] != "en1oxy7wnw8j9n" {
			return provider.Credential{}, fmt.Errorf("Trae 国际刷新客户端类型无效")
		}
		if fields["ide_version"] == "" {
			fields["ide_version"] = "3.5.66"
		}
		if fields["referer_origin"] == "" {
			fields["referer_origin"] = "https://work.trae.ai"
		}
		if fields["referer_origin"] != "https://work.trae.ai" && fields["referer_origin"] != "https://solo.trae.ai" {
			return provider.Credential{}, fmt.Errorf("Trae 国际来源只接受本人登录使用的 work.trae.ai 或 solo.trae.ai")
		}
		for name, fallback := range map[string]string{"app_language": "en", "app_version": "1.0.0.1229", "upstream_region": "US-East", "scope": "marscode-us", "tenant": "marscode", "user_identity": "Free"} {
			if fields[name] == "" {
				fields[name] = fallback
			}
		}
		for _, name := range []string{"web_id", "biz_user_id", "user_unique_id", "user_identity", "scope", "tenant", "upstream_region", "app_language", "app_version", "timezone"} {
			if len(fields[name]) > 1024 || strings.ContainsAny(fields[name], "\r\n\x00") {
				return provider.Credential{}, fmt.Errorf("国际登录态字段 %s 格式无效", name)
			}
		}
	} else {
		if region != "cn" {
			return provider.Credential{}, fmt.Errorf("Trae 国际版远程代理协议尚未作为聊天渠道接入，请使用国内聊天登录态")
		}
		base, authBase = "https://trae-api-cn.mchost.guru", "https://api.trae.com.cn"
		if fields["access_token"] == "" || fields["uid"] == "" {
			return provider.Credential{}, fmt.Errorf("Trae 需要完整 access_token 与 uid")
		}
		if fields["machine_id"] == "" || fields["device_id"] == "" {
			return provider.Credential{}, fmt.Errorf("Trae 需要原登录设备 machine_id 和 device_id，不自动生成替代设备")
		}
		if fields["variant"] == "" {
			fields["variant"] = "solo"
		}
		if fields["variant"] != "solo" && fields["variant"] != "cn" {
			return provider.Credential{}, fmt.Errorf("Trae 国内产品 variant 只支持 solo 或 cn")
		}
		if fields["refresh_client_id"] == "" {
			if fields["variant"] == "solo" {
				fields["refresh_client_id"] = "en1oxy7wnw8j9n"
			} else {
				fields["refresh_client_id"] = "ono9krqynydwx5"
			}
		}
		if fields["refresh_client_id"] != "en1oxy7wnw8j9n" && fields["refresh_client_id"] != "ono9krqynydwx5" {
			return provider.Credential{}, fmt.Errorf("Trae 刷新客户端类型无效")
		}
		if fields["scene"] == "" {
			fields["scene"] = "solo_work_lite"
		}
		if fields["scene"] != "solo_work_lite" && fields["scene"] != "chat_v3" {
			return provider.Credential{}, fmt.Errorf("Trae 聊天场景只支持 solo_work_lite 或 chat_v3")
		}
		if fields["ide_version"] == "" {
			if fields["scene"] == "chat_v3" {
				fields["ide_version"] = "3.3.62"
			} else {
				fields["ide_version"] = "0.1.61"
			}
		}
		if fields["ide_version_code"] == "" {
			if fields["scene"] == "chat_v3" {
				fields["ide_version_code"] = "20260811"
			} else {
				fields["ide_version_code"] = "20260820"
			}
		}
	}
	var err error
	fields["base_url"], err = restrictedEndpoint(fields["base_url"], base)
	if err != nil {
		return provider.Credential{}, err
	}
	fields["api_host"], err = restrictedEndpoint(fields["api_host"], authBase)
	if err != nil {
		return provider.Credential{}, err
	}
	for _, name := range []string{"access_token", "refresh_token", "personal_token", "uid", "enterprise_id", "nickname", "machine_id", "machine_token", "machine_type", "device_id", "ide_version", "ide_version_code"} {
		if len(fields[name]) > 16384 || strings.ContainsAny(fields[name], "\r\n\x00") {
			return provider.Credential{}, fmt.Errorf("登录态字段 %s 格式无效", name)
		}
	}
	models, exists := root["models"]
	if exists {
		list, ok := models.([]any)
		if !ok || len(list) > 2048 {
			return provider.Credential{}, fmt.Errorf("models 必须是型号字符串数组，最多 2048 项")
		}
		for _, v := range list {
			if _, ok := v.(string); !ok || textValue(v) == "" {
				return provider.Credential{}, fmt.Errorf("models 存在空值或非字符串")
			}
		}
		encoded, _ := json.Marshal(list)
		fields["models"] = string(encoded)
	}
	expires, err := expiryTime(fields["expires_at"])
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.Credential{Provider: c.family, Region: provider.Region(region), Raw: append([]byte{}, raw...), Fields: fields, ExpiresAt: expires}, nil
}

func expiryTime(value string) (time.Time, error) {
	if value == "" || value == "0" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(n, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expires_at 必须为有效时间戳或 RFC3339 时间")
}

// 订阅令牌只发给所选地区的固定上游；本地测试允许回环地址。
func restrictedEndpoint(value, fallback string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("订阅上游地址无效")
	}
	clean := strings.TrimRight(u.String(), "/")
	if clean == fallback {
		return clean, nil
	}
	ip := net.ParseIP(u.Hostname())
	if (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) && (u.Path == "" || u.Path == "/") {
		return clean, nil
	}
	return "", fmt.Errorf("订阅令牌仅允许发往所选地区官方地址或本机回环测试服务")
}
