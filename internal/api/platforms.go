package api

import (
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
	"net/http"
)

// 能力清单只展示已经注册的渠道，登录态与官方 API 分开说明。
func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	entries := []map[string]any{
		{"id": "trae-intl", "name": "Trae 国际远程会话", "auth_types": []string{"oauth"}, "default_region": "global", "regions": []string{"global"}, "note": "导入本人 Trae 国际完整登录态（access_token、uid，可选 refresh_token、原 web_id、referer_origin、timezone）。通过远程会话协议提供文本回复，不自动创建项目。当前不支持客户端函数工具、图片、采样或思考控制；创建成功后不自动重复换号。", "documentation": "https://www.trae.ai"},
		{"id": "qoder", "name": "Qoder 登录态", "auth_types": []string{"oauth"}, "default_region": "cn", "regions": []string{"cn", "global"}, "note": "导入本人 Qoder 完整账户 JSON（account/auth），或填写 personal_token 与 region；直接调用桌面协议，无需额外运行 CLI。型号填写 Qoder 实际 key，例如 auto。原生 CLI 加密凭证不可直接导入。", "documentation": "https://qoder.com"},
		{"id": "trae", "name": "Trae 国内聊天登录态", "auth_types": []string{"oauth"}, "default_region": "cn", "regions": []string{"cn"}, "note": "导入本人国内完整账户 JSON：access_token、uid、machine_id、device_id；保留原设备信息。可选 variant=solo/cn、scene=solo_work_lite/chat_v3。国际版请选择 Trae 国际远程会话渠道。", "documentation": "https://www.trae.cn"},
		{"id": "workbuddy", "name": "WorkBuddy 订阅", "auth_types": []string{"oauth"}, "browser_login": true, "default_base_url": "", "note": "保留原有国内、海外浏览器授权。"},
		{"id": "claude", "name": "Claude / Anthropic", "auth_types": []string{"api_key", "oauth"}, "default_base_url": "https://api.anthropic.com/v1", "note": "支持官方 API、浏览器授权和本人 Claude Code 登录态导入；本机接收回调，局域网和远程可回填授权码，登录态加密保存并自动续期。模型权限与用量以该账号上游返回为准。"},
		{"id": "gemini", "name": "Gemini / Google", "auth_types": []string{"api_key", "oauth"}, "default_base_url": "https://generativelanguage.googleapis.com/v1beta", "note": "官方 API 或 Code Assist 登录态；Code Assist 需要 project_id，自动刷新需要对应 OAuth 客户端配置。"},
		{"id": "openai", "name": "OpenAI 官方 API", "auth_types": []string{"api_key"}, "default_base_url": "https://api.openai.com/v1", "note": "通过原生 Responses API 接入；ChatGPT 订阅请选择 Codex。"},
		{"id": "codex", "name": "ChatGPT / Codex 登录态", "auth_types": []string{"oauth"}, "default_base_url": "https://chatgpt.com/backend-api/codex", "note": "导入本人 Codex auth.json；填写实际可用模型；不是 ChatGPT 网页会话接口。"},
		{"id": "openai-compatible", "name": "其他 OpenAI 兼容服务", "auth_types": []string{"api_key"}, "default_base_url": "", "note": "适用于本人配置的兼容服务或独立登录态网关，基础地址需含接口前缀。"},
	}
	for _, p := range compatible.Profiles() {
		entries = append(entries, map[string]any{"id": p.ID, "name": p.Name, "auth_types": []string{"api_key"}, "default_base_url": p.DefaultBaseURL, "default_region": p.DefaultRegion, "models_required": p.ModelsRequired, "note": p.Note, "documentation": p.Documentation, "endpoints": p.Endpoints})
	}
	out := []map[string]any{}
	for _, entry := range entries {
		id := entry["id"].(string)
		entry["browser_login"] = provider.BrowserLoginSupported(id)
		entry["checkin"] = provider.CheckinFor(id, "cn")
		entry["checkin_by_region"] = map[string]any{"cn": provider.CheckinFor(id, "cn"), "global": provider.CheckinFor(id, "global")}
		if id == "gemini" || id == "codex" || id == "trae-intl" || id == "claude" {
			entry["regions"] = []string{"global"}
			entry["default_region"] = "global"
		}
		if id == "workbuddy" {
			entry["regions"] = []string{"cn", "global"}
			entry["default_region"] = "cn"
		}
		if id == "gemini" {
			entry["browser_login_note"] = "需要自己的 Google OAuth 客户端 ID、对应密钥（如需）及 Code Assist 项目 ID"
		}
		if id == "claude" {
			entry["browser_login_note"] = "在本人浏览器同意授权，本机可自动保存；远程部署请复制完整授权码（包含 # 后的状态）。可配置获准的自有 OAuth 客户端与注册回调地址。"
		}
		if s.registry.Has(entry["id"].(string)) {
			out = append(out, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
