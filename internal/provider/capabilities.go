package provider

import (
	"context"
	"time"
)

// CheckinCapability 区分每日签到、活动领取以及平台没有签到的情况。
type CheckinCapability struct {
	Supported    bool   `json:"supported"`
	Mode         string `json:"mode"`
	Label        string `json:"label"`
	Note         string `json:"note"`
	Timezone     string `json:"timezone,omitempty"`
	DayStartHour int    `json:"day_start_hour"`
}

func CheckinFor(family, region string) CheckinCapability {
	c := CheckinCapability{Mode: "none", Label: "无需签到", Note: "此渠道没有已接入的签到接口"}
	switch family {
	case "workbuddy":
		c = CheckinCapability{Supported: true, Mode: "daily", Label: "每日签到", Note: "领取平台当日签到奖励；以平台返回结果为准", Timezone: "UTC+8"}
	case "qoder":
		c = CheckinCapability{Supported: true, Mode: "campaign", Label: "领取活动奖励", Note: "仅领取当前可领取的积分活动；活动资格和设备校验由 Qoder 决定，每日窗口从北京时间 10 点开始", Timezone: "UTC+8", DayStartHour: 10}
	case "trae":
		if region != "global" {
			c = CheckinCapability{Supported: true, Mode: "credits", Label: "领取每日积分", Note: "先查询资格，领取后复核签到状态；原登录设备信息必须有效", Timezone: "UTC+8"}
		}
	case "trae-intl":
		c.Note = "Trae 国际版使用独立权益体系，未确认可用的每日签到接口"
	}
	return c
}

// CheckinPeriod 按平台的时区和重置时间去重，不依赖部署机器的时区。
func CheckinPeriod(t time.Time, capability CheckinCapability) string {
	if capability.Timezone == "UTC+8" {
		t = t.In(time.FixedZone("UTC+8", 8*3600))
	}
	return t.Add(-time.Duration(capability.DayStartHour) * time.Hour).Format("2006-01-02")
}

func BrowserLoginSupported(family string) bool {
	switch family {
	case "workbuddy", "qoder", "trae", "trae-intl", "codex", "gemini", "claude":
		return true
	}
	return false
}

// BrowserLoginOptions 中的私密配置仅存于服务端，完成后随凭证加密保存。
type BrowserLoginOptions struct {
	Region       string   `json:"region"`
	RedirectURI  string   `json:"redirect_uri"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	ProjectID    string   `json:"project_id"`
	Models       []string `json:"models"`
}

type BrowserLoginSession struct {
	Family, Region, AuthURL, Mode, UserCode            string
	State, Verifier, DeviceAuthID, MachineID, DeviceID string
	Options                                            BrowserLoginOptions
	ExpiresAt                                          time.Time
	PollInterval                                       time.Duration
	NextPoll                                           time.Time
	// Raw 仅供已核验回调交给管理端轮询保存，不向浏览器返回。
	Raw []byte
	// 已兑换的令牌仅存服务端内存，身份查询暂时失败时不重复消费授权码。
	Exchanged []byte
}

type BrowserLoginAdapter interface {
	BeginBrowserLogin(context.Context, BrowserLoginOptions) (*BrowserLoginSession, error)
	PollBrowserLogin(context.Context, *BrowserLoginSession) ([]byte, bool, error)
	CompleteBrowserLogin(context.Context, *BrowserLoginSession, string) ([]byte, error)
}
