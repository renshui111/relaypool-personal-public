package config

import "testing"

func TestReviewPoolConfigurationValidation(t *testing.T) {
	clearEnv(t)
	for _, poolJSON := range []string{
		`{"max_attempts":0}`, `{"max_attempts":9}`, `{"max_inflight":-1}`,
		`{"breaker_cooldown":"0s"}`, `{"breaker_cooldown_max":"0s"}`,
		`{"default_cooldown":"-1s"}`, `{"min_rate_limit_cooldown":"11m"}`,
	} {
		if _, err := Load(writeConfig(t, `{"pool":`+poolJSON+`}`)); err == nil {
			t.Fatalf("错误号池配置未被拒绝：%s", poolJSON)
		}
	}
	cfg, err := Load(writeConfig(t, `{"pool":{"max_attempts":2,"max_inflight":0}}`))
	if err != nil || cfg.Pool.MaxAttempts != 2 || cfg.Pool.MaxInflight != 0 {
		t.Fatalf("合法零并发配置未正确读取：%+v，%v", cfg.Pool, err)
	}
}
