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
// The shape is a reader that runs each Read on a goroutine and abandons it when
// the deadline expires. The goroutine may outlive the call, but it is parked in
// a read on a socket that gets closed when the stream is closed, so it is not a
// leak in practice.
type idleTimeoutReader struct {
	reader  io.ReadCloser
	timeout time.Duration
	// result carries one read's outcome back to the caller.
	result chan readResult
	// closing is closed when the caller gives up, unblocking a pending read.
	closing chan struct{}

	// closeOnce makes Close idempotent AND thread-safe.
	//
	// A plain bool here is a data race: the caller's Close and the sanitizer
	// goroutine's deferred Close can run concurrently, and a bare flag was read
	// and written by both. sync.Once also guarantees the channel is closed
	// exactly once, which a read-then-write flag does not.
	closeOnce sync.Once
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
		// Buffered so the reader goroutine never blocks delivering its result
		// after the caller has moved on.
		result:  make(chan readResult, 1),
		closing: make(chan struct{}),
	}
}

// WithIdleTimeout 复用已经验证的流读取超时处理，供兼容 API 渠道使用。
func WithIdleTimeout(reader io.ReadCloser, timeout time.Duration) io.ReadCloser {
	return newIdleTimeoutReader(reader, timeout)
}

// Read performs one read, bounded by the idle timeout.
func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	// The goroutine needs its own buffer: the caller may reuse p after this
	// call returns, and writing into it from an abandoned goroutine would be a
	// data race.
	buf := make([]byte, len(p))

	go func() {
		n, err := r.reader.Read(buf)
		select {
		case r.result <- readResult{n: n, err: err}:
		case <-r.closing:
			// The caller gave up; nobody is listening.
		}
	}()

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case res := <-r.result:
		if res.n > 0 {
			copy(p, buf[:res.n])
		}
		return res.n, res.err
	case <-timer.C:
		// Signal the pending read that its result is unwanted, then surface a
		// timeout the caller can classify.
		r.signalClosing()
		return 0, &idleTimeoutError{timeout: r.timeout}
	case <-r.closing:
		return 0, io.EOF
	}
}

// Close releases the underlying reader.
//
// It is safe to call concurrently and to call more than once: sync.Once makes
// the channel close happen exactly once, which matters because both the client
// (abandoning a stream) and the sanitizer goroutine (finishing normally) close
// this reader.
func (r *idleTimeoutReader) Close() error {
	r.signalClosing()
	return r.reader.Close()
}

// signalClosing closes the closing channel exactly once.
//
// The earlier implementation used a plain bool read-then-write, which raced
// when two goroutines closed concurrently: the race detector flagged it, and a
// double close of a channel would have panicked.
func (r *idleTimeoutReader) signalClosing() {
	r.closeOnce.Do(func() {
		close(r.closing)
	})
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
