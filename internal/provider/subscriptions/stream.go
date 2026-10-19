package subscriptions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/relaypool/relaypool/internal/provider"
)

// 逐事件转换，无后台泵和无限缓冲；调用方关闭流会直接关闭上游连接。
type subscriptionStream struct {
	body                      io.ReadCloser
	scanner                   *bufio.Scanner
	client                    *Client
	cred                      provider.Credential
	model, id                 string
	buffer                    []byte
	done, sawOutput, sawTools bool
	usage                     any
	plans                     map[string]string
	planBytes                 int
}

func newSubscriptionStream(body io.ReadCloser, client *Client, cred provider.Credential, model string) *subscriptionStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	return &subscriptionStream{body: body, scanner: scanner, client: client, cred: cred, model: model, id: "chatcmpl-" + uuid.NewString(), plans: map[string]string{}}
}
func (s *subscriptionStream) Close() error { return s.body.Close() }
func (s *subscriptionStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.buffer) == 0 {
		if err := s.prime(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}
func (s *subscriptionStream) prime() error {
	if len(s.buffer) > 0 {
		return nil
	}
	if s.done {
		return io.EOF
	}
	// 控制帧数量有限，避免永远发送元数据却没有回答的上游占住调度槽位。
	for count := 0; count < 512; count++ {
		event, data, err := s.nextEvent()
		if err != nil {
			return err
		}
		if s.client.family == "trae-intl" {
			s.buffer, err = s.traeIntlEvent(event, data)
		} else if s.client.family == "qoder" {
			s.buffer, err = s.qoderEvent(event, data)
		} else {
			s.buffer, err = s.traeEvent(event, data)
		}
		if err != nil {
			return err
		}
		if len(s.buffer) > 0 {
			return nil
		}
	}
	return failure(502, "订阅上游连续控制帧过多，未返回有效回答")
}

// plan_item 是同一个节点的累计文本，仅发送新后缀，重复或较短快照不重复输出。
func (s *subscriptionStream) traeIntlEvent(event string, data []byte) ([]byte, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, failure(502, "国际会话事件格式无效")
	}
	switch event {
	case "error":
		return nil, s.client.upstreamFailure(s.cred, 502, lookup(value, "code"), data, nil)
	case "token_usage":
		usage := map[string]int64{}
		for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			n, ok := value[name].(json.Number)
			if !ok {
				continue
			}
			v, err := n.Int64()
			if err != nil || v < 0 {
				return nil, failure(502, "国际会话用量格式无效")
			}
			usage[name] = v
		}
		if usage["total_tokens"] == 0 {
			usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]
		}
		s.usage = usage
	case "plan_item":
		id, text := lookup(value, "id"), ""
		text, _ = value["thought"].(string)
		if !remoteID(id) {
			return nil, failure(502, "国际会话文本节点编号无效")
		}
		previous, exists := s.plans[id]
		if !exists && len(s.plans) >= 256 {
			return nil, failure(502, "国际会话文本节点过多")
		}
		if strings.HasPrefix(previous, text) {
			return nil, nil
		}
		if !strings.HasPrefix(text, previous) {
			return nil, failure(502, "国际会话修改了已发送文本，无法无损转换")
		}
		s.planBytes += len(text) - len(previous)
		if s.planBytes > 8<<20 {
			return nil, failure(502, "国际会话累计文本过大")
		}
		s.plans[id] = text
		s.sawOutput = true
		return s.chunk(map[string]any{"content": text[len(previous):]}, "", nil), nil
	case "done":
		if !s.sawOutput {
			return nil, failure(502, "国际会话结束但没有有效文本回复")
		}
		s.done = true
		return append(s.chunk(map[string]any{}, "stop", s.usage), []byte("data: [DONE]\n\n")...), nil
	}
	return nil, nil
}
func (s *subscriptionStream) nextEvent() (string, []byte, error) {
	name := ""
	var data bytes.Buffer
	for s.scanner.Scan() {
		line := strings.TrimSuffix(s.scanner.Text(), "\r")
		if line == "" {
			if data.Len() > 0 {
				return name, bytes.TrimSuffix(data.Bytes(), []byte("\n")), nil
			}
			name = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len()+len(line) > 1<<20 {
				return "", nil, failure(502, "订阅事件超过 1 MB")
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
	}
	if err := s.scanner.Err(); err != nil {
		return "", nil, err
	}
	if data.Len() > 0 {
		return name, bytes.TrimSuffix(data.Bytes(), []byte("\n")), nil
	}
	return "", nil, io.ErrUnexpectedEOF
}

func (s *subscriptionStream) chunk(delta map[string]any, finish string, usage any) []byte {
	var reason any
	if finish != "" {
		reason = finish
	}
	frame := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}}
	if usage != nil {
		frame["usage"] = usage
	}
	return append(append([]byte("data: "), marshal(frame)...), []byte("\n\n")...)
}

func (s *subscriptionStream) qoderEvent(event string, data []byte) ([]byte, error) {
	if event == "error" {
		return nil, s.client.upstreamFailure(s.cred, 502, "", data, nil)
	}
	if string(data) == "[DONE]" {
		return s.finishQoder()
	}
	var outer map[string]any
	if json.Unmarshal(data, &outer) != nil {
		return nil, failure(502, "Qoder 流格式无效")
	}
	status := 0
	if value, ok := outer["statusCodeValue"].(float64); ok {
		status = int(value)
	}
	if body, ok := outer["body"]; ok {
		if text, ok := body.(string); ok {
			data = []byte(text)
		} else {
			data = marshal(body)
		}
	}
	if status >= 400 {
		return nil, s.client.upstreamFailure(s.cred, status, "", data, nil)
	}
	if string(data) == "[DONE]" {
		return s.finishQoder()
	}
	var frame map[string]any
	if json.Unmarshal(data, &frame) != nil {
		return nil, failure(502, "Qoder 内层流格式无效")
	}
	if value, ok := frame["error"]; ok && value != nil {
		return nil, s.client.upstreamFailure(s.cred, 502, lookup(object(value), "code"), data, nil)
	}
	choices, ok := frame["choices"].([]any)
	if !ok {
		if usage, ok := frame["usage"]; ok {
			s.usage = usage
		}
		return nil, nil
	}
	if value, ok := frame["usage"]; ok {
		s.usage = value
	}
	meaningful := false
	for _, value := range choices {
		choice := object(value)
		delta := object(choice["delta"])
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		tools, _ := delta["tool_calls"].([]any)
		if content != "" || reasoning != "" || len(tools) > 0 {
			s.sawOutput = true
			meaningful = true
		}
		if lookup(choice, "finish_reason") != "" && s.sawOutput {
			meaningful = true
		}
	}
	if !meaningful {
		return nil, nil
	}
	frame["model"] = s.model
	return append(append([]byte("data: "), marshal(frame)...), []byte("\n\n")...), nil
}
func (s *subscriptionStream) finishQoder() ([]byte, error) {
	if !s.sawOutput {
		return nil, failure(502, "Qoder 流已结束但没有有效回答")
	}
	s.done = true
	output := []byte{}
	if s.usage != nil {
		output = s.chunk(map[string]any{}, "", s.usage)
	}
	return append(output, []byte("data: [DONE]\n\n")...), nil
}

func (s *subscriptionStream) traeEvent(event string, data []byte) ([]byte, error) {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return nil, failure(502, "Trae 流格式无效")
	}
	switch event {
	case "error":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		_ = decoder.Decode(&value)
		return nil, s.client.upstreamFailure(s.cred, 502, lookup(value, "code"), data, nil)
	case "metadata":
		return nil, nil
	case "token_usage":
		s.usage = value
		return nil, nil
	case "output":
		delta := map[string]any{}
		if text, ok := value["response"].(string); ok && text != "" {
			delta["content"] = text
		}
		if text, ok := value["reasoning_content"].(string); ok && text != "" {
			delta["reasoning_content"] = text
		}
		if raw, ok := value["tool_calls"]; ok && raw != nil {
			var calls []any
			if list, ok := raw.([]any); ok {
				calls = list
			} else if one, ok := raw.(map[string]any); ok {
				calls = []any{one}
			} else {
				return nil, failure(502, "Trae 工具增量格式无效")
			}
			for index, rawCall := range calls {
				call := object(rawCall)
				if call == nil {
					return nil, failure(502, "Trae 工具增量格式无效")
				}
				if fn, ok := call["function_call"]; ok {
					call["function"] = fn
					delete(call, "function_call")
				}
				fn := object(call["function"])
				if fn == nil {
					return nil, failure(502, "Trae 工具函数缺失")
				}
				delete(fn, "namespace")
				delete(fn, "partial_arguments")
				if args, ok := fn["arguments"]; ok {
					if _, ok := args.(string); !ok {
						fn["arguments"] = string(marshal(args))
					}
				}
				if _, ok := call["index"]; !ok {
					call["index"] = index
				}
				call["type"] = "function"
			}
			if len(calls) > 0 {
				delta["tool_calls"] = calls
				s.sawTools = true
			}
		}
		if len(delta) == 0 {
			return nil, nil
		}
		s.sawOutput = true
		return s.chunk(delta, "", nil), nil
	case "done":
		if !s.sawOutput {
			return nil, failure(502, "Trae 流已结束但没有有效回答")
		}
		finish := lookup(value, "finish_reason")
		if finish == "" {
			if s.sawTools {
				finish = "tool_calls"
			} else {
				finish = "stop"
			}
		}
		s.done = true
		return append(s.chunk(map[string]any{}, finish, s.usage), []byte("data: [DONE]\n\n")...), nil
	}
	return nil, nil
}
