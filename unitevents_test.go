package main

import (
	"context"
	"errors"
	"io"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// errTransient is a stand-in for a transient systemd read failure.
var errTransient = errors.New("transient reconcile failure")

// withFastRetries shrinks the package retry policy so retry tests run quickly,
// and returns a function that restores the previous values.
func withFastRetries(t *testing.T, maxRetries int) func() {
	t.Helper()
	origMax, origBase, origCap := reconcileMaxRetries, reconcileRetryBase, reconcileRetryMax
	reconcileMaxRetries = maxRetries
	reconcileRetryBase = time.Millisecond
	reconcileRetryMax = 5 * time.Millisecond
	return func() {
		reconcileMaxRetries, reconcileRetryBase, reconcileRetryMax = origMax, origBase, origCap
	}
}

func TestUnitEventIsExit(t *testing.T) {
	t.Run("an ActiveState of inactive means the unit finished", func(t *testing.T) {
		if !unitEventIsExit(changedProps(map[string]string{"ActiveState": "inactive", "SubState": "dead"})) {
			t.Fatal("expected inactive/dead to be treated as an exit")
		}
	})

	t.Run("an ActiveState of failed means the unit finished", func(t *testing.T) {
		if !unitEventIsExit(changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"})) {
			t.Fatal("expected failed/failed to be treated as an exit")
		}
	})

	t.Run("an ActiveState of active is still running", func(t *testing.T) {
		if unitEventIsExit(changedProps(map[string]string{"ActiveState": "active", "SubState": "running"})) {
			t.Fatal("expected active/running not to be treated as an exit")
		}
	})

	t.Run("a deactivating transition is not yet an exit", func(t *testing.T) {
		if unitEventIsExit(changedProps(map[string]string{"ActiveState": "deactivating", "SubState": "stop-sigterm"})) {
			t.Fatal("expected deactivating not to be treated as an exit")
		}
	})

	t.Run("a payload without ActiveState is not an exit", func(t *testing.T) {
		if unitEventIsExit(changedProps(map[string]string{"SubState": "dead"})) {
			t.Fatal("expected a payload lacking ActiveState not to be treated as an exit")
		}
	})
}

func TestReconcileUnit(t *testing.T) {
	const unit = "io-containerd-systemd-ns-abc-init.service"

	t.Run("a tracked, still-running unit has its state read once", func(t *testing.T) {
		p := &fakeProcess{name: unit}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		reconcileUnit(context.Background(), units, systemd.PathBusEscape(unit))

		if p.LoadStateCalls() != 1 {
			t.Fatalf("expected exactly one LoadState call, got %d", p.LoadStateCalls())
		}
	})

	t.Run("an untracked unit is not read", func(t *testing.T) {
		other := &fakeProcess{name: unit}
		units := &fakeLookup{m: map[string]Process{unit: other}}

		reconcileUnit(context.Background(), units, systemd.PathBusEscape("some-other-unit.service"))

		if other.LoadStateCalls() != 0 {
			t.Fatalf("expected no LoadState call for an untracked unit, got %d", other.LoadStateCalls())
		}
	})

	t.Run("an already-exited unit is not read again", func(t *testing.T) {
		p := &fakeProcess{name: unit, state: pState{ExitCode: 1, ExitedAt: time.Now()}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		reconcileUnit(context.Background(), units, systemd.PathBusEscape(unit))

		if p.LoadStateCalls() != 0 {
			t.Fatalf("expected no LoadState call for an exited unit, got %d", p.LoadStateCalls())
		}
	})
}

// TestEnqueueIfExit covers the informer predicate. A real unit lifecycle emits
// two exit-matching events -- a leading inactive/dead before the unit starts and
// the real terminal event -- so the predicate must enqueue only useful work.
func TestEnqueueIfExit(t *testing.T) {
	const unit = "io-containerd-systemd-ns-abc-init.service"

	newQueued := func(p Process) (*unitWorkQueue, processLookup) {
		return newUnitWorkQueue(), &fakeLookup{m: map[string]Process{unit: p}}
	}

	t.Run("an exit transition for a tracked, running unit is enqueued", func(t *testing.T) {
		q, units := newQueued(&fakeProcess{name: unit})
		enqueueIfExit(q, units, exitUpdate(unit))
		if queueLen(q) != 1 {
			t.Fatalf("expected the exiting unit to be enqueued, queue has %d", queueLen(q))
		}
	})

	t.Run("a non-exit transition is not enqueued", func(t *testing.T) {
		q, units := newQueued(&fakeProcess{name: unit})
		running := newUpdate(unit, changedProps(map[string]string{"ActiveState": "active", "SubState": "running"}))
		enqueueIfExit(q, units, running)
		if queueLen(q) != 0 {
			t.Fatalf("expected a running-state change not to enqueue, queue has %d", queueLen(q))
		}
	})

	t.Run("an exit transition for an untracked unit is not enqueued", func(t *testing.T) {
		q, units := newQueued(&fakeProcess{name: unit})
		enqueueIfExit(q, units, exitUpdate("some-other-unit.service"))
		if queueLen(q) != 0 {
			t.Fatalf("expected an untracked unit not to enqueue, queue has %d", queueLen(q))
		}
	})

	t.Run("a trailing exit event for an already-exited unit is not enqueued", func(t *testing.T) {
		q, units := newQueued(&fakeProcess{name: unit, state: pState{ExitCode: 1, ExitedAt: time.Now()}})
		enqueueIfExit(q, units, exitUpdate(unit))
		if queueLen(q) != 0 {
			t.Fatalf("expected an already-exited unit not to enqueue, queue has %d", queueLen(q))
		}
	})
}

func TestReactToUnitEvents(t *testing.T) {
	const unit = "io-containerd-systemd-ns-abc-init.service"

	t.Run("an exit event for a tracked unit triggers a state read", func(t *testing.T) {
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			f.setState(pState{ExitCode: 1, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.LoadStateCalls() == 1 }) {
			t.Fatal("reactor never reconciled the exiting unit")
		}
		cancel()
		<-done
	})

	t.Run("the pre-start inactive event does not mark an unstarted process exited", func(t *testing.T) {
		// A real unit's first PropertiesChanged is inactive/dead, before it runs,
		// which matches the exit predicate. LoadState is level-driven and no-ops
		// while the process has not started (init: pid==0; exec: no exit file), so
		// this must not produce an exit. Only the real terminal event, after the
		// process has started, marks it exited -- exactly once.
		var started atomic.Bool
		var exits int32
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			if !started.Load() {
				return nil // models LoadState no-op for a not-yet-started process
			}
			atomic.AddInt32(&exits, 1)
			f.setState(pState{ExitCode: 5, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit) // pre-start inactive/dead (completed send == consumed)

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.LoadStateCalls() >= 1 }) {
			t.Fatal("reactor never reconciled the pre-start event")
		}
		if p.ProcessState().Exited() {
			t.Fatal("pre-start inactive event spuriously marked the process exited")
		}

		started.Store(true)
		updates <- exitUpdate(unit) // the real terminal event

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.ProcessState().Exited() }) {
			t.Fatal("the real exit event did not mark the process exited")
		}
		cancel()
		<-done

		if got := atomic.LoadInt32(&exits); got != 1 {
			t.Fatalf("expected exactly one exit transition, got %d", got)
		}
	})

	t.Run("a started process whose persisted state is not terminal reads the exit code from systemd", func(t *testing.T) {
		p := &fakeProcess{
			name: unit,
			loadStateFn: func(f *fakeProcess) error {
				f.setState(pState{Pid: 42, Status: "running"}) // create's running-state file
				return nil
			},
			loadExitStateFn: func(f *fakeProcess) error {
				f.setState(pState{Pid: 42, ExitCode: 7, ExitedAt: time.Now()})
				return nil
			},
		}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.ProcessState().Exited() }) {
			t.Fatal("reactor never read the terminal exit from systemd")
		}
		if got := p.ProcessState().ExitCode; got != 7 {
			t.Fatalf("expected exit code 7 from systemd, got %d", got)
		}
		if got := p.LoadExitStateCalls(); got != 1 {
			t.Fatalf("expected exactly one systemd exit read, got %d", got)
		}
		cancel()
		<-done
	})

	t.Run("an unstarted process does not read the exit code from systemd", func(t *testing.T) {
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			return nil // models LoadState no-op for a not-yet-started process (pid==0)
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit) // pre-start inactive/dead

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.LoadStateCalls() >= 1 }) {
			t.Fatal("reactor never reconciled the pre-start event")
		}
		if got := p.LoadExitStateCalls(); got != 0 {
			t.Fatalf("expected no systemd exit read for an unstarted unit, got %d", got)
		}
		if p.ProcessState().Exited() {
			t.Fatal("pre-start event spuriously marked the process exited")
		}
		cancel()
		<-done
	})

	t.Run("a persisted terminal exit is not re-read from systemd", func(t *testing.T) {
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			f.setState(pState{Pid: 9, ExitCode: 2, ExitedAt: time.Now()}) // create's fast-exit file
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.LoadStateCalls() >= 1 }) {
			t.Fatal("reactor never reconciled the unit")
		}
		if got := p.LoadExitStateCalls(); got != 0 {
			t.Fatalf("expected no systemd exit read when the file is already terminal, got %d", got)
		}
		if got := p.ProcessState().ExitCode; got != 2 {
			t.Fatalf("expected the persisted exit code 2 to stand, got %d", got)
		}
		cancel()
		<-done
	})

	t.Run("a non-exit event does not trigger a state read", func(t *testing.T) {
		p := &fakeProcess{name: unit}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		running := newUpdate(unit, changedProps(map[string]string{"ActiveState": "active", "SubState": "running"}))
		updates <- running // completing this send means the informer has consumed it

		cancel()
		<-done

		if p.LoadStateCalls() != 0 {
			t.Fatalf("expected no read for a running-state change, got %d", p.LoadStateCalls())
		}
	})

	t.Run("an exit event for an untracked unit does not trigger a state read", func(t *testing.T) {
		tracked := &fakeProcess{name: unit}
		units := &fakeLookup{m: map[string]Process{unit: tracked}}

		updates := make(chan unitUpdate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate("some-other-unit.service")

		cancel()
		<-done

		if tracked.LoadStateCalls() != 0 {
			t.Fatalf("expected no read for an untracked unit, got %d", tracked.LoadStateCalls())
		}
	})

	t.Run("cancelling the context stops the reactor", func(t *testing.T) {
		units := &fakeLookup{m: map[string]Process{}}
		updates := make(chan unitUpdate)
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("reactToUnitEvents did not return after context cancellation")
		}
	})

	t.Run("a slow reconcile for one unit does not block reconciles for others", func(t *testing.T) {
		const slowUnit = "io-containerd-systemd-ns-slow-init.service"
		const fastUnit = "io-containerd-systemd-ns-fast-init.service"

		block := make(chan struct{})
		slow := &fakeProcess{name: slowUnit, loadStateFn: func(f *fakeProcess) error {
			<-block // hold this reconcile until released
			f.setState(pState{ExitCode: 1, ExitedAt: time.Now()})
			return nil
		}}
		fast := &fakeProcess{name: fastUnit, loadStateFn: func(f *fakeProcess) error {
			f.setState(pState{ExitCode: 0, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{slowUnit: slow, fastUnit: fast}}

		updates := make(chan unitUpdate, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(slowUnit) // a worker starts and blocks
		updates <- exitUpdate(fastUnit) // must still be handled by another worker

		if !eventually(2*time.Second, time.Millisecond, func() bool { return fast.LoadStateCalls() == 1 }) {
			t.Fatal("fast unit was not reconciled while a slow unit's read was in flight")
		}

		close(block) // release the slow reconcile
		if !eventually(2*time.Second, time.Millisecond, func() bool { return slow.LoadStateCalls() == 1 }) {
			t.Fatal("slow unit was never reconciled")
		}

		cancel()
		<-done
	})

	t.Run("events arriving during a reconcile coalesce into a single read", func(t *testing.T) {
		release := make(chan struct{})
		started := make(chan struct{})
		var reads int32
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			atomic.AddInt32(&reads, 1)
			close(started) // fires once; a second read would panic here
			<-release      // hold the read open so more events arrive meanwhile
			f.setState(pState{ExitCode: 1, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		// An unbuffered channel makes each send synchronize with the informer, so
		// a completed send means that event was consumed and (while the first read
		// is in flight) coalesced onto the in-progress work.
		updates := make(chan unitUpdate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit) // a worker starts the read and blocks
		<-started

		for i := 0; i < 4; i++ {
			updates <- exitUpdate(unit) // consumed by the informer, coalesced while processing
		}

		if got := atomic.LoadInt32(&reads); got != 1 {
			t.Fatalf("expected a single read while duplicate events coalesced, got %d", got)
		}

		close(release)
		cancel()
		<-done

		// The coalesced events cause one re-queued reconcile, but the Exited guard
		// makes it a no-op, so still exactly one read.
		if got := atomic.LoadInt32(&reads); got != 1 {
			t.Fatalf("expected a single read for coalesced events, got %d", got)
		}
	})

	t.Run("a failing reconcile is retried until it succeeds", func(t *testing.T) {
		defer withFastRetries(t, 5)()

		var attempts int32
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			if atomic.AddInt32(&attempts, 1) < 3 {
				return errTransient // fail the first two reconciles
			}
			f.setState(pState{ExitCode: 1, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.ProcessState().Exited() }) {
			t.Fatal("expected the unit to be reconciled after retries")
		}
		if got := atomic.LoadInt32(&attempts); got != 3 {
			t.Fatalf("expected 3 reconcile attempts (2 failures + 1 success), got %d", got)
		}
		cancel()
		<-done
	})

	t.Run("a persistently failing reconcile gives up after the retry cap", func(t *testing.T) {
		const maxRetries = 3
		defer withFastRetries(t, maxRetries)()

		var attempts int32
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			atomic.AddInt32(&attempts, 1)
			return errTransient // never succeeds
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		// One initial attempt plus maxRetries retries, then it must stop.
		want := int32(maxRetries + 1)
		if got := stableFor(2*time.Second, 200*time.Millisecond, time.Millisecond, func() int64 {
			return int64(atomic.LoadInt32(&attempts))
		}); got != int64(want) {
			t.Fatalf("expected %d attempts (initial + %d retries), got %d", want, maxRetries, got)
		}
		cancel()
		<-done
	})

	t.Run("a unit deleted while queued for retry is dropped", func(t *testing.T) {
		// A high retry cap: if deletion did not drop the queued work, it would
		// keep retrying up to this many times.
		defer withFastRetries(t, 100)()

		var attempts int32
		units := &fakeLookup{m: map[string]Process{}}
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			// Simulate the unit being deleted (untracked) after its second failed
			// reconcile, as Delete would do via s.units.Delete.
			if atomic.AddInt32(&attempts, 1) == 2 {
				units.remove(unit)
			}
			return errTransient
		}}
		units.mu.Lock()
		units.m[unit] = p
		units.mu.Unlock()

		updates := make(chan unitUpdate, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() { reactToUnitEvents(ctx, units, seqFromChan(ctx, updates)); close(done) }()

		updates <- exitUpdate(unit)

		// After deletion the queued name reconciles to a no-op (units.Get is nil),
		// so LoadState is not called again and retries stop well short of the cap.
		if got := stableFor(2*time.Second, 200*time.Millisecond, time.Millisecond, func() int64 {
			return int64(atomic.LoadInt32(&attempts))
		}); got != 2 {
			t.Fatalf("expected reconciles to stop at 2 once the unit was deleted, got %d", got)
		}
		cancel()
		<-done
	})
}

func TestEventReactorResync(t *testing.T) {
	const running = "io-containerd-systemd-ns-run-init.service"
	const missed = "io-containerd-systemd-ns-missed-init.service"

	t.Run("resync recovers an exit that arrived while disconnected", func(t *testing.T) {
		// The unit already exited in systemd, but no event was delivered (the
		// connection was down). resync must reconcile it and pick up the exit.
		p := &fakeProcess{name: missed, loadStateFn: func(f *fakeProcess) error {
			f.setState(pState{ExitCode: 2, ExitedAt: time.Now()})
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{missed: p}}

		r := newEventReactor(units)
		stop := r.start(context.Background())
		defer stop()

		r.resync()

		if !eventually(2*time.Second, time.Millisecond, func() bool { return p.ProcessState().Exited() }) {
			t.Fatal("resync did not reconcile the missed exit")
		}
	})

	t.Run("resync reconciles every tracked unit", func(t *testing.T) {
		a := &fakeProcess{name: running}
		b := &fakeProcess{name: missed}
		units := &fakeLookup{m: map[string]Process{running: a, missed: b}}

		r := newEventReactor(units)
		stop := r.start(context.Background())
		defer stop()

		r.resync()

		if !eventually(2*time.Second, time.Millisecond, func() bool {
			return a.LoadStateCalls() == 1 && b.LoadStateCalls() == 1
		}) {
			t.Fatalf("expected each tracked unit reconciled once, got a=%d b=%d", a.LoadStateCalls(), b.LoadStateCalls())
		}
	})

	t.Run("resync does not re-read an already-exited unit", func(t *testing.T) {
		p := &fakeProcess{name: missed, state: pState{ExitCode: 1, ExitedAt: time.Now()}}
		units := &fakeLookup{m: map[string]Process{missed: p}}

		r := newEventReactor(units)
		stop := r.start(context.Background())
		defer stop()

		r.resync()

		// Give a reconcile the chance to (wrongly) run, then assert it didn't read.
		if got := stableFor(time.Second, 200*time.Millisecond, time.Millisecond, func() int64 {
			return int64(p.LoadStateCalls())
		}); got != 0 {
			t.Fatalf("expected no read for an already-exited unit, got %d", got)
		}
	})
}

// --- helpers ---

// eventually polls cond until it returns true or timeout elapses, sleeping step
// between checks. It returns true as soon as cond passes, so a passing check
// returns promptly; only a failing check runs to the full deadline.
func eventually(timeout, step time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(step)
	}
}

// stableFor polls value until it stops changing for debounce, then returns the
// settled value; if value keeps changing it runs until timeout and returns the
// last observation. A value that settles quickly returns quickly.
func stableFor(timeout, debounce, step time.Duration, value func() int64) int64 {
	deadline := time.Now().Add(timeout)
	last := value()
	stableSince := time.Now()
	for {
		if time.Since(stableSince) >= debounce {
			return last
		}
		if !time.Now().Before(deadline) {
			return last
		}
		time.Sleep(step)
		if v := value(); v != last {
			last = v
			stableSince = time.Now()
		}
	}
}

func changedProps(kv map[string]string) map[string]dbus.Variant {
	changed := make(map[string]dbus.Variant, len(kv))
	for k, v := range kv {
		changed[k] = dbus.MakeVariant(v)
	}
	return changed
}

func exitUpdate(unit string) unitUpdate {
	return newUpdate(unit, changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"}))
}

// newUpdate builds a unitUpdate the way the decoder does: the reactor keys off
// the systemd-escaped object-path base, so tests escape the real unit name here
// rather than hand-writing escaped paths.
func newUpdate(unit string, changed map[string]dbus.Variant) unitUpdate {
	return unitUpdate{pathBase: systemd.PathBusEscape(unit), changed: changed}
}

// seqFromChan adapts a channel of updates into the iter.Seq the reactor consumes,
// so tests can drive the informer by sending on a channel. It stops when ctx is
// cancelled or the channel is closed.
func seqFromChan(ctx context.Context, ch <-chan unitUpdate) iter.Seq[unitUpdate] {
	return func(yield func(unitUpdate) bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case u, ok := <-ch:
				if !ok {
					return
				}
				if !yield(u) {
					return
				}
			}
		}
	}
}

// reactToUnitEvents wires a reactor's worker pool and informer loop for a single
// update stream, blocking until the stream ends. It skips the resync that
// runEventReactor performs on connect, so a test drives only the events it sends.
func reactToUnitEvents(ctx context.Context, units processLookup, updates iter.Seq[unitUpdate]) {
	r := newEventReactor(units)
	stop := r.start(ctx)
	defer stop()
	r.consume(ctx, updates)
}

type fakeLookup struct {
	mu sync.Mutex
	m  map[string]Process
}

// GetByPath mirrors unitManager.GetByPath: it resolves a process from a
// systemd-escaped object-path base by matching each tracked process's PathName.
func (l *fakeLookup) GetByPath(pathBase string) Process {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.m {
		if p.PathName() == pathBase {
			return p
		}
	}
	return nil
}

// Paths returns the escaped object-path base of every tracked unit, matching
// what GetByPath accepts.
func (l *fakeLookup) Paths() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	paths := make([]string, 0, len(l.m))
	for _, p := range l.m {
		paths = append(paths, p.PathName())
	}
	return paths
}

func (l *fakeLookup) remove(name string) {
	l.mu.Lock()
	delete(l.m, name)
	l.mu.Unlock()
}

type fakeProcess struct {
	name            string
	mu              sync.Mutex
	state           pState
	loadCalls       int32
	loadExitCalls   int32
	loadStateFn     func(*fakeProcess) error
	loadExitStateFn func(*fakeProcess) error
}

// LoadState mirrors the real implementations, which do NOT hold the process lock
// while performing the (potentially blocking) systemd read -- so the fake must
// not either, or a blocking loadStateFn would deadlock a concurrent
// ProcessState reader.
func (f *fakeProcess) LoadState(context.Context) error {
	atomic.AddInt32(&f.loadCalls, 1)
	if f.loadStateFn != nil {
		return f.loadStateFn(f)
	}
	return nil
}

func (f *fakeProcess) LoadStateCalls() int {
	return int(atomic.LoadInt32(&f.loadCalls))
}

// LoadExitState mirrors the real reconcile-path systemd read. Like LoadState it
// does not hold the process lock while running loadExitStateFn.
func (f *fakeProcess) LoadExitState(context.Context) error {
	atomic.AddInt32(&f.loadExitCalls, 1)
	if f.loadExitStateFn != nil {
		return f.loadExitStateFn(f)
	}
	return nil
}

func (f *fakeProcess) LoadExitStateCalls() int {
	return int(atomic.LoadInt32(&f.loadExitCalls))
}

func (f *fakeProcess) setState(st pState) {
	f.mu.Lock()
	f.state = st
	f.mu.Unlock()
}

func (f *fakeProcess) ProcessState() pState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeProcess) SetState(_ context.Context, st pState) pState {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.CopyTo(&f.state)
	return f.state
}

func (f *fakeProcess) Name() string         { return f.name }
func (f *fakeProcess) PathName() string     { return systemd.PathBusEscape(f.name) }
func (f *fakeProcess) Pid() uint32          { return f.ProcessState().Pid }
func (f *fakeProcess) LogWriter() io.Writer { return io.Discard }

func (f *fakeProcess) Start(context.Context) (uint32, error)     { return 0, nil }
func (f *fakeProcess) ResizePTY(context.Context, int, int) error { return nil }
func (f *fakeProcess) Wait(context.Context) (pState, error)      { return f.ProcessState(), nil }
func (f *fakeProcess) Delete(context.Context) (pState, error)    { return f.ProcessState(), nil }
func (f *fakeProcess) State(context.Context) (*State, error)     { return &State{}, nil }
func (f *fakeProcess) Kill(context.Context, int, bool) error     { return nil }
