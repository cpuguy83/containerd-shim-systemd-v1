package main

import (
	"context"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// processLookup resolves a tracked process by its systemd unit name.
// It is satisfied by *unitManager and lets the event reactor be tested with a
// fake that never touches D-Bus.
type processLookup interface {
	Get(name string) Process
}

// connected is the subset of *systemd.Conn used by the reconnect watchdog.
type connected interface {
	Connected() bool
}

// watchEvents starts the systemd unit event reactor. It reacts to unit exits
// immediately, in parallel with the periodic reconcile in Watch (see state.go);
// both funnel through the idempotent Process.SetState, so a single real exit
// still yields at most one TaskExit.
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
// The 1-minute reconcile in Watch is the backstop for anything missed while
// re-dialing.
func (s *Service) runEventReactor(ctx context.Context) {
	const retry = time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := systemd.NewSystemdConnectionContext(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Warn("event reactor: connect failed; reconcile scan is the backstop")
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

		reactToUnitEvents(runCtx, s.units, updates, errs)

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
// after maxRetries attempts, at which point the periodic scan in Watch is the
// backstop. These are vars so tests can shrink them.
var (
	reconcileMaxRetries = 5
	reconcileRetryBase  = 200 * time.Millisecond
	reconcileRetryMax   = 10 * time.Second
)

// reactToUnitEvents runs the event reactor as a controller: the loop below is
// the informer -- it consumes the property-change stream and does only cheap,
// I/O-free work, translating each relevant exit event into a unit name on the
// work queue. A pool of workers drains that queue and performs the blocking
// state read (the reconcile).
//
// Splitting the two means the informer never blocks on a systemd read, so one
// slow or hung read cannot stall event intake or let the subscription's bounded
// channel back up and drop updates. The work queue coalesces duplicate events
// for a unit and never reconciles the same unit on two workers at once, so
// multiple events for the same unit cannot interfere.
func reactToUnitEvents(ctx context.Context, units processLookup, updates <-chan *systemd.PropertiesUpdate, errs <-chan error) {
	q := newUnitWorkQueue()

	var wg sync.WaitGroup
	for i := 0; i < eventReconcileWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconcileWorker(ctx, q, units)
		}()
	}
	// Ordered so ShutDown runs first (waking the workers) and then we wait for
	// them to finish.
	defer wg.Wait()
	defer q.ShutDown()

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
			enqueueIfExit(q, units, u)
		}
	}
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
			log.G(ctx).WithError(err).WithField("unit", name).Warn("Giving up reconciling unit after retries; periodic scan will recover it")
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
		// Already reconciled by an earlier event or the periodic scan.
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
