package api

import (
	"context"
	"github.com/relaypool/relaypool/internal/store"
	"log"
	"strings"
	"time"
)

// readLegacyArmorEnabled 仅迁移旧密钥时读取历史设置，不能控制现有请求。
func readLegacyArmorEnabled(st *store.Store, fromConfig bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	value, err := st.GetSetting(ctx, store.SettingArmorEnabled)
	if err == store.ErrSettingNotFound {
		return fromConfig
	}
	if err != nil {
		log.Printf("读取旧破甲状态失败：%v", err)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "on", "yes":
		return true
	case "false", "0", "off", "no":
		return false
	}
	return false
}
