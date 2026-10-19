package provider

import (
	"testing"
	"time"
)

func TestPlatformCheckinBoundary(t *testing.T) {
	qoder := CheckinFor("qoder", "global")
	before := time.Date(2026, 10, 10, 1, 59, 0, 0, time.UTC)
	after := before.Add(time.Minute)
	if CheckinPeriod(before, qoder) == CheckinPeriod(after, qoder) {
		t.Fatal("Qoder 的北京时间 10 点没有开启新领取周期")
	}
	if CheckinPeriod(before, qoder) != CheckinPeriod(time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC), qoder) {
		t.Fatal("Qoder 10 点前未归入上一周期")
	}
	if CheckinFor("trae-intl", "global").Supported || CheckinFor("claude", "global").Supported || CheckinFor("openai-compatible", "cn").Supported {
		t.Fatal("为没有签到的平台开放了签到")
	}
	if !CheckinFor("trae", "cn").Supported || !CheckinFor("workbuddy", "global").Supported {
		t.Fatal("已接入签到未声明")
	}
}
