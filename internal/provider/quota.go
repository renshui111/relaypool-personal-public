package provider

import (
	"context"
	"time"
)

// QuotaAmount 保留平台计量单位；缺失值不当成零，货币不当成 Token。
type QuotaAmount struct {
	Label     string   `json:"label"`
	Unit      string   `json:"unit"`
	Remaining *float64 `json:"remaining,omitempty"`
	Used      *float64 `json:"used,omitempty"`
	Total     *float64 `json:"total,omitempty"`
}

type QuotaSnapshot struct {
	State     string        `json:"state"`
	Amounts   []QuotaAmount `json:"amounts,omitempty"`
	CheckedAt time.Time     `json:"checked_at,omitzero"`
	Message   string        `json:"message,omitempty"`
}

// QuotaProvider 为可选能力，余额查询失败不得改变聊天可用性或人工启停。
type QuotaProvider interface {
	SupportsQuota() bool
	FetchQuota(context.Context, Credential) (QuotaSnapshot, error)
}
