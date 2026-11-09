package workbuddy

import (
	"encoding/json"
	"fmt"
	"time"
)

// decodeTokenData is shared by login and refresh so expiry conventions cannot
// drift. Errors deliberately omit upstream values, which may contain secrets.
func decodeTokenData(data []byte, now time.Time) (Credential, error) {
	token, err := DecodeCredential(data)
	if err != nil {
		return Credential{}, fmt.Errorf("WorkBuddy 令牌响应缺少有效登录态")
	}
	var expiry struct {
		ExpiresIn      int64 `json:"expiresIn"`
		ExpiresInSnake int64 `json:"expires_in"`
		CreatedAt      int64 `json:"created_at"`
		CreatedAtCamel int64 `json:"createdAt"`
	}
	if json.Unmarshal(data, &expiry) != nil {
		return Credential{}, fmt.Errorf("WorkBuddy 令牌有效期格式无效")
	}
	if token.ExpiresAt < 0 {
		return Credential{}, fmt.Errorf("WorkBuddy 令牌有效期无效")
	}
	if token.ExpiresAt > 0 {
		token.ExpiresAt = token.Expiry().Unix()
	}
	if token.ExpiresAt == 0 {
		seconds := firstNonZero(expiry.ExpiresIn, expiry.ExpiresInSnake)
		if seconds < 0 || seconds > 366*24*60*60 {
			return Credential{}, fmt.Errorf("WorkBuddy 令牌有效期无效")
		}
		if seconds > 0 {
			created := firstNonZero(expiry.CreatedAtCamel, expiry.CreatedAt)
			if created > 1e12 {
				created /= 1000
			}
			if created <= 0 {
				created = now.Unix()
			}
			token.ExpiresAt = created + seconds
		}
	}
	return token, nil
}

func decodeRefreshResponse(raw []byte, now time.Time) (Credential, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return Credential{}, fmt.Errorf("WorkBuddy 续期响应格式无效")
	}
	if codeRaw, ok := obj["code"]; ok {
		var code *int
		if json.Unmarshal(codeRaw, &code) != nil || code == nil {
			return Credential{}, fmt.Errorf("WorkBuddy 续期响应状态无效")
		}
		if *code != 0 {
			return Credential{}, newFailure(200, "auth", fmt.Sprint(*code), "WorkBuddy 续期未获允许，请检查登录状态", nil)
		}
		if data, ok := obj["data"]; ok {
			return decodeTokenData(data, now)
		}
	}
	return decodeTokenData(raw, now)
}
