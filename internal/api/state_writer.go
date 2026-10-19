package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

// stateWriter persists pool runtime state without blocking the request path.
//
// WHY THIS EXISTS: the pool calls its observer on EVERY state change, and a
// successful request triggers one via MarkOK. Writing to SQLite synchronously
// there would put a database write (plus a mutex acquisition) in the latency
// path of every single completion, which is the busiest code in the gateway.
//
// The fix has two parts:
//
//  1. COALESCE per account: only the latest state matters, so a burst of
//     requests on one account collapses into one write. This is what keeps the
//     write rate proportional to accounts rather than to requests.
//  2. WRITE ASYNCHRONOUSLY: the observer hands the snapshot to a goroutine and
//     returns immediately, so a slow disk cannot add latency to a completion.
//
// The tradeoff is explicit: a crash loses the last few seconds of cooldown
// state. That is acceptable because cooldown state is derived (a failed account
// re-cools on its next failure) and the alternative costs latency on every
// request.
type stateWriter struct {
	store *store.Store

	mu      sync.Mutex
	pending map[string]pool.Item
	// 最高版本在写盘后仍保留，拒绝迟到的旧回调。
	versions map[string]uint64
	epochs   map[string]uint64
	flushMu  sync.Mutex
	wake     chan struct{}
	// stopped closes when the writer should exit.
	stopped chan struct{}
	closing bool
	// closeOnce guards start/stop symmetry.
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup

	// flushInterval bounds how stale the persisted state can be when the
	// request rate is very low.
	flushInterval time.Duration
}

// newStateWriter creates the writer. Call start to begin persisting.
func newStateWriter(st *store.Store) *stateWriter {
	return &stateWriter{
		store:         st,
		pending:       map[string]pool.Item{},
		versions:      map[string]uint64{},
		epochs:        map[string]uint64{},
		wake:          make(chan struct{}, 1),
		stopped:       make(chan struct{}),
		flushInterval: 2 * time.Second,
	}
}

// start launches the background flusher.
func (w *stateWriter) start() {
	w.startOnce.Do(func() {
		w.wg.Add(1)
		go w.loop()
	})
}

// stop flushes any pending state and waits for the loop to exit.
//
// It flushes on the way out so a clean shutdown does not lose the final
// cooldown, which is the one case where losing it would be visible: a restart
// immediately after would let a cooling account be retried.
func (w *stateWriter) stop() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closing = true
		w.mu.Unlock()
		close(w.stopped)
		w.wg.Wait()
		// One final synchronous flush, using a fresh context because the
		// shutdown context may already be cancelled.
		w.flush(context.Background())
	})
}

// observe records a state change. It never blocks.
func (w *stateWriter) observe(item pool.Item) {
	if w == nil || w.store == nil {
		return
	}
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	if epoch := w.epochs[item.ID]; epoch > item.RuntimeEpoch {
		w.mu.Unlock()
		return
	}
	if item.RuntimeEpoch > w.epochs[item.ID] {
		w.epochs[item.ID] = item.RuntimeEpoch
		delete(w.versions, item.ID)
	}
	if latest := w.versions[item.ID]; latest > 0 && item.StateVersion <= latest {
		w.mu.Unlock()
		return
	}
	if item.StateVersion > 0 {
		w.versions[item.ID] = item.StateVersion
	}
	// 版本为零兼容旧调用方，号池发送的非零版本按版本顺序合并。
	w.pending[item.ID] = item
	w.mu.Unlock()

	// A non-blocking wake: if the flusher is already awake, this is a no-op and
	// the pending item will be picked up by that pass.
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// loop drains pending state until stopped.
func (w *stateWriter) loop() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopped:
			return
		case <-ticker.C:
			w.flush(context.Background())
		case <-w.wake:
			// Debounce briefly so a burst collapses into one write rather than
			// one write per request.
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-w.stopped:
				timer.Stop()
				return
			}
			timer.Stop()
			w.flush(context.Background())
		}
	}
}

// flush writes every pending account state.
func (w *stateWriter) flush(parent context.Context) {
	// 定时、手动和停机刷盘必须串行，避免数据库再次收到乱序批次。
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	// Take the batch and clear it under the lock, so an account updated during
	// the write is queued for the next pass rather than lost.
	batch := w.pending
	w.pending = map[string]pool.Item{}
	w.mu.Unlock()

	for id, item := range batch {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		err := w.store.UpdatePoolRuntime(ctx, id, runtimeFromItem(item))
		cancel()
		if err != nil {
			log.Printf("api: persist runtime state for %s: %v", id, err)
			// 临时写入故障保留最新快照，下一次定时刷盘重试，不扩大队列。
			if !errors.Is(err, store.ErrNotFound) {
				w.mu.Lock()
				if _, newer := w.pending[id]; !newer {
					w.pending[id] = item
				}
				w.mu.Unlock()
			}
		}
	}
}

func runtimeFromItem(item pool.Item) store.AccountRuntime {
	return store.AccountRuntime{
		GroupDownUntil: item.GroupDownUntil,
		DownUntil:      item.DownUntil, LastError: item.LastError, LastKind: item.LastKind,
		BackoffLevel: item.BackoffLevel, ModelDownUntil: item.ModelDownUntil,
		ModelBackoff: item.ModelBackoff, ModelLastKind: item.ModelLastKind, ProvenModels: item.ProvenModels,
	}
}

// 删除账号后移除待写状态，避免持久化队列长期保留已删除的账号。
func (w *stateWriter) forget(id string) {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pending, id)
	delete(w.versions, id)
	delete(w.epochs, id)
}
