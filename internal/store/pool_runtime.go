package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AccountRuntime 只包含健康状态，绝不保存凭证或运行中的并发计数。
type AccountRuntime struct {
	DownUntil      time.Time            `json:"-"`
	LastError      string               `json:"-"`
	LastKind       string               `json:"-"`
	BackoffLevel   int                  `json:"backoff_level,omitempty"`
	GroupDownUntil time.Time            `json:"group_cooldown,omitempty"`
	ModelDownUntil map[string]time.Time `json:"model_cooldowns,omitempty"`
	ModelBackoff   map[string]int       `json:"model_backoff,omitempty"`
	ModelLastKind  map[string]string    `json:"model_last_kind,omitempty"`
	ProvenModels   []string             `json:"proven_models,omitempty"`
}

func validQuotaScope(scope string) bool {
	return scope == "shared" || scope == "account" || scope == "model"
}

// 一条原子更新保存账号和模型健康，避免两部分写入中途断开。
func (s *Store) UpdatePoolRuntime(ctx context.Context, id string, state AccountRuntime) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("编码号池状态：%w", err)
	}
	status := "ready"
	if state.DownUntil.After(time.Now()) || state.GroupDownUntil.After(time.Now()) {
		status = "cooldown"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET cooldown_until=?, last_error=?, last_error_kind=?, status=?, pool_runtime=?, updated_at=? WHERE id=?`,
		formatTime(state.DownUntil), state.LastError, state.LastKind, status, string(raw), now(), id)
	if err != nil {
		return fmt.Errorf("保存账号 %s 的号池状态：%w", id, err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("账号 %s：%w", id, ErrNotFound)
	}
	return nil
}
