package huawei

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

// Reads bounded SSE events without a goroutine. Closing the wrapper always
// closes the original socket, including when the client cancels before Read.
type checkedStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	cred    provider.Credential
	pending []byte
	done    bool
}

func newCheckedStream(body io.ReadCloser, cred provider.Credential) *checkedStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	return &checkedStream{body: body, scanner: scanner, cred: cred}
}
func (s *checkedStream) Close() error { return s.body.Close() }
func (s *checkedStream) prime() error { data, err := s.next(); s.pending = data; return err }
func (s *checkedStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		data, err := s.next()
		if err != nil {
			return 0, err
		}
		s.pending = data
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func (s *checkedStream) next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	data := []string{}
	size := 0
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if len(data) > 0 {
				return s.event(strings.Join(data, "\n"))
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			size += len(line)
			if size > 4<<20 {
				return nil, &provider.Failure{Status: 502, Message: "华为流事件超过大小限制"}
			}
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, &provider.Failure{Status: 502, Message: "华为流读取失败或超时"}
	}
	if len(data) > 0 {
		return s.event(strings.Join(data, "\n"))
	}
	return nil, &provider.Failure{Status: 502, Message: "华为流在结束标记前断开"}
}
func (s *checkedStream) event(data string) ([]byte, error) {
	if data == "[DONE]" {
		s.done = true
		return []byte("data: [DONE]\n\n"), nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &root) != nil || root == nil {
		return nil, &provider.Failure{Status: 502, Message: "华为返回无效流事件"}
	}
	if raw := root["error"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		return nil, failure(502, redact(data, s.cred))
	}
	if len(root["choices"]) == 0 && len(root["usage"]) == 0 {
		return nil, &provider.Failure{Status: 502, Message: "华为返回无法识别的流事件"}
	}
	return []byte("data: " + data + "\n\n"), nil
}
