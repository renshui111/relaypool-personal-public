package executor

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// 普通请求在返回或异常时释放名额，成功的流把名额交给响应体管理。
func (e *Executor) runAdmittedAttempt(
	ctx context.Context,
	item pool.Item,
	p provider.Provider,
	index int,
	attempt func(context.Context, pool.Item, provider.Provider, int) (*Result, *pool.Classified, error),
) (*Result, *pool.Classified, error) {
	transferred := false
	defer func() {
		if !transferred {
			e.pool.ReleaseItem(item)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	result, classified, err := attempt(ctx, item, p, index)
	if err == nil && result != nil && result.Stream != nil {
		if result.Stream.Body == nil {
			return nil, nil, errors.New("上游返回的流没有响应体")
		}
		managed := &admittedStream{
			body:    result.Stream.Body,
			release: func() { e.pool.ReleaseItem(item) },
		}
		// 即使调用方尚未读取，取消请求也会关闭上游并归还名额。
		managed.cancelMu.Lock()
		managed.stopCancel = context.AfterFunc(ctx, func() { _ = managed.Close() })
		managed.cancelMu.Unlock()
		result.Stream.Body = managed
		transferred = true
	}
	return result, classified, err
}

// 流读完、读取失败或调用方关闭时统一收尾；重复关闭不会多释放名额。
type admittedStream struct {
	body       io.ReadCloser
	release    func()
	once       sync.Once
	closeErr   error
	stopCancel func() bool
	cancelMu   sync.Mutex
}

func (s *admittedStream) Read(buffer []byte) (int, error) {
	n, err := s.body.Read(buffer)
	if err != nil {
		_ = s.Close()
	}
	return n, err
}

func (s *admittedStream) Close() error {
	// 回调和显式关闭可能同时发生；停止回调放在 once 内会与自身争锁。
	s.cancelMu.Lock()
	stop := s.stopCancel
	s.cancelMu.Unlock()
	if stop != nil {
		stop()
	}
	s.once.Do(func() {
		defer s.release()
		s.closeErr = s.body.Close()
	})
	return s.closeErr
}
