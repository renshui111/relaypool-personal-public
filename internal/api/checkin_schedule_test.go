package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 页面展示实际后台计划，不能把默认时刻误当成部署者修改后的配置。
func TestCheckinScheduleReportsConfiguredHours(t *testing.T) {
	h, s := newTestServer(t, newStubProvider())
	s.cfg.Schedule.CheckinEnabled = false
	s.cfg.Schedule.CheckinHours = []int{10, 22}
	resp, err := http.DefaultClient.Do(authRequest(t, "GET", h.URL+"/admin/checkin/schedule", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var value struct {
		Enabled   bool   `json:"enabled"`
		Hours     []int  `json:"hours"`
		Timezone  string `json:"timezone"`
		UTCOffset string `json:"utc_offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || value.Enabled || len(value.Hours) != 2 || value.Hours[0] != 10 || value.Hours[1] != 22 || value.Timezone == "" || value.UTCOffset == "" {
		t.Fatal("领取计划没有反映实际配置或时区")
	}
}

func TestCheckinScheduleRejectsAnonymous(t *testing.T) {
	h, _ := newTestServer(t, newStubProvider())
	resp, err := http.Get(h.URL + "/admin/checkin/schedule")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("匿名请求获得了管理数据")
	}
}
