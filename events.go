package main

import (
	"context"
	"io"
	"sync"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/sirupsen/logrus"
)

func (s *Service) Forward(ctx context.Context, publisher events.Publisher) {
	for e := range s.events {
		ctx := namespaces.WithNamespace(ctx, e.ns)
		err := publisher.Publish(ctx, GetTopic(e.e), e.e)
		if err != nil {
			logrus.WithError(err).Error("post event")
		}
	}
	if closer, ok := publisher.(io.Closer); ok {
		closer.Close()
	}
	close(s.waitEvents)
}

type eventEnvelope struct {
	ns string
	e  interface{}
}

// eventPublishTimeout bounds how long a task event waits for room in the publish
// queue. It exists so a wedged publisher cannot pin a reactor worker or an RPC
// handler forever; it is not a cancellation path, and is deliberately generous,
// because giving up means containerd never hears about something that happened.
// It is a var so tests can shrink it.
var eventPublishTimeout = 30 * time.Second

// send hands an event to Forward. Its two contexts have distinct jobs. The
// caller's supplies values only -- the unit logger and trace span -- because an
// event records something that already happened, so whether the RPC client that
// happened to be on the stack gave up has no bearing on whether containerd hears
// about it, and the reactor paths have no client at all. The wait is bounded by
// the shim's own lifetime instead, so anything the shim later wants to cancel a
// publish for -- shutdown, a bound on queue processing -- can reach it, which
// severing the tree with WithoutCancel would have made impossible.
func (s *Service) send(ctx context.Context, ns string, e interface{}) bool {
	wait, cancel := context.WithTimeout(s.publishCtx, eventPublishTimeout)
	defer cancel()

	select {
	case s.events <- eventEnvelope{ns, e}:
		return true
	case <-wait.Done():
		log.G(ctx).WithError(wait.Err()).Errorf("Did not publish %T; containerd will not see it", e)
		return false
	}
}

// eventOutbox publishes one process's task events in the order containerd can
// make sense of them.
//
// Each event has a precondition: containerd must have been told the process
// exists before it is told the process is gone, must have been told a task
// started before it is told it exited, and must be told nothing at all once it
// has been told the process is deleted. The shim does not learn these things in
// that order -- the reactor sees a workload die while Start is still running,
// and a Delete can finish while that same Start is still on its way to
// publishing -- so an event whose precondition is not met yet is held, and every
// send re-checks the held ones to publish whatever has since become publishable.
// An event whose precondition is never met is never published: a task deleted
// before it ever ran, or one whose Create failed and tore itself down through
// the ordinary teardown path, is discarded along with whatever it was still
// holding.
//
// Callers hand it every event and let it decide; there is no separate "queue"
// and "publish" entry point to pick between.
//
// It is held by value: every process owns exactly one and never shares it, so
// there is nothing to point at. Copying one after use would copy its mutex,
// which go vet's copylocks check catches.
type eventOutbox struct {
	ns   string
	send func(ctx context.Context, ns string, evt interface{}) bool

	// mu is held across publishing so a flush cannot be overtaken by an event
	// racing it. send is bounded by eventPublishTimeout, so the wait is bounded
	// backpressure on this process alone.
	mu      sync.Mutex
	known   bool // containerd has been told the process exists
	started bool // containerd has been told the task started
	deleted bool // containerd has been told the process is gone
	held    []interface{}
}

func newEventOutbox(ns string, send func(ctx context.Context, ns string, evt interface{}) bool) eventOutbox {
	return eventOutbox{ns: ns, send: send}
}

// Send publishes evt if containerd can make sense of it yet, and holds it until
// it can otherwise.
func (o *eventOutbox) Send(ctx context.Context, evt interface{}) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.held = append(o.held, evt)
	o.flush(ctx)
}

// publishable reports whether evt's precondition is met. o.mu must be held.
func (o *eventOutbox) publishable(evt interface{}) bool {
	if o.deleted {
		// Nothing follows a delete. A Start still on its way to publishing when
		// a Delete completed would otherwise resurrect an untracked task in the
		// event stream, and drag the exit held behind it along too.
		return false
	}
	switch evt.(type) {
	case *eventsapi.TaskExit:
		return o.started
	case *eventsapi.TaskDelete:
		return o.known
	}
	return true
}

// flush publishes every held event whose precondition is now met, oldest first,
// and leaves the rest held. It passes over the queue again for as long as a pass
// publishes something, because publishing one event can be what meets the
// precondition of another queued ahead of it -- which is exactly the
// exit-observed-before-its-start case. It stops when a pass publishes nothing,
// or early when a send fails. o.mu must be held.
func (o *eventOutbox) flush(ctx context.Context) {
	for {
		var (
			held    []interface{}
			sent    bool
			stalled bool
		)
		for i, evt := range o.held {
			if !o.publishable(evt) {
				held = append(held, evt)
				continue
			}
			// An event is recorded only once it has actually been handed over.
			// A create or start that never reached Forward but still counted as
			// published would let a later delete or exit out without the
			// prerequisite containerd never saw.
			if !o.send(ctx, o.ns, evt) {
				held = append(held, o.held[i:]...)
				stalled = true
				break
			}
			o.record(evt)
			sent = true
		}
		o.held = held

		if !sent || stalled {
			return
		}
	}
}

// record notes what publishing evt has told containerd, which is what meets the
// preconditions of later events. o.mu must be held.
func (o *eventOutbox) record(evt interface{}) {
	switch evt.(type) {
	case *eventsapi.TaskCreate, *eventsapi.TaskExecAdded:
		o.known = true
	case *eventsapi.TaskStart, *eventsapi.TaskExecStarted:
		o.started = true
	case *eventsapi.TaskDelete:
		o.deleted = true
	}
}

// GetTopic converts an event from an interface type to the specific
// event topic id
func GetTopic(e interface{}) string {
	switch e.(type) {
	case *eventsapi.TaskCreate:
		return runtime.TaskCreateEventTopic
	case *eventsapi.TaskStart:
		return runtime.TaskStartEventTopic
	case *eventsapi.TaskOOM:
		return runtime.TaskOOMEventTopic
	case *eventsapi.TaskExit:
		return runtime.TaskExitEventTopic
	case *eventsapi.TaskDelete:
		return runtime.TaskDeleteEventTopic
	case *eventsapi.TaskExecAdded:
		return runtime.TaskExecAddedEventTopic
	case *eventsapi.TaskExecStarted:
		return runtime.TaskExecStartedEventTopic
	case *eventsapi.TaskPaused:
		return runtime.TaskPausedEventTopic
	case *eventsapi.TaskResumed:
		return runtime.TaskResumedEventTopic
	case *eventsapi.TaskCheckpointed:
		return runtime.TaskCheckpointedEventTopic
	default:
		logrus.Warnf("no topic for type %#v", e)
	}
	return runtime.TaskUnknownTopic
}
