package workbuddy

import (
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// trackingCloser records whether Close was called.
//
// The flag is atomic because the copier goroutine and the test can both observe
// it, and the race detector runs in CI.
type trackingCloser struct {
	reader io.Reader
	closed atomic.Bool
}

func (c *trackingCloser) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *trackingCloser) Close() error {
	c.closed.Store(true)
	return nil
}
func (c *trackingCloser) isClosed() bool { return c.closed.Load() }

// The stream chain is:
//
//	resp.Body -> idleTimeoutReader -> io.NopCloser -> pipe reader
//
// A caller of ChatStream closes the OUTERMOST reader. That must propagate back
// to the upstream body, or every abandoned stream leaks a connection and a
// goroutine. This test pins that contract down.
func TestStreamClosePropagatesToUpstreamBody(t *testing.T) {
	// A stream that never ends, so the only way the copier stops is a close.
	upstream := &trackingCloser{reader: strings.NewReader(strings.Repeat(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n", 10000))}

	wrapped := newIdleTimeoutReader(upstream, time.Second)
	resp := rewriteChatStream(&http.Response{
		Body:   wrapped,
		Header: http.Header{},
	})

	// Read one frame so the copier goroutine is definitely running.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("first read: %v", err)
	}

	// The caller abandons the stream.
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Give the copier goroutine a moment to unwind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if upstream.isClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("closing the stream did not close the upstream body; " +
		"every abandoned stream would leak a connection and a goroutine")
}

// A goroutine-count check that catches a leak regardless of the mechanism.
func TestNoGoroutineLeakOnAbandonedStream(t *testing.T) {
	// Let any startup noise settle.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		upstream := &trackingCloser{reader: strings.NewReader(strings.Repeat(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n", 10000))}
		wrapped := newIdleTimeoutReader(upstream, 5*time.Second)
		resp := rewriteChatStream(&http.Response{
			Body:   wrapped,
			Header: http.Header{},
		})

		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf)
		_ = resp.Body.Close()
	}

	// Allow the copiers to finish.
	deadline := time.Now().Add(3 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines grew from %d to %d across 20 abandoned streams; the "+
		"copier goroutine is leaking", before, after)
}

// The idle-timeout reader must not leak its own read goroutine when the caller
// closes it mid-read.
func TestIdleReaderCloseUnblocksPendingRead(t *testing.T) {
	// A reader that blocks forever, standing in for an upstream that accepted
	// the request and then went silent.
	blocking := &blockingReader{release: make(chan struct{})}
	r := newIdleTimeoutReader(blocking, 10*time.Second)
	defer close(blocking.release)

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 8)
		_, _ = r.Read(buf)
		close(done)
	}()

	// Give the read a moment to park.
	time.Sleep(50 * time.Millisecond)

	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("closing the idle reader did not unblock the pending read")
	}
}

type blockingReader struct {
	release chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}
func (b *blockingReader) Close() error { return nil }
