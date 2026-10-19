package api

import (
	"fmt"
	"github.com/relaypool/relaypool/internal/provider"
)

// 凭证类型以实际解码结果为准，避免把 API 密钥存成 OAuth、误导后续刷新流程。
func credentialAuthType(cred provider.Credential) string {
	if cred.Field("api_key") != "" {
		return "api_key"
	}
	return "oauth"
}

func validateAuthType(value string, cred provider.Credential) error {
	if value != "" && value != credentialAuthType(cred) {
		return fmt.Errorf("凭证类型与实际内容不一致，请分别创建 API 或登录态账号")
	}
	return nil
}
