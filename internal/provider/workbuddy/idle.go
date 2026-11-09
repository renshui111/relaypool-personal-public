package workbuddy

import (
	"io"
	"sync"
	"time"
)

// idleTimeoutReader fails a read that stalls longer than the idle window.
//
// WHY THIS EXISTS: net/http has no per-read timeout. A whole-request timeout
// cannot be used on a stream either, because a long completion would be cut off
// mid-answer. Without something here, an upstream that accepts the request and
// then stops sending holds the connection and a goroutine open forever, and a
// handful of those exhaust the pool.
//
// Reads use a private buffer because timeout/Close can return before the
// underlying Read unwinds. Timeout closes the upstream immediately. As with
// net/http response bodies, the underlying Close should unblock a pending Read.
type idleTimeoutReader struct {
	reader  io.ReadCloser
	timeout time.Duration
	// Serialize readers and reuse the private buffer only after a read finishes.
	// Close never takes readMu, so it can interrupt a blocked read.
	readMu sync.Mutex
	buf    []byte
	eof    bool
	// closing is closed when the caller gives up, unblocking a pending read.
	closing chan struct{}

	// Both the notification and the underlying Close happen exactly once.
	closeOnce sync.Once
	closeErr  error
}

type readResult struct {
	n   int
	err error
}

// newIdleTimeoutReader wraps reader with a per-read deadline.
func newIdleTimeoutReader(reader io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return reader
	}
	return &idleTimeoutReader{
		reader:  reader,
		timeout: timeout,
		closing: make(chan struct{}),
	}
}

// WithIdleTimeout 复用已经验证的流读取超时处理，供兼容 API 渠道使用。
func WithIdleTimeout(reader io.ReadCloser, timeout time.Duration) io.ReadCloser {
	return newIdleTimeoutReader(reader, timeout)
}

// Read performs one read, bounded by the idle timeout.
func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	select {
	case <-r.closing:
		return 0, io.EOF
	default:
	}
	if r.eof {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	// The goroutine needs its own buffer: the caller may reuse p after this
	// call returns, and writing into it from an abandoned goroutine would be a
	// data race.
	if cap(r.buf) < len(p) {
		r.buf = make([]byte, len(p))
	}
	buf := r.buf[:len(p)]
	// A per-read buffered result prevents a late read from being delivered to
	// a different caller, and cannot block if timeout/Close has already won.
	result := make(chan readResult, 1)

	go func() {
		n, err := r.reader.Read(buf)
		result <- readResult{n: n, err: err}
	}()

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case res := <-result:
		select {
		case <-r.closing:
			return 0, io.EOF
		default:
		}
		if res.n > 0 {
			copy(p, buf[:res.n])
		}
		if res.err == io.EOF {
			r.eof = true
		}
		return res.n, res.err
	case <-timer.C:
		// Release the socket without relying on an outer wrapper to call Close.
		_ = r.Close()
		return 0, &idleTimeoutError{timeout: r.timeout}
	case <-r.closing:
		return 0, io.EOF
	}
}

// Close releases the underlying reader.
//
// Concurrent callers see the same Close result. The upstream body is closed
// once even if timeout, caller cancellation and sanitizer cleanup coincide.
func (r *idleTimeoutReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.closing)
		r.closeErr = r.reader.Close()
	})
	return r.closeErr
}

// idleTimeoutError reports that a stream stalled.
type idleTimeoutError struct {
	timeout time.Duration
}

func (e *idleTimeoutError) Error() string {
	return "workbuddy: the upstream stream stalled for " + e.timeout.String()
}

// Timeout reports that this error was a timeout, so callers can distinguish it
// from a genuine network failure without importing net.
func (e *idleTimeoutError) Timeout() bool { return true }

// Temporary reports whether the operation is worth retrying.
func (e *idleTimeoutError) Temporary() bool { return true }
