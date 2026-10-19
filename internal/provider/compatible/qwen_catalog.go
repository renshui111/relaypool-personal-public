package compatible

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

// 百炼的模型目录是独立路径和分页格式；在同一主机查询，不将密钥发给其他地区。
func (c *Client) fetchQwenCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	fields := make(map[string]string, len(cred.Fields))
	for key, value := range cred.Fields {
		fields[key] = value
	}
	fields["base_url"] = strings.TrimSuffix(cred.Field("base_url"), "/compatible-mode/v1")
	cred.Fields = fields
	out := []provider.ModelInfo{}
	seen := map[string]bool{}
	for page := 1; page <= 32; page++ {
		response, err := c.request(ctx, cred, "GET", "/api/v1/models?page_no="+strconv.Itoa(page)+"&page_size=100&capabilities=TG&capabilities=Reasoning", nil, false)
		if err != nil {
			return nil, err
		}
		var listing struct {
			Success *bool `json:"success"`
			Output  struct {
				Total  int `json:"total"`
				Models []struct {
					Model    string   `json:"model"`
					Name     string   `json:"name"`
					Features []string `json:"features"`
				} `json:"models"`
			} `json:"output"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&listing)
		response.Body.Close()
		if err != nil || listing.Output.Models == nil || (listing.Success != nil && !*listing.Success) {
			return nil, &provider.Failure{Status: 502, Message: "百炼模型目录格式无效"}
		}
		for _, model := range listing.Output.Models {
			if model.Model == "" || seen[model.Model] {
				continue
			}
			seen[model.Model] = true
			tools := false
			for _, feature := range model.Features {
				if feature == "function-calling" {
					tools = true
				}
			}
			out = append(out, provider.ModelInfo{NativeModel: model.Model, PublicModel: model.Model, DisplayName: model.Name, Tools: tools})
		}
		if page*100 >= listing.Output.Total {
			return out, nil
		}
		if len(listing.Output.Models) == 0 {
			return nil, &provider.Failure{Status: 502, Message: "百炼模型目录分页不完整"}
		}
	}
	return nil, &provider.Failure{Status: 502, Message: "百炼模型目录超过分页上限，请配置实际模型列表"}
}
