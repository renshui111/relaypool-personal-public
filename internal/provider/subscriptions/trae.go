package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/relaypool/relaypool/internal/provider"
)

func traeHeaders(cred provider.Credential, stream bool) http.Header {
	h := http.Header{}
	for k, v := range map[string]string{"Content-Type": "application/json", "User-Agent": "Trae/" + cred.Field("ide_version"), "Authorization": "Cloud-IDE-JWT " + cred.Field("access_token"), "X-Cloudide-Token": cred.Field("access_token"), "X-Ide-Token": cred.Field("access_token"), "X-Uid": cred.Field("uid"), "X-App-Id": "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8", "X-App-Version": "default", "X-Ide-Version": cred.Field("ide_version"), "X-Ide-Version-Code": cred.Field("ide_version_code"), "X-App-Version-Code": cred.Field("ide_version_code"), "X-Ide-Version-Type": "stable", "X-Device-Type": "windows", "X-OS-Version": "Windows 11 Pro", "X-Device-Brand": "83DG", "Request-Traffic-Type": "prod", "X-Machine-Id": cred.Field("machine_id"), "X-Device-Id": cred.Field("device_id"), "X-User-Region": "CN"} {
		h.Set(k, v)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h
}

func (c *Client) FetchCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	var models []string
	_ = json.Unmarshal([]byte(cred.Field("models")), &models)
	if len(models) > 0 {
		out := []provider.ModelInfo{}
		for _, model := range models {
			out = append(out, provider.ModelInfo{NativeModel: model, PublicModel: model, DisplayName: model, Tools: c.family != "trae-intl"})
		}
		return out, nil
	}
	if cred.Field("access_token") == "" {
		return nil, provider.ErrNotReady
	}
	if c.family == "trae-intl" {
		return c.traeIntlCatalog(ctx, cred)
	}
	if c.family == "qoder" {
		return c.qoderCatalog(ctx, cred)
	}
	body, _ := json.Marshal(map[string]any{"function": cred.Field("scene"), "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil})
	response, err := c.do(ctx, cred, "POST", cred.Field("base_url")+traeModelsPath, body, traeHeaders(cred, false), false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var listing struct {
		Config []struct {
			Name    string `json:"config_name"`
			Hidden  bool   `json:"is_invisible_to_user"`
			Context struct {
				Dev int `json:"dev"`
			} `json:"context_window_tokens"`
			Display struct {
				Name       string `json:"display_name"`
				Capability string `json:"model_capability"`
			} `json:"display_config"`
			Reasoning struct {
				Supported bool     `json:"support_thinking"`
				Options   []string `json:"options"`
				Default   string   `json:"default_level"`
			} `json:"reasoning_effort_config"`
		} `json:"config_info_list"`
	}
	if err := decodeLimited(response.Body, &listing); err != nil {
		return nil, err
	}
	out := []provider.ModelInfo{}
	seen := map[string]bool{}
	for _, model := range listing.Config {
		if model.Hidden || model.Name == "" || seen[model.Name] {
			continue
		}
		seen[model.Name] = true
		out = append(out, provider.ModelInfo{NativeModel: model.Name, PublicModel: model.Name, DisplayName: model.Display.Name, ContextWindow: model.Context.Dev, Tools: true, Reasoning: model.Reasoning.Supported, ReasoningOptions: model.Reasoning.Options, ReasoningDefault: model.Reasoning.Default})
	}
	if len(out) == 0 {
		return nil, failure(502, "Trae 返回的聊天型号目录为空")
	}
	return out, nil
}
