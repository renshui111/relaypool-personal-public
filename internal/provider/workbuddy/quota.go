package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func (*Client) SupportsQuota() bool { return true }

type resourcePackage struct {
	Remain      *float64 `json:"CapacityRemain"`
	Used        *float64 `json:"CapacityUsed"`
	Size        *float64 `json:"CapacitySize"`
	CycleRemain *float64 `json:"CycleCapacityRemain"`
	CycleUsed   *float64 `json:"CycleCapacityUsed"`
	CycleSize   *float64 `json:"CycleCapacitySize"`
}

// FetchQuota 查询有效积分包，周期包和固定包分别取值，分页不足时拒绝报假总额。
func (c *Client) FetchQuota(ctx context.Context, req provider.Credential) (provider.QuotaSnapshot, error) {
	cred, err := c.decodeCredential(req)
	if err != nil || !cred.Ready() {
		return provider.QuotaSnapshot{}, provider.ErrNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	clock := now
	if cred.Region() == provider.RegionCN {
		clock = now.In(quotaResetLocation)
	}
	remaining, used, total := 0.0, 0.0, 0.0
	allUsed, allTotal := true, true
	var lastPage []byte
	for page := 1; page <= 20; page++ {
		payload, _ := json.Marshal(map[string]any{"PageNumber": page, "PageSize": 100, "ProductCode": "p_tcaca", "Status": []int{0, 3}, "PackageEndTimeRangeBegin": clock.Format("2006-01-02 15:04:05"), "PackageEndTimeRangeEnd": clock.AddDate(101, 0, 0).Format("2006-01-02 15:04:05")})
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cred.BillingBase(), "/")+pathUserResource, bytes.NewReader(payload))
		if err != nil {
			return provider.QuotaSnapshot{}, fmt.Errorf("无法构造额度查询")
		}
		SetBillingHeaders(r.Header, cred)
		if c.cfg.UserAgent != "" {
			r.Header.Set("User-Agent", c.cfg.UserAgent)
		}
		client := *c.http
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		response, err := client.Do(r)
		if err != nil {
			return provider.QuotaSnapshot{}, fmt.Errorf("额度上游连接失败")
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return provider.QuotaSnapshot{}, fmt.Errorf("额度查询被上游拒绝（%d）", response.StatusCode)
		}
		var envelope struct {
			Code *int `json:"code"`
			Data struct {
				Response struct {
					Data struct {
						Accounts json.RawMessage `json:"Accounts"`
					} `json:"Data"`
				} `json:"Response"`
			} `json:"data"`
		}
		if readErr != nil || len(raw) > 4<<20 || json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 {
			return provider.QuotaSnapshot{}, fmt.Errorf("额度响应无效")
		}
		accounts := envelope.Data.Response.Data.Accounts
		var packages []resourcePackage
		if len(accounts) == 0 || string(accounts) == "null" || json.Unmarshal(accounts, &packages) != nil {
			return provider.QuotaSnapshot{}, fmt.Errorf("额度响应缺少积分包，不能确定剩余值")
		}
		if page > 1 && len(packages) > 0 && bytes.Equal(lastPage, accounts) {
			return provider.QuotaSnapshot{}, fmt.Errorf("额度接口未正确分页，不能确定总额")
		}
		lastPage = append(lastPage[:0], accounts...)
		for _, item := range packages {
			r, u, t := item.Remain, item.Used, item.Size
			if (item.CycleSize != nil && *item.CycleSize > 0) || (item.CycleRemain != nil && *item.CycleRemain > 0) || (item.CycleUsed != nil && *item.CycleUsed > 0) {
				r, u, t = item.CycleRemain, item.CycleUsed, item.CycleSize
			}
			for _, value := range []*float64{r, u, t} {
				if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || math.Abs(*value) > 1e15) {
					return provider.QuotaSnapshot{}, fmt.Errorf("额度计数无效")
				}
			}
			if r == nil && u != nil && t != nil {
				value := math.Max(0, *t-*u)
				r = &value
			}
			if r == nil {
				return provider.QuotaSnapshot{}, fmt.Errorf("积分包未上报剩余值")
			}
			remaining += math.Max(0, *r)
			if t != nil {
				total += math.Max(0, *t)
			} else {
				allTotal = false
			}
			if u == nil && t != nil {
				value := math.Max(0, *t-*r)
				u = &value
			}
			if u != nil {
				used += math.Max(0, *u)
			} else {
				allUsed = false
			}
			if remaining > 1e15 || used > 1e15 || total > 1e15 {
				return provider.QuotaSnapshot{}, fmt.Errorf("积分包合计超出有效范围")
			}
		}
		if len(packages) < 100 {
			amount := provider.QuotaAmount{Label: "有效积分包合计", Unit: "积分", Remaining: &remaining}
			if allUsed {
				amount.Used = &used
			}
			if allTotal {
				amount.Total = &total
			}
			return provider.QuotaSnapshot{State: "ok", Amounts: []provider.QuotaAmount{amount}, CheckedAt: time.Now().UTC(), Message: "平台有效积分包，积分不等于 Token；套餐限制仍以上游为准"}, nil
		}
	}
	return provider.QuotaSnapshot{}, fmt.Errorf("积分包超过查询上限，不能确定完整额度")
}
