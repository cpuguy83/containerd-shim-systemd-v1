package main

import (
	"context"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// processLookup resolves and enumerates tracked processes by systemd unit name.
// It is satisfied by *unitManager and lets the event reactor be tested with a
// fake that never touches D-Bus.
type processLookup interface {
	Get(name string) Process
	// Names returns the unit names of every currently tracked process.
	Names() []string
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
// It deliberately uses SetPropertiesSubscriber and NOT SetSubStateSubscriber:
// go-systemd's substate subscriber calls GetUnitPathProperties (a GetAll) for
// every unit signal seen on the connection. On the direct private bus that read
// triggers UnitNew/UnitRemoved feedback which, compounded per event, pegs a CPU
// core (the historical "event storm", commit 910fe5a). The properties
// subscriber never reads, so dispatch performs no GetAll on our behalf; we do a
// single targeted read only for our own units, only when they exit.
func (s *Service) watchEvents(ctx context.Context) {
	go s.runEventReactor(ctx)
}

// runEventReactor owns a dedicated private connection for the subscription so it
// can be re-dialed if the connection drops, without disturbing the shared
// connection used for method calls (which every Process references directly).
//
// The work queue and its worker pool live for the whole reactor, while the
// connection (and thus the subscription) is re-established on drop. Each time a
// subscription comes up the reactor resyncs -- enqueuing every tracked unit --
// so a unit that exited while the connection was down is still reconciled. That
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

		conn, err := systemd.NewSystemdConnectionContext(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Warn("event reactor: connect failed; will retry")
			if sleepCtx(ctx, retry) {
				return
			}
			continue
		}

		runCtx, cancel := context.WithCancel(ctx)
		go watchConnection(runCtx, cancel, conn)

		updates := make(chan *systemd.PropertiesUpdate, 256)
		errs := make(chan error, 256)
		conn.SetPropertiesSubscriber(updates, errs)

		// Recover any exit missed while we had no subscription. At startup this
		// is a no-op because nothing is tracked yet; after a reconnect it catches
		// units that exited during the gap.
		r.resync()

		r.consume(runCtx, updates, errs)

		cancel()
		conn.Close()

		if ctx.Err() != nil {
			return
		}
		log.G(ctx).Warn("event reactor: subscription ended, re-dialing")
		if sleepCtx(ctx, retry) {
			return
		}
	}
}

// watchConnection cancels the run context when the subscription connection drops
// (Conn.Connected() reports false), so runEventReactor can re-dial.
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
	for _, name := range r.units.Names() {
		r.q.Add(name)
	}
}

// consume is the informer loop: it drains the property-change stream until ctx
// is cancelled or the channels close, enqueuing exit transitions. It does only
// cheap, I/O-free work, so the informer never blocks on a systemd read -- one
// slow or hung reconcile cannot stall event intake or let the subscription's
// bounded channel back up and drop updates. The work queue coalesces duplicate
// events for a unit and never reconciles the same unit on two workers at once,
// so multiple events for the same unit cannot interfere.
func (r *eventReactor) consume(ctx context.Context, updates <-chan *systemd.PropertiesUpdate, errs <-chan error) {
	for {
		select {
		case <-ctx.Done():
			log.G(ctx).WithError(ctx.Err()).Info("Exiting unit event loop")
			return
		case err, ok := <-errs:
			if !ok {
				return
			}
			if err != nil {
				log.G(ctx).WithError(err).Debug("systemd property subscription error")
			}
		case u, ok := <-updates:
			if !ok {
				return
			}
			enqueueIfExit(r.q, r.units, u)
		}
	}
}

// reactToUnitEvents wires a reactor's worker pool and informer loop for a single
// subscription, blocking until ctx is cancelled or the channels close. It does
// not resync (production drives that from runEventReactor on connect); it exists
// so the informer/worker behaviour can be exercised in tests without a
// connection.
func reactToUnitEvents(ctx context.Context, units processLookup, updates <-chan *systemd.PropertiesUpdate, errs <-chan error) {
	r := newEventReactor(units)
	stop := r.start(ctx)
	defer stop()
	r.consume(ctx, updates, errs)
}

// enqueueIfExit is the informer predicate: it enqueues a unit for reconciliation
// only on an exit transition for a unit we still track and have not already
// marked exited. It does no I/O (the ProcessState read is an in-memory lock), so
// it cannot trigger the GetAll feedback storm, and it keeps three sources of
// noise off the queue:
//
//   - non-exit transitions (the flood of activating/running changes) and other
//     units' signals -- the private bus broadcasts every unit's changes;
//   - trailing exit events after we have already handled the exit -- a normal
//     lifecycle emits two exit-matching events (a leading inactive/dead before
//     the unit starts, then the real inactive/failed), plus more on restarts;
//   - the leading pre-start inactive/dead is still enqueued (the process is not
//     yet exited), but reconcileUnit/LoadState is level-driven and no-ops for a
//     not-yet-started process, so it cannot produce a spurious exit.
func enqueueIfExit(q *unitWorkQueue, units processLookup, u *systemd.PropertiesUpdate) {
	if !unitEventIsExit(u.Changed) {
		return
	}
	p := units.Get(u.UnitName)
	if p == nil || p.ProcessState().Exited() {
		return
	}
	q.Add(u.UnitName)
}

// reconcileWorker drains the queue, reconciling one unit at a time until the
// queue is shut down. A reconcile that returns an error is retried with an
// exponential backoff up to reconcileMaxRetries; a success (or exhausting the
// retries) clears the unit's retry count.
func reconcileWorker(ctx context.Context, q *unitWorkQueue, units processLookup) {
	for {
		name, shutdown := q.Get()
		if shutdown {
			return
		}
		err := reconcileUnit(ctx, units, name)
		q.Done(name)

		switch {
		case err == nil:
			q.Forget(name)
		case q.Retry(name, reconcileMaxRetries, reconcileRetryBase, reconcileRetryMax):
			log.G(ctx).WithError(err).WithField("unit", name).Debug("Reconcile failed; scheduled retry")
		default:
			q.Forget(name)
			log.G(ctx).WithError(err).WithField("unit", name).Warn("Giving up reconciling unit after retries; a reconnect resync will recover it")
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
// The Unit-interface payload that enqueued this work signals that the unit
// stopped but does not carry the exit code (ExecMainStatus lives on the Service
// interface, which go-systemd does not forward). LoadState reads the persisted
// exit file first and otherwise does a single targeted GetAll on our own,
// still-loaded unit -- safe from the feedback storm because the unit is loaded
// and the read is scoped to us.
func reconcileUnit(ctx context.Context, units processLookup, name string) error {
	p := units.Get(name)
	if p == nil {
		// Unit is untracked or was deleted between enqueue and reconcile.
		return nil
	}
	if p.ProcessState().Exited() {
		// Already reconciled by an earlier event or a resync.
		return nil
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("unit", name))
	ctx = WithShimLog(ctx, p.LogWriter())

	if err := p.LoadState(ctx); err != nil {
		return err
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
