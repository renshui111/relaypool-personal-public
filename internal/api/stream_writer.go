package api

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const defaultStreamWriteTimeout = 15 * time.Second

// 每次发送重新设置期限，只限制客户端阻塞写入，不限制整个对话的时长。
type streamResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func (w *streamResponseWriter) deadline() error {
	err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	// 内存测试响应体没有连接；真实 HTTP 服务通过 Unwrap 设置连接期限。
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
func (w *streamResponseWriter) Write(body []byte) (int, error) {
	if err := w.deadline(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(body)
}
func (w *streamResponseWriter) FlushError() error {
	if err := w.deadline(); err != nil {
		return err
	}
	return w.controller.Flush()
}
func (w *streamResponseWriter) Flush() { _ = w.FlushError() }

// 正式连接使用可返回错误的刷新操作；兼容已有内存流测试。
func writeStreamChunk(w io.Writer, flusher httpFlusher, body []byte) error {
	if _, err := w.Write(body); err != nil {
		return err
	}
	if f, ok := flusher.(interface{ FlushError() error }); ok {
		return f.FlushError()
	}
	flusher.Flush()
	return nil
}
