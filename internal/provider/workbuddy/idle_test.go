package workbuddy

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type interruptibleBody struct {
	closed   chan struct{}
	finished chan struct{}
	reads    atomic.Int32
	closes   atomic.Int32
	once     sync.Once
	closeErr error
}

func (b *interruptibleBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	<-b.closed
	// A late completion must touch only the wrapper's buffer, not caller memory.
	n := copy(p, "late")
	close(b.finished)
	return n, io.EOF
}
func (b *interruptibleBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.closed) })
	return b.closeErr
}

func TestIdleTimeoutClosesUpstreamWithoutCallerCleanup(t *testing.T) {
	b := &interruptibleBody{closed: make(chan struct{}), finished: make(chan struct{})}
	r := newIdleTimeoutReader(b, 20*time.Millisecond)
	p := []byte("keep")
	n, err := r.Read(p)
	var timeout *idleTimeoutError
	if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected idle timeout, got n=%d err=%v", n, err)
	}
	if string(p) != "keep" {
		t.Fatal("timed out read changed caller buffer")
	}
	select {
	case <-b.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout left upstream read blocked")
	}
	if b.closes.Load() != 1 {
		t.Fatal("timeout did not close upstream exactly once")
	}
	if n, err := r.Read(p); n != 0 || err != io.EOF {
		t.Fatal("closed stream accepted another read")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if b.closes.Load() != 1 || b.reads.Load() != 1 {
		t.Fatal("timeout cleanup repeated upstream operations")
	}
}

func TestConcurrentIdleCloseIsIdempotentAndPreservesError(t *testing.T) {
	want := errors.New("synthetic close failure")
	b := &interruptibleBody{closed: make(chan struct{}), finished: make(chan struct{}), closeErr: want}
	r := newIdleTimeoutReader(b, time.Second)
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		go func() { errs <- r.Close() }()
	}
	for i := 0; i < 32; i++ {
		if err := <-errs; err != want {
			t.Fatal("close result changed between callers")
		}
	}
	if b.closes.Load() != 1 {
		t.Fatalf("upstream closed %d times", b.closes.Load())
	}
	if n, err := r.Read(make([]byte, 8)); n != 0 || err != io.EOF || b.reads.Load() != 0 {
		t.Fatal("read after close started upstream work")
	}
}

func TestConcurrentIdleReadsKeepBytesWithTheirCaller(t *testing.T) {
	input := make([]byte, 64)
	for i := range input {
		input[i] = byte(i + 33)
	}
	r := newIdleTimeoutReader(io.NopCloser(strings.NewReader(string(input))), time.Second)
	defer r.Close()
	type outcome struct {
		b   byte
		n   int
		err error
	}
	results := make(chan outcome, len(input))
	for range input {
		go func() { p := make([]byte, 1); n, err := r.Read(p); results <- outcome{p[0], n, err} }()
	}
	seen := map[byte]bool{}
	for range input {
		out := <-results
		if out.n != 1 || out.err != nil || seen[out.b] {
			t.Fatalf("lost, duplicated or failed stream byte: n=%d err=%v", out.n, out.err)
		}
		seen[out.b] = true
	}
	for _, b := range input {
		if !seen[b] {
			t.Fatal("stream content changed")
		}
	}
}

type finalChunkBody struct{ reads int }

func (b *finalChunkBody) Read(p []byte) (int, error) { b.reads++; return copy(p, "OK"), io.EOF }
func (*finalChunkBody) Close() error                 { return nil }

func TestIdleReaderHandlesEmptyReadsAndFinalChunk(t *testing.T) {
	b := &finalChunkBody{}
	r := newIdleTimeoutReader(b, time.Second)
	defer r.Close()
	if n, err := r.Read(nil); n != 0 || err != nil || b.reads != 0 {
		t.Fatal("empty read touched upstream")
	}
	p := make([]byte, 8)
	if n, err := r.Read(p); n != 2 || err != io.EOF || string(p[:n]) != "OK" {
		t.Fatal("last chunk lost")
	}
	if n, err := r.Read(p); n != 0 || err != io.EOF || b.reads != 1 {
		t.Fatal("EOF started another upstream read")
	}
}

func TestDisabledIdleTimeoutReturnsOriginalReader(t *testing.T) {
	b := io.NopCloser(strings.NewReader("ok"))
	if WithIdleTimeout(b, 0) != b {
		t.Fatal("disabled timeout added a wrapper")
	}
}
