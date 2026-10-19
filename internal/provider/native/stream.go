package native

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// 转换器按客户端读取节奏拉取，关闭直接释放原响应，不启动额外后台协程。
type translatedStream struct {
	body               io.ReadCloser
	scanner            *bufio.Scanner
	family, model, id  string
	cred               provider.Credential
	pending            []byte
	terminal, complete bool
	usage              provider.Usage
	tools              map[int]int
	nextTool           int
	hasTools           bool
}

func newTranslatedStream(body io.ReadCloser, family, model string, cred provider.Credential) io.ReadCloser {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	return &translatedStream{body: body, scanner: scanner, family: family, model: model, id: "chatcmpl-native-" + strconv.FormatInt(time.Now().UnixNano(), 10), cred: cred, tools: map[int]int{}}
}
func (s *translatedStream) Close() error { return s.body.Close() }
func (s *translatedStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.terminal {
			return 0, io.EOF
		}
		raw, err := nextEvent(s.scanner)
		if err != nil {
			if err == io.EOF && s.complete {
				s.pending = []byte("data: [DONE]\n\n")
				s.terminal = true
				break
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			return 0, &provider.Failure{Status: 502, Message: "平台流包含无效 JSON"}
		}
		if errorValue, ok := event["error"]; ok {
			return 0, &provider.Failure{Status: 502, Message: "平台流返回错误", Body: cleanError(string(marshal(errorValue)), s.cred)}
		}
		frames, err := s.convert(event)
		if err != nil {
			return 0, err
		}
		// 中立帧之外保留同协议事件，网关会在其他协议的出口过滤此内部帧。
		if s.family == "claude" || s.family == "openai" || s.family == "codex" {
			meta := map[string]any{"_relaypool_native_event": event}
			s.pending = append(append(append([]byte("data: "), marshal(meta)...), []byte("\n\n")...), frames...)
		} else {
			s.pending = frames
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func nextEvent(scanner *bufio.Scanner) ([]byte, error) {
	var lines [][]byte
	size := 0
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if len(lines) > 0 {
				return bytes.Join(lines, []byte("\n")), nil
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		size += len(value)
		if size > 16<<20 {
			return nil, fmt.Errorf("平台 SSE 事件超过 16 MB")
		}
		lines = append(lines, append([]byte{}, value...))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		return bytes.Join(lines, []byte("\n")), nil
	}
	return nil, io.EOF
}
func (s *translatedStream) chunk(delta map[string]any, reason string, withUsage bool) []byte {
	payload := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if reason != "" {
		choice["finish_reason"] = reason
	}
	payload["choices"] = []any{choice}
	if withUsage {
		payload["usage"] = map[string]any{"prompt_tokens": s.usage.PromptTokens, "completion_tokens": s.usage.CompletionTokens, "total_tokens": s.usage.TotalTokens}
	}
	return append(append([]byte("data: "), marshal(payload)...), []byte("\n\n")...)
}
func (s *translatedStream) tool(index int, id, name, args, signature string, first bool) []byte {
	tool := map[string]any{"index": index, "function": map[string]any{"arguments": args}}
	if first {
		tool["id"] = id
		tool["type"] = "function"
		tool["function"].(map[string]any)["name"] = name
	}
	if signature != "" {
		tool["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
	}
	s.hasTools = true
	return s.chunk(map[string]any{"tool_calls": []any{tool}}, "", false)
}
func (s *translatedStream) convert(event map[string]any) ([]byte, error) {
	switch s.family {
	case "claude":
		return s.claudeEvent(event)
	case "gemini":
		return s.geminiEvent(event)
	default:
		return s.responsesEvent(event)
	}
}
func (s *translatedStream) claudeEvent(event map[string]any) ([]byte, error) {
	switch str(event, "type") {
	case "message_start":
		m := object(event["message"])
		if id := str(m, "id"); id != "" {
			s.id = id
		}
		s.usage = usage(s.family, m["usage"])
		return s.chunk(map[string]any{"role": "assistant"}, "", true), nil
	case "content_block_start":
		block := object(event["content_block"])
		switch str(block, "type") {
		case "text":
			if text := textValue(block, "text"); text != "" {
				return s.chunk(map[string]any{"content": text}, "", false), nil
			}
		case "tool_use":
			index := s.nextTool
			s.nextTool++
			s.tools[number(event["index"])] = index
			return s.tool(index, str(block, "id"), str(block, "name"), "", "", true), nil
		}
	case "content_block_delta":
		delta := object(event["delta"])
		switch str(delta, "type") {
		case "text_delta":
			return s.chunk(map[string]any{"content": delta["text"]}, "", false), nil
		case "thinking_delta":
			return s.chunk(map[string]any{"reasoning_content": delta["thinking"]}, "", false), nil
		case "input_json_delta":
			index, ok := s.tools[number(event["index"])]
			if !ok {
				return nil, fmt.Errorf("Claude 工具增量缺少起始块")
			}
			return s.tool(index, "", "", textValue(delta, "partial_json"), "", false), nil
		}
	case "message_delta":
		if value, ok := object(event["usage"])["output_tokens"]; ok {
			s.usage.CompletionTokens = number(value)
			s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
		}
		reason := ""
		if value := str(object(event["delta"]), "stop_reason"); value != "" {
			reason = finish(value)
		}
		return s.chunk(map[string]any{}, reason, true), nil
	case "message_stop":
		s.complete = true
		s.terminal = true
		return []byte("data: [DONE]\n\n"), nil
	}
	return nil, nil
}
func (s *translatedStream) geminiEvent(root map[string]any) ([]byte, error) {
	if wrapped, ok := root["response"].(map[string]any); ok {
		root = wrapped
	}
	if id := str(root, "responseId"); id != "" {
		s.id = id
	}
	if root["usageMetadata"] != nil {
		s.usage = usage(s.family, root["usageMetadata"])
	}
	candidates := array(root["candidates"])
	if len(candidates) == 0 {
		if feedback := object(root["promptFeedback"]); str(feedback, "blockReason") != "" {
			s.complete = true
			return s.chunk(map[string]any{}, "content_filter", true), nil
		}
		if root["usageMetadata"] != nil {
			return s.chunk(map[string]any{}, "", true), nil
		}
		return nil, nil
	}
	candidate := object(candidates[0])
	var out []byte
	for _, item := range array(object(candidate["content"])["parts"]) {
		part := object(item)
		if text, ok := part["text"].(string); ok {
			key := "content"
			if thought, _ := part["thought"].(bool); thought {
				key = "reasoning_content"
			}
			out = append(out, s.chunk(map[string]any{key: text}, "", false)...)
		}
		if fn, ok := part["functionCall"].(map[string]any); ok {
			id := str(fn, "id")
			if id == "" {
				id = "call_" + strconv.Itoa(s.nextTool)
			}
			out = append(out, s.tool(s.nextTool, id, str(fn, "name"), string(marshal(fn["args"])), str(part, "thoughtSignature"), true)...)
			s.nextTool++
		}
	}
	if reason := str(candidate, "finishReason"); reason != "" {
		s.complete = true
		end := finish(reason)
		if end == "stop" && s.hasTools {
			end = "tool_calls"
		}
		out = append(out, s.chunk(map[string]any{}, end, true)...)
	}
	return out, nil
}
func (s *translatedStream) responsesEvent(event map[string]any) ([]byte, error) {
	switch str(event, "type") {
	case "response.created":
		response := object(event["response"])
		if id := str(response, "id"); id != "" {
			s.id = id
		}
		return s.chunk(map[string]any{"role": "assistant"}, "", false), nil
	case "response.output_text.delta":
		return s.chunk(map[string]any{"content": event["delta"]}, "", false), nil
	case "response.reasoning_summary_text.delta":
		return s.chunk(map[string]any{"reasoning_content": event["delta"]}, "", false), nil
	case "response.output_item.added":
		item := object(event["item"])
		if str(item, "type") == "function_call" {
			index := s.nextTool
			s.nextTool++
			s.tools[number(event["output_index"])] = index
			return s.tool(index, str(item, "call_id"), str(item, "name"), textValue(item, "arguments"), "", true), nil
		}
	case "response.function_call_arguments.delta":
		index, ok := s.tools[number(event["output_index"])]
		if !ok {
			return nil, fmt.Errorf("Responses 工具增量缺少起始事件")
		}
		return s.tool(index, "", "", textValue(event, "delta"), "", false), nil
	case "response.completed", "response.incomplete":
		response := object(event["response"])
		if id := str(response, "id"); id != "" {
			s.id = id
		}
		s.usage = usage(s.family, response["usage"])
		reason := "stop"
		if str(event, "type") == "response.incomplete" {
			reason = "length"
		} else if s.hasTools {
			reason = "tool_calls"
		}
		s.complete = true
		s.terminal = true
		out := s.chunk(map[string]any{}, reason, true)
		return append(out, []byte("data: [DONE]\n\n")...), nil
	case "response.failed", "error":
		return nil, &provider.Failure{Status: 502, Message: "Responses 流推理失败", Body: cleanError(string(marshal(event)), s.cred)}
	}
	return nil, nil
}
func collectStream(body io.Reader, model string) (*provider.ChatResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	result := &provider.ChatResult{Model: model, Created: time.Now().Unix(), Message: provider.Message{Role: "assistant"}}
	var text strings.Builder
	toolMap := map[int]map[string]any{}
	size := 0
	done := false
	for {
		raw, err := nextEvent(scanner)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		size += len(raw)
		if size > 32<<20 {
			return nil, fmt.Errorf("收集后的完成响应超过 32 MB")
		}
		if bytes.Equal(raw, []byte("[DONE]")) {
			done = true
			break
		}
		var frame map[string]any
		if json.Unmarshal(raw, &frame) != nil {
			return nil, fmt.Errorf("完成流格式无效")
		}
		if event, ok := frame["_relaypool_native_event"].(map[string]any); ok {
			if kind := str(event, "type"); kind == "response.completed" || kind == "response.incomplete" {
				response := object(event["response"])
				if output, ok := response["output"]; ok {
					result.NativeOutput = marshal(output)
				}
			}
			continue
		}
		if id := str(frame, "id"); id != "" {
			result.ID = id
		}
		if u, ok := frame["usage"]; ok {
			m := object(u)
			result.Usage = provider.Usage{PromptTokens: number(m["prompt_tokens"]), CompletionTokens: number(m["completion_tokens"]), TotalTokens: number(m["total_tokens"])}
		}
		for _, value := range array(frame["choices"]) {
			choice := object(value)
			delta := object(choice["delta"])
			text.WriteString(textValue(delta, "content"))
			result.Message.ReasoningContent += textValue(delta, "reasoning_content")
			if reason := str(choice, "finish_reason"); reason != "" {
				result.FinishReason = reason
			}
			for _, item := range array(delta["tool_calls"]) {
				entry := object(item)
				index := number(entry["index"])
				current := toolMap[index]
				if current == nil {
					current = functionCall(str(entry, "id"), str(object(entry["function"]), "name"), "", "")
					toolMap[index] = current
				}
				fn := current["function"].(map[string]any)
				if extra, ok := entry["extra_content"]; ok {
					current["extra_content"] = extra
				}
				fn["arguments"] = textValue(fn, "arguments") + textValue(object(entry["function"]), "arguments")
			}
		}
	}
	if !done {
		return nil, io.ErrUnexpectedEOF
	}
	result.Message.Content = marshal(text.String())
	tools := []any{}
	for i := 0; i < len(toolMap); i++ {
		if toolMap[i] == nil {
			return nil, fmt.Errorf("完成工具索引不连续")
		}
		tools = append(tools, toolMap[i])
	}
	if len(tools) > 0 {
		result.Message.ToolCalls = marshal(tools)
	}
	return result, nil
}

// CollectChatStream 收集已转换的 Chat 流；保持工具增量、用量及完整结束检查一致。
func CollectChatStream(body io.Reader, model string) (*provider.ChatResult, error) {
	return collectStream(body, model)
}
