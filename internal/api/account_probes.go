package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// 固定账号的上下文只由管理路由设置，普通调用头不能开启故障检查。
type pinnedAccountContextKey struct{}

func (s *Server) handleAccountChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	item, ok := s.pool.ByID(id)
	if !ok {
		writeError(w, 404, "account_not_found", "账号不存在")
		return
	}
	if !item.Enabled {
		writeError(w, 409, "account_disabled", "该账号已手动停用，请先启用或使用账号测试")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeError(w, 400, "invalid_request", "消息格式无效")
		return
	}
	if err = validateChatRequest(req); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	// 界面传入裸模型；带渠道前缀时必须与指定账号一致。
	if family, model, found := strings.Cut(req.Model, "/"); found && s.registry.Has(family) {
		if family != item.Provider {
			writeError(w, 400, "model_provider_mismatch", "模型渠道与所选账号不一致")
			return
		}
		req.Model = model
	}
	r = r.WithContext(context.WithValue(r.Context(), pinnedAccountContextKey{}, id))
	s.serveChat(w, r, req, protocolChatCompletions)
}

type accountTestResult struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Model              string `json:"model,omitempty"`
	Status             string `json:"status"`
	Message            string `json:"message"`
	Enabled            bool   `json:"enabled"`
	Recovered          bool   `json:"recovered"`
	LatencyMS          int64  `json:"latency_ms"`
	OutputLimitApplied bool   `json:"output_limit_applied"`
}

// 批量检查每号仅发一条短消息；两路并发、逐项返回、断开即取消。
func (s *Server) handleTestAccounts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs   []string `json:"ids"`
		Model string   `json:"model"`
	}
	if err := decodeJSONBody(w, r, &input, 32<<10); err != nil {
		writeError(w, 400, "invalid_request", "测试参数无效")
		return
	}
	if len(input.IDs) > 1000 || len(input.Model) > 256 {
		writeError(w, 400, "invalid_request", "单次最多检查 1000 个账号，模型名称不能超过 256 字节")
		return
	}
	if !s.accountTestMu.TryLock() {
		writeError(w, 409, "test_running", "已有账号测试正在运行，请等待或停止后重试")
		return
	}
	defer s.accountTestMu.Unlock()
	items := s.pool.Items()
	selected := map[string]bool{}
	for _, id := range input.IDs {
		selected[id] = true
	}
	if len(selected) > 0 {
		filtered := make([]pool.Item, 0, len(selected))
		for _, item := range items {
			if selected[item.ID] {
				filtered = append(filtered, item)
				delete(selected, item.ID)
			}
		}
		if len(selected) > 0 {
			writeError(w, 404, "account_not_found", "选择的账号已不存在，请刷新列表")
			return
		}
		items = filtered
	}
	if len(items) > 1000 {
		writeError(w, 400, "too_many_accounts", "请选择不超过 1000 个账号分批检查")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming_unsupported", "当前服务不支持实时返回测试结果")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(value any) bool {
		data, _ := json.Marshal(value)
		// 写入期限防止断开的慢客户端占住整轮测试。
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		if err != nil {
			cancel()
			return false
		}
		flusher.Flush()
		return true
	}
	defer func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }()
	if !send(map[string]any{"type": "start", "total": len(items)}) {
		return
	}
	jobs := make(chan pool.Item)
	results := make(chan accountTestResult, 2)
	var workers sync.WaitGroup
	for n := 0; n < 2; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := s.testAccount(ctx, item, strings.TrimSpace(input.Model))
				select {
				case results <- result:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
			select {
			case jobs <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	// 每十秒发送心跳，使公网代理与客户端能区分等待和断线。
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case result, open := <-results:
			if !open {
				send(map[string]any{"type": "done"})
				return
			}
			if !send(map[string]any{"type": "result", "result": result}) {
				workers.Wait()
				return
			}
		case <-heartbeat.C:
			if !send(map[string]any{"type": "heartbeat"}) {
				workers.Wait()
				return
			}
		case <-ctx.Done():
			workers.Wait()
			return
		}
	}
}

func (s *Server) testAccount(parent context.Context, original pool.Item, model string) accountTestResult {
	started := time.Now()
	result := accountTestResult{ID: original.ID, Name: original.Name, Enabled: original.Enabled, Status: "failed"}
	key, hasKey := parent.Value(keyPermissionsContextKey{}).(store.APIKey)
	if hasKey && !key.Permissions.AllowsAccount(original.ID) {
		result.Status = "skipped"
		result.Message = "当前密钥未获准测试此账号"
		return result
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	if model == "" {
		for _, candidate := range original.Models {
			if strings.TrimSpace(candidate) != "" && (!hasKey || s.keyAllowsModel(key.Permissions, candidate, original.Provider)) {
				model = strings.TrimSpace(candidate)
				break
			}
		}
	}
	if model == "" {
		adapter, err := s.registry.Lookup(original.Provider)
		if err == nil {
			cred, err := (credentialSource{store: s.store, registry: s.registry, OnRefreshed: s.pool.ResetCredentialHealth}).CredentialForContext(ctx, original.ID)
			if err != nil {
				result.Message = "缺少有效凭证，请更新登录态或 API 密钥"
				return result
			}
			if err == nil {
				catalog, err := adapter.FetchCatalog(ctx, cred)
				if err == nil {
					for _, m := range catalog {
						if m.NativeModel != "" && (!hasKey || s.keyAllowsModel(key.Permissions, m.NativeModel, original.Provider)) {
							model = m.NativeModel
							break
						}
					}
				}
			}
		}
	}
	if family, native, found := strings.Cut(model, "/"); found && s.registry.Has(family) {
		if family != original.Provider {
			result.Status = "skipped"
			result.Message = "指定模型的渠道与本账号不一致"
			return result
		}
		model = native
	}
	if model == "" {
		result.Status = "skipped"
		result.Message = "没有可用模型，请在导入凭证时填写模型，或指定本轮测试模型"
		return result
	}
	if hasKey && !s.keyAllowsModel(key.Permissions, model, original.Provider) {
		result.Status = "skipped"
		result.Message = "当前密钥未获准调用此型号"
		return result
	}
	result.Model = model
	text, _ := json.Marshal("请只回复 OK")
	maxTokens := 128
	chat := &provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: text}}, StrictTokenLimit: true}
	body := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "请只回复 OK"}}, "stream": false}
	// 两个订阅协议不接受输出上限；短消息检查省去该参数，普通聊天仍严格校验。
	if original.Provider != "codex" && original.Provider != "trae-intl" {
		chat.MaxTokens = &maxTokens
		body["max_tokens"] = maxTokens
		result.OutputLimitApplied = true
	}
	chat.Raw, _ = json.Marshal(body)
	response, err := s.executor.ChatNonStream(ctx, executor.Request{Chat: chat, OnlyAccount: original.ID, RecoveryProbe: true})
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		// 只展示分类，不把上游任意错误正文、认证请求头或凭证回传浏览器。
		var failure *executor.FailureError
		if errors.As(err, &failure) {
			result.Message = testFailureMessage(failure.Kind)
			if failure.Code == "pool_saturated" {
				result.Message = "账号并发名额暂不可用，请等待正在进行的请求结束后重试"
			}
		} else {
			result.Message = "上游请求失败，请检查网络和账号配置"
		}
		if ctx.Err() != nil {
			result.Message = "测试已取消或超过 60 秒，账号状态未因取消而降级"
		}
		return result
	}
	if response == nil || response.Chat == nil {
		result.Message = "上游没有返回有效消息"
		return result
	}
	result.Status = "success"
	result.Message = "账号已返回文本消息"
	if !result.OutputLimitApplied {
		result.Message += "；此渠道不支持输出上限，测试仅受 60 秒超时限制"
	}
	if current, ok := s.pool.ByID(original.ID); ok {
		result.Recovered = current.Ready != nil && *current.Ready && current.LastKind == "" && !current.AccountDownUntil().After(time.Now())
		if !result.Recovered {
			result.Message += "；状态或凭证已变化，未覆盖较新的故障，请重新检查"
		}
	}
	if !original.Enabled {
		result.Message += "；保留手动停用状态"
	}
	return result
}

func testFailureMessage(kind string) string {
	switch kind {
	case pool.KindAuth, pool.KindNotReady:
		return "认证未通过，请更新登录态或 API 密钥"
	case pool.KindQuota:
		return "上游额度不足，请检查套餐和余额"
	case pool.KindRateLimit:
		return "上游限流，请稍后重试"
	case pool.KindModelNotAvailable:
		return "该账号不能使用此模型，请更换测试模型"
	case pool.KindInvalidRequest:
		return "上游拒绝测试参数，请核对渠道和模型配置"
	default:
		return "上游不可用或没有返回文本，请检查网络与渠道配置"
	}
}
