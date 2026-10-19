package compatible

// Endpoint 是已核验的官方地址；地区和套餐由用户选择，不根据密钥猜测。
type Endpoint struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Region string `json:"region"`
}

type Profile struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	DefaultBaseURL string     `json:"default_base_url"`
	DefaultRegion  string     `json:"default_region"`
	ModelsRequired bool       `json:"models_required"`
	Note           string     `json:"note"`
	Documentation  string     `json:"documentation"`
	Endpoints      []Endpoint `json:"endpoints"`
	KeyHeader      string     `json:"-"`
}

// 同一份配置用于注册适配器和生成控制台选项，避免前后端地址漂移。
func Profiles() []Profile {
	return []Profile{
		{ID: "deepseek", Name: "DeepSeek 官方 API", DefaultBaseURL: "https://api.deepseek.com", DefaultRegion: "cn", Note: "支持工具调用及思考内容回传；网页登录态尚未接入。", Documentation: "https://api-docs.deepseek.com/"},
		{ID: "grok", Name: "Grok / xAI", DefaultBaseURL: "https://api.x.ai/v1", DefaultRegion: "global", Note: "使用官方 Chat Completions 接口；原生 Responses 的内置工具暂不支持。", Documentation: "https://docs.x.ai/developers/rest-api-reference/inference/chat-completions", Endpoints: []Endpoint{{"全球", "https://api.x.ai/v1", "global"}, {"美国", "https://us.api.x.ai/v1", "global"}}},
		{ID: "qwen", Name: "Qwen / 阿里云百炼", DefaultBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", DefaultRegion: "cn", Note: "密钥须与地址同地区；推荐业务空间专属域名。自动目录来自百炼模型查询接口，不代表该密钥已有全部模型权限。", Documentation: "https://help.aliyun.com/zh/model-studio/compatibility-of-openai-with-dashscope", Endpoints: []Endpoint{{"北京（现有地址）", "https://dashscope.aliyuncs.com/compatible-mode/v1", "cn"}, {"北京（业务空间）", "https://{workspace_id}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "cn"}, {"新加坡（业务空间）", "https://{workspace_id}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", "global"}, {"东京（业务空间）", "https://{workspace_id}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1", "global"}, {"美国弗吉尼亚", "https://dashscope-us.aliyuncs.com/compatible-mode/v1", "global"}}},
		{ID: "glm", Name: "GLM / 智谱 / Z.AI", DefaultBaseURL: "https://open.bigmodel.cn/api/paas/v4", DefaultRegion: "cn", ModelsRequired: true, Note: "填写实际模型 ID；普通 API 与 Coding 套餐地址、额度分别配置。thinking 和 reasoning_content 原样回传。", Documentation: "https://docs.bigmodel.cn/cn/api/introduction", Endpoints: []Endpoint{{"国内普通 API", "https://open.bigmodel.cn/api/paas/v4", "cn"}, {"海外普通 API", "https://api.z.ai/api/paas/v4", "global"}, {"国内 Coding 套餐", "https://open.bigmodel.cn/api/coding/paas/v4", "cn"}, {"海外 Coding 套餐", "https://api.z.ai/api/coding/paas/v4", "global"}}},
		{ID: "kimi", Name: "Kimi / Moonshot", DefaultBaseURL: "https://api.moonshot.cn/v1", DefaultRegion: "cn", Note: "使用开放平台 API 密钥；Kimi 网页会员和 Kimi Coding 登录态不等于本渠道凭证。保留工具轮次思考内容。", Documentation: "https://platform.kimi.com/docs/get-api-key"},
		{ID: "doubao", Name: "豆包 / 火山方舟", DefaultBaseURL: "https://ark.cn-beijing.volces.com/api/v3", DefaultRegion: "cn", ModelsRequired: true, Note: "填写控制台可调用的模型 ID 或推理接入点 ep-ID；本渠道支持 Chat API，桌面会员登录态尚未接入。", Documentation: "https://docs.volcengine.com/docs/ark/chat-api"},
		{ID: "hunyuan", Name: "混元 / 腾讯 TokenHub", DefaultBaseURL: "https://tokenhub.tencentmaas.com/v1", DefaultRegion: "cn", Note: "旧混元平台已公告迁移；使用 TokenHub API Key，不能填云 API SecretId / SecretKey。国内与海外额度分开。", Documentation: "https://cloud.tencent.com/document/product/1823/130078", Endpoints: []Endpoint{{"广州", "https://tokenhub.tencentmaas.com/v1", "cn"}, {"新加坡", "https://tokenhub-intl.tencentmaas.com/v1", "global"}}},
		{ID: "mimo", Name: "MiMo / 小米", DefaultBaseURL: "https://api.xiaomimimo.com/v1", DefaultRegion: "cn", ModelsRequired: true, KeyHeader: "api-key", Note: "支持独立 api-key 鉴权及思考内容回放；按量 API 与 Token Plan 使用各自密钥。填写实际聊天模型 ID。", Documentation: "https://mimo.mi.com/docs/en-US/quick-start/summary/first-api-call", Endpoints: []Endpoint{{"按量 API", "https://api.xiaomimimo.com/v1", "cn"}, {"Token Plan", "https://token-plan-cn.xiaomimimo.com/v1", "cn"}}},
	}
}

func profileFor(id string) Profile {
	for _, p := range Profiles() {
		if p.ID == id {
			return p
		}
	}
	return Profile{ID: id}
}
