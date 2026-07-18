package main

import (
	"context"
	"iter"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/godbus/dbus/v5"
)

// processLookup resolves and enumerates tracked processes by their systemd unit
// object-path base (the escaped unit name from a D-Bus signal). It is satisfied
// by *unitManager and lets the event reactor be tested with a fake that never
// touches D-Bus.
type processLookup interface {
	// GetByPath resolves a process from a systemd unit object-path base (the
	// escaped unit name carried by a signal), without unescaping it.
	GetByPath(pathBase string) Process
	// Paths returns the escaped object-path base of every tracked unit -- the
	// keys GetByPath accepts -- for a resync sweep.
	Paths() []string
}

// connected is the subset of *systemd.Conn used by the reconnect watchdog.
type connected interface {
	Connected() bool
}

// watchEvents starts the systemd unit event reactor. It is the shim's sole
// monitor of unit state: it reacts to unit exits immediately over D-Bus and,
// after a reconnect, resyncs tracked units to recover anything missed while
// disconnected.
//
// The reactor reads unit PropertiesChanged signals off a dedicated connection
// itself (see unitsignals.go) rather than via go-systemd's Set*Subscriber, which
// races on its subscriber field and reads unit properties per signal. Reading
// raw signals never reads properties, so it cannot trigger the historical
// GetAll feedback storm. Terminal Service properties are retained from the
// signal itself; a targeted read is only a fallback.
func (s *Service) watchEvents(ctx context.Context) {
	go s.runEventReactor(ctx)
}

// runEventReactor owns a dedicated private connection for the signal stream so
// it can be re-dialed if the connection drops, without disturbing the shared
// connection used for method calls (which every Process references directly).
//
// The work queue and its worker pool live for the whole reactor, while the
// connection (and thus the signal stream) is re-established on drop. Each time a
// connection comes up the reactor resyncs -- enqueuing every tracked unit -- so
// a unit that exited while the connection was down is still reconciled. That
// resync is the missed-event backstop that replaces the old periodic scan.
func (s *Service) runEventReactor(ctx context.Context) {
	const retry = time.Second

	r := newEventReactor(s.units)
	stop := r.start(ctx)
	defer stop()

	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := dialSignalBus(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Warn("event reactor: connect failed; will retry")
			if sleepCtx(ctx, retry) {
				return
			}
			continue
		}

		runCtx, cancel := context.WithCancel(ctx)
		go watchConnection(runCtx, cancel, conn)

		sigs := make(chan *dbus.Signal, signalBufferSize)
		conn.Signal(sigs)

		// Recover any exit missed while we had no connection. At startup this is a
		// no-op because nothing is tracked yet; after a reconnect it catches units
		// that exited during the gap.
		r.resync()

		r.consume(runCtx, unitUpdates(runCtx, sigs))

		cancel()
		conn.Close()

		if ctx.Err() != nil {
			return
		}
		log.G(ctx).Warn("event reactor: signal stream ended, re-dialing")
		if sleepCtx(ctx, retry) {
			return
		}
	}
}

// watchConnection cancels the run context when the signal connection drops
// (Connected() reports false), so runEventReactor can re-dial.
func watchConnection(ctx context.Context, onLost func(), conn connected) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !conn.Connected() {
				log.G(ctx).Warn("event reactor: systemd connection lost")
				onLost()
				return
			}
		}
	}
}

// sleepCtx sleeps for d and reports whether ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) (cancelled bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// eventReconcileWorkers is the number of workers that drain the reconcile queue.
// Because the work queue processes each unit on at most one worker at a time,
// this is effectively the cap on concurrent systemd reads: enough that a slow or
// hung read for one unit does not stall reconciles for others, but small enough
// not to fan a mass exit out into an unbounded burst of reads.
const eventReconcileWorkers = 8

// Retry policy for a failed reconcile (e.g. a transient systemd read error).
// Failures back off base, 2*base, 4*base, ... capped at max, and are given up on
// after maxRetries attempts, at which point the next reconnect resync is the
// backstop. These are vars so tests can shrink them.
var (
	reconcileMaxRetries = 5
	reconcileRetryBase  = 200 * time.Millisecond
	reconcileRetryMax   = 10 * time.Second
)

// eventReactor is the controller half of the event pipeline: a work queue keyed
// by unit name plus a pool of workers that reconcile units. It outlives any
// single D-Bus connection -- the connection (and its subscription) is fed in via
// consume and can be re-established on drop, while the queue and workers persist.
type eventReactor struct {
	units processLookup
	q     *unitWorkQueue
}

func newEventReactor(units processLookup) *eventReactor {
	return &eventReactor{units: units, q: newUnitWorkQueue()}
}

// start launches the worker pool and returns a stop function that shuts the
// queue down and waits for the workers to drain.
func (r *eventReactor) start(ctx context.Context) (stop func()) {
	var wg sync.WaitGroup
	for i := 0; i < eventReconcileWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconcileWorker(ctx, r.q, r.units)
		}()
	}
	return func() {
		r.q.ShutDown()
		wg.Wait()
	}
}

// resync enqueues every tracked unit for reconciliation. It runs when a
// subscription is (re-)established so an exit missed while the connection was
// down is recovered: reconcileUnit re-reads current state, and the Exited()
// guard makes it a no-op for units that are still running or already handled.
func (r *eventReactor) resync() {
	for _, pathBase := range r.units.Paths() {
		r.q.Add(pathBase)
	}
}

// consume is the informer loop: it ranges the decoded update stream until ctx
// is cancelled or the stream ends, enqueuing exit transitions. It does only
// cheap, I/O-free work, so the informer never blocks on a systemd read -- one
// slow or hung reconcile cannot stall event intake or let the signal channel
// back up and drop updates. The work queue coalesces duplicate events for a unit
// and never reconciles the same unit on two workers at once, so multiple events
// for the same unit cannot interfere.
//
// Taking an iter.Seq decouples the informer from the signal source. Context
// cancellation is handled by the sequence, which stops yielding and returns.
func (r *eventReactor) consume(ctx context.Context, updates iter.Seq[unitUpdate]) {
	for u := range updates {
		enqueueIfExit(r.q, r.units, u)
	}
	log.G(ctx).WithError(ctx.Err()).Info("Exiting unit event loop")
}

// enqueueIfExit is the informer predicate: it enqueues a unit for reconciliation
// on a Unit exit transition or a terminal Service update for a process we still
// track and have not already marked exited. A terminal Service update is copied
// into the process before enqueueing, so a worker does not lose the exit code if
// systemd unloads the unit first. Signals for units we do not track are dropped
// by an escaped-name map miss without unescaping the name. The predicate does no
// I/O, so it cannot trigger the GetAll feedback storm.
// Non-exit transitions and trailing events after a recorded exit are dropped.
// The leading pre-start inactive/dead event is still enqueued, but
// reconcileUnit/LoadState is level-driven and no-ops while the process has no
// PID, so it cannot produce a spurious exit.
func enqueueIfExit(q *unitWorkQueue, units processLookup, u unitUpdate) {
	switch u.interfaceName {
	case serviceInterface:
		state, ok := serviceExitState(u.changed)
		if !ok {
			return
		}
		p := units.GetByPath(u.pathBase)
		if p == nil || p.ProcessState().Exited() {
			return
		}
		p.RecordSystemdExitState(state)
	case unitInterface:
		if !unitEventIsExit(u.changed) {
			return
		}
		p := units.GetByPath(u.pathBase)
		if p == nil || p.ProcessState().Exited() {
			return
		}
	default:
		return
	}
	q.Add(u.pathBase)
}

// reconcileWorker drains the queue, reconciling one unit at a time until the
// queue is shut down. A reconcile that returns an error is retried with an
// exponential backoff up to reconcileMaxRetries; a success (or exhausting the
// retries) clears the unit's retry count. The queue key is the unit's escaped
// object-path base; the real name is resolved only for the failure logs.
func reconcileWorker(ctx context.Context, q *unitWorkQueue, units processLookup) {
	for {
		pathBase, shutdown := q.Get()
		if shutdown {
			return
		}
		err := reconcileUnit(ctx, units, pathBase)
		q.Done(pathBase)

		if err == nil {
			q.Forget(pathBase)
			continue
		}

		// Log with the real unit name when the unit is still tracked.
		unit := pathBase
		if p := units.GetByPath(pathBase); p != nil {
			unit = p.Name()
		}
		if q.Retry(pathBase, reconcileMaxRetries, reconcileRetryBase, reconcileRetryMax) {
			log.G(ctx).WithError(err).WithField("unit", unit).Debug("Reconcile failed; scheduled retry")
		} else {
			q.Forget(pathBase)
			log.G(ctx).WithError(err).WithField("unit", unit).Warn("Giving up reconciling unit after retries; a reconnect resync will recover it")
		}
	}
}

// reconcileUnit brings the shim's cached state for a unit up to date with
// systemd. It is level-driven: rather than acting on a specific event it reads
// current state, so coalesced or repeated events converge on the same result and
// SetState emits at most one TaskExit. It returns an error only when the state
// read itself failed, so the caller can retry; an untracked or already-exited
// unit is nothing to do and returns nil.
//
// pathBase is the unit's escaped object-path base, resolved through the reactor's
// index; the systemd read inside LoadState uses the process's own name, so the
// key is only a lookup handle here.
//
// LoadState reads the persisted exit file first. If it only contains a running
// state, LoadExitState consumes terminal properties retained from the Service
// signal. A targeted GetAll on our own unit is the fallback for a Unit exit
// signal that arrived without a terminal Service update.
func reconcileUnit(ctx context.Context, units processLookup, pathBase string) error {
	p := units.GetByPath(pathBase)
	if p == nil {
		// Unit is untracked or was deleted between enqueue and reconcile.
		return nil
	}
	if p.ProcessState().Exited() {
		// Already reconciled by an earlier event or a resync.
		return nil
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("unit", p.Name()))
	ctx = WithShimLog(ctx, p.LogWriter())

	if err := p.LoadState(ctx); err != nil {
		return err
	}

	// Create normally persists only a running state. When it is not terminal for
	// a started process, consume the Service signal's terminal state or fall back
	// to the still-loaded unit. A Pid of 0 means the unit never started (the
	// leading pre-start inactive/dead event), so there is nothing to read.
	if !p.ProcessState().Exited() && p.Pid() > 0 {
		if err := p.LoadExitState(ctx); err != nil {
			return err
		}
	}
	log.G(ctx).WithField("state", p.ProcessState()).Debug("Reconciled unit after exit event")
	return nil
}

// unitEventIsExit reports whether a Unit-interface PropertiesChanged payload
// indicates the unit has finished running. ActiveState "inactive" or "failed"
// is the reliable terminal signal; "activating"/"deactivating"/"active" are
// transient or running states. SubState alone is ambiguous (e.g. oneshot units
// report SubState "exited" while ActiveState stays "active").
func unitEventIsExit(changed map[string]dbus.Variant) bool {
	v, ok := changed["ActiveState"]
	if !ok {
		return false
	}
	switch s, _ := v.Value().(string); s {
	case "inactive", "failed":
		return true
	default:
		return false
	}
}
