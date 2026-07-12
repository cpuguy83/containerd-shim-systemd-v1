package main

import (
	"sync"
	"time"
)

// unitWorkQueue is a minimal Kubernetes-controller-style work queue keyed by
// systemd unit name. It gives the event reactor these properties so that
// multiple events for the same unit never interfere:
//
//   - Coalescing: a unit name that is added repeatedly before it is processed is
//     returned by Get only once.
//   - Exclusivity: a unit name handed out by Get is not handed out again until
//     Done is called, so at most one worker reconciles a given unit at a time.
//   - No lost updates: if a unit is Added again while it is being processed, it
//     is re-queued once when Done is called, so a change observed mid-reconcile
//     is still handled.
//   - Bounded retries: a failed reconcile can be re-queued after an exponential
//     backoff (Retry) up to a cap, and the per-item failure count is cleared on
//     success (Forget). The reconnect resync remains the ultimate backstop.
//
// It intentionally omits the metrics and pluggable rate limiters of client-go's
// workqueue; the reconcile is level-driven (it reads current state), so a simple
// bounded exponential backoff is enough.
type unitWorkQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string
	dirty      map[string]struct{}
	processing map[string]struct{}
	failures   map[string]int
	shutdown   bool
	shutdownCh chan struct{}
}

func newUnitWorkQueue() *unitWorkQueue {
	q := &unitWorkQueue{
		dirty:      make(map[string]struct{}),
		processing: make(map[string]struct{}),
		failures:   make(map[string]int),
		shutdownCh: make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add enqueues name unless it is already pending. If name is currently being
// processed it is marked dirty so Done re-enqueues it.
func (q *unitWorkQueue) Add(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.shutdown {
		return
	}
	if _, ok := q.dirty[name]; ok {
		return
	}
	q.dirty[name] = struct{}{}
	if _, ok := q.processing[name]; ok {
		// Being processed now; Done will re-enqueue it because it is dirty.
		return
	}
	q.queue = append(q.queue, name)
	q.cond.Signal()
}

// AddAfter enqueues name once delay has elapsed. A non-positive delay enqueues
// immediately. The scheduled add is abandoned if the queue is shut down first.
func (q *unitWorkQueue) AddAfter(name string, delay time.Duration) {
	if delay <= 0 {
		q.Add(name)
		return
	}
	go func() {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
			q.Add(name)
		case <-q.shutdownCh:
		}
	}()
}

// Retry schedules name to be re-added after an exponential backoff, unless it
// has already been retried maxRetries times since the last Forget. It returns
// true if a retry was scheduled. The backoff is base * 2^(attempts) capped at
// max, so successive failures wait base, 2*base, 4*base, ... up to max.
func (q *unitWorkQueue) Retry(name string, maxRetries int, base, max time.Duration) bool {
	q.mu.Lock()
	if q.shutdown {
		q.mu.Unlock()
		return false
	}
	attempts := q.failures[name]
	if attempts >= maxRetries {
		q.mu.Unlock()
		return false
	}
	q.failures[name] = attempts + 1
	q.mu.Unlock()

	q.AddAfter(name, backoff(attempts, base, max))
	return true
}

// Forget clears the retry count for name. Call it once name has been reconciled
// successfully (or permanently given up on) so a future failure backs off from
// scratch and the failure map does not grow unbounded.
func (q *unitWorkQueue) Forget(name string) {
	q.mu.Lock()
	delete(q.failures, name)
	q.mu.Unlock()
}

// Get blocks until a unit name is available and returns it, or returns
// shutdown=true once the queue has been shut down and drained. The caller must
// call Done once the work for the returned name is complete.
func (q *unitWorkQueue) Get() (name string, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.queue) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		return "", true
	}

	name = q.queue[0]
	q.queue = q.queue[1:]
	delete(q.dirty, name)
	q.processing[name] = struct{}{}
	return name, false
}

// Done marks the work for name complete. If name was Added again while it was
// being processed, it is re-enqueued so the newer change is handled.
func (q *unitWorkQueue) Done(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.processing, name)
	if _, ok := q.dirty[name]; ok {
		q.queue = append(q.queue, name)
		q.cond.Signal()
	}
}

// ShutDown wakes every waiting worker and abandons any pending AddAfter timers.
// Get drains any already-queued items and then reports shutdown.
func (q *unitWorkQueue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.shutdown {
		return
	}
	q.shutdown = true
	close(q.shutdownCh)
	q.cond.Broadcast()
}

// backoff returns base doubled attempts times, capped at max (and at least
// base). attempts is clamped so the shift cannot overflow.
func backoff(attempts int, base, max time.Duration) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 62 {
		return max
	}
	d := base << uint(attempts)
	if d <= 0 || d > max { // overflow or over cap
		return max
	}
	return d
}
