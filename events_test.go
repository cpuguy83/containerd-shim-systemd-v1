package main

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
)

func TestLifecycleEventsFollowTransitions(t *testing.T) {
	t.Run("a start is published before an exit observed while starting", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c1")
		if err := p.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}

		// The reactor sees the workload die before Start returns.
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})
		if got := events(); len(got) != 0 {
			t.Fatalf("events published while starting = %v, want none held back", got)
		}

		p.finishStart(context.Background(), 42)

		assertEventTypes(t, events(), &eventsapi.TaskStart{}, &eventsapi.TaskExit{})
	})

	t.Run("an exit after the start is published immediately", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c2")
		if err := p.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}
		p.finishStart(context.Background(), 42)

		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		assertEventTypes(t, events(), &eventsapi.TaskStart{}, &eventsapi.TaskExit{})
	})

	t.Run("an exit held behind a start that never came is not published", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c3")
		if err := p.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}

		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		if got := events(); len(got) != 0 {
			t.Fatalf("events for a task that never started = %v, want none", got)
		}
	})

	t.Run("deleting a container publishes a task delete", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c4")
		p.events.Send(context.Background(), &eventsapi.TaskCreate{ContainerID: "c4"})

		p.finishDelete(context.Background())

		assertEventTypes(t, events(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("a container containerd was never told about publishes no task delete", func(t *testing.T) {
		// A Create that fails tears its process down through the same teardown
		// path, having never published a task create.
		p, events := newRecordingInitProcess(t, "c4b")

		p.finishDelete(context.Background())

		if got := events(); len(got) != 0 {
			t.Fatalf("events for an unannounced container = %v, want none", got)
		}
	})

	t.Run("a container deleted before it ever started still publishes a task delete", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c5")
		p.events.Send(context.Background(), &eventsapi.TaskCreate{ContainerID: "c5"})
		if err := p.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		p.finishDelete(context.Background())

		// The exit stays held -- containerd was never told it started -- but the
		// delete is not gated on a start that will never come.
		assertEventTypes(t, events(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("a repeated delete publishes a single task delete", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c6")
		p.events.Send(context.Background(), &eventsapi.TaskCreate{ContainerID: "c6"})

		p.finishDelete(context.Background())
		p.finishDelete(context.Background())

		assertEventTypes(t, events(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("a task delete carries the recorded exit", func(t *testing.T) {
		p, events := newRecordingInitProcess(t, "c7")
		p.events.Send(context.Background(), &eventsapi.TaskCreate{ContainerID: "c7"})
		exitedAt := time.Now()
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 137, ExitedAt: exitedAt, Status: "exited"})

		p.finishDelete(context.Background())

		got := events()
		if len(got) != 2 {
			t.Fatalf("events = %v, want a task create and a task delete", eventTypes(got))
		}
		del, ok := got[1].(*eventsapi.TaskDelete)
		if !ok {
			t.Fatalf("event = %T, want *events.TaskDelete", got[1])
		}
		if del.ContainerID != "c7" || del.Pid != 42 || del.ExitStatus != 137 {
			t.Fatalf("task delete = %+v, want container c7, pid 42, status 137", del)
		}
		if !del.ExitedAt.AsTime().Equal(exitedAt.UTC()) {
			t.Fatalf("task delete exited at %s, want %s", del.ExitedAt.AsTime(), exitedAt.UTC())
		}
	})

	t.Run("deleting an exec publishes no task delete", func(t *testing.T) {
		parent, events := newRecordingInitProcess(t, "c8")
		ep := newRecordingExecProcess(parent, "exec1")

		ep.advance(phaseDeleted)

		if got := events(); len(got) != 0 {
			t.Fatalf("events for an exec delete = %v, want none", got)
		}
	})

	t.Run("an exec start is published before an exit observed while starting", func(t *testing.T) {
		parent, events := newRecordingInitProcess(t, "c9")
		ep := newRecordingExecProcess(parent, "exec1")
		if err := ep.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}

		ep.SetState(context.Background(), pState{Pid: 99, ExitCode: 2, ExitedAt: time.Now()})
		ep.finishStart(context.Background(), 99)

		assertEventTypes(t, events(), &eventsapi.TaskExecStarted{}, &eventsapi.TaskExit{})
	})
}

// Each event has its own precondition, so a send releases whatever has become
// publishable rather than assuming one arrival unblocks everything held.
func TestEventOutboxReleasesOnlyWhatIsPublishable(t *testing.T) {
	ctx := context.Background()

	t.Run("a create releases a held delete and leaves a held exit alone", func(t *testing.T) {
		o, published := newRecordingOutbox()
		o.Send(ctx, &eventsapi.TaskExit{})
		o.Send(ctx, &eventsapi.TaskDelete{})
		if got := published(); len(got) != 0 {
			t.Fatalf("published %v before either precondition was met, want none", eventTypes(got))
		}

		o.Send(ctx, &eventsapi.TaskCreate{})

		assertEventTypes(t, published(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("a start releases a held exit and leaves a held delete alone", func(t *testing.T) {
		o, published := newRecordingOutbox()
		o.Send(ctx, &eventsapi.TaskExit{})
		o.Send(ctx, &eventsapi.TaskDelete{})

		o.Send(ctx, &eventsapi.TaskStart{})

		assertEventTypes(t, published(), &eventsapi.TaskStart{}, &eventsapi.TaskExit{})
	})

	t.Run("each precondition releases its own event as it is met", func(t *testing.T) {
		o, published := newRecordingOutbox()
		o.Send(ctx, &eventsapi.TaskExit{})
		o.Send(ctx, &eventsapi.TaskDelete{})

		o.Send(ctx, &eventsapi.TaskStart{})
		o.Send(ctx, &eventsapi.TaskCreate{})

		assertEventTypes(t, published(),
			&eventsapi.TaskStart{}, &eventsapi.TaskExit{},
			&eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("nothing is published after a delete", func(t *testing.T) {
		o, published := newRecordingOutbox()
		o.Send(ctx, &eventsapi.TaskCreate{})
		o.Send(ctx, &eventsapi.TaskDelete{})

		// A Start still on its way to publishing when the Delete completed.
		o.Send(ctx, &eventsapi.TaskStart{})
		o.Send(ctx, &eventsapi.TaskExit{})

		assertEventTypes(t, published(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("a delete does not release the exit held behind a start", func(t *testing.T) {
		o, published := newRecordingOutbox()
		o.Send(ctx, &eventsapi.TaskCreate{})
		o.Send(ctx, &eventsapi.TaskExit{})

		o.Send(ctx, &eventsapi.TaskDelete{})
		o.Send(ctx, &eventsapi.TaskStart{})

		assertEventTypes(t, published(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})
}

// A send that is not delivered must not count as published: recording its
// prerequisite would let a later event out without the one containerd never saw.
func TestEventOutboxHoldsUndeliveredEvents(t *testing.T) {
	ctx := context.Background()

	t.Run("an undelivered create does not let a later delete out", func(t *testing.T) {
		o, published, deliver := newBlockedOutbox()
		deliver(false)
		o.Send(ctx, &eventsapi.TaskCreate{})
		if got := published(); len(got) != 0 {
			t.Fatalf("published %v while delivery was failing, want none", eventTypes(got))
		}

		deliver(true)
		o.Send(ctx, &eventsapi.TaskDelete{})

		// The create is retried first. Had the dropped one counted as published,
		// the delete would have gone out on its own.
		assertEventTypes(t, published(), &eventsapi.TaskCreate{}, &eventsapi.TaskDelete{})
	})

	t.Run("an undelivered start does not let a later exit out", func(t *testing.T) {
		o, published, deliver := newBlockedOutbox()
		deliver(false)
		o.Send(ctx, &eventsapi.TaskStart{})
		if got := published(); len(got) != 0 {
			t.Fatalf("published %v while delivery was failing, want none", eventTypes(got))
		}

		deliver(true)
		o.Send(ctx, &eventsapi.TaskExit{})

		assertEventTypes(t, published(), &eventsapi.TaskStart{}, &eventsapi.TaskExit{})
	})
}

// An event records something that already happened, so the caller giving up must
// not stop it reaching containerd -- but the shim's own cancellation, and the
// publish timeout, still have to be able to end the wait.
func TestServiceSendIsBoundToTheShimNotTheCaller(t *testing.T) {
	t.Run("a cancelled caller still publishes", func(t *testing.T) {
		s := &Service{events: make(chan eventEnvelope, 1), publishCtx: context.Background()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if !s.send(ctx, "testns", &eventsapi.TaskExit{}) {
			t.Fatal("a cancelled caller stopped an event being published")
		}

		select {
		case envelope := <-s.events:
			if _, ok := envelope.e.(*eventsapi.TaskExit); !ok {
				t.Fatalf("queued %T, want *events.TaskExit", envelope.e)
			}
		default:
			t.Fatal("nothing was queued")
		}
	})

	t.Run("a shim that is shutting down ends the wait", func(t *testing.T) {
		// An unbuffered queue with nothing draining it stands in for a publisher
		// that has stopped keeping up.
		shim, shutdown := context.WithCancel(context.Background())
		s := &Service{events: make(chan eventEnvelope), publishCtx: shim}

		done := make(chan bool, 1)
		go func() { done <- s.send(context.Background(), "testns", &eventsapi.TaskExit{}) }()
		shutdown()

		select {
		case sent := <-done:
			if sent {
				t.Fatal("send reported success with nothing draining the queue")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("send could not be cancelled by the shim shutting down")
		}
	})

	t.Run("a publisher that stops draining times out rather than blocking", func(t *testing.T) {
		restore := eventPublishTimeout
		eventPublishTimeout = 10 * time.Millisecond
		t.Cleanup(func() { eventPublishTimeout = restore })

		// An unbuffered queue with nothing draining it stands in for a publisher
		// that has stopped keeping up.
		s := &Service{events: make(chan eventEnvelope), publishCtx: context.Background()}

		done := make(chan bool, 1)
		go func() { done <- s.send(context.Background(), "testns", &eventsapi.TaskExit{}) }()

		select {
		case sent := <-done:
			if sent {
				t.Fatal("send reported success with nothing draining the queue")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("send blocked past its timeout")
		}
	})
}

func newRecordingOutbox() (*eventOutbox, func() []interface{}) {
	o, published, _ := newBlockedOutbox()
	return o, published
}

// newBlockedOutbox returns an outbox whose delivery can be turned on and off, to
// stand in for Service.send dropping an event on a canceled request context. It
// starts out delivering.
func newBlockedOutbox() (*eventOutbox, func() []interface{}, func(bool)) {
	var mu sync.Mutex
	var published []interface{}
	delivering := true

	o := newEventOutbox("testns", func(_ context.Context, _ string, evt interface{}) bool {
		mu.Lock()
		defer mu.Unlock()
		if !delivering {
			return false
		}
		published = append(published, evt)
		return true
	})

	return &o, func() []interface{} {
			mu.Lock()
			defer mu.Unlock()
			return append([]interface{}(nil), published...)
		}, func(on bool) {
			mu.Lock()
			delivering = on
			mu.Unlock()
		}
}

// discardEvents is an outbox for tests that exercise a real process but do not
// care what it publishes.
func discardEvents() eventOutbox {
	return newEventOutbox("testns", func(context.Context, string, interface{}) bool { return true })
}

// newRecordingExecProcess builds an exec with its own outbox feeding the
// parent's recorder, as Service.Exec does. Nothing has been published for it
// yet.
func newRecordingExecProcess(parent *initProcess, execID string) *execProcess {
	ep := &execProcess{
		process: &process{ns: parent.ns, id: execID, events: newEventOutbox(parent.ns, parent.events.send)},
		parent:  parent,
		execID:  execID,
	}
	ep.cond = sync.NewCond(&ep.mu)
	return ep
}

// newRecordingInitProcess builds an initProcess whose outbox records every event
// it publishes, in order. Nothing has been reported to containerd yet, so the
// outbox behaves exactly as it does in production.
func newRecordingInitProcess(t *testing.T, id string) (*initProcess, func() []interface{}) {
	t.Helper()

	var mu sync.Mutex
	var events []interface{}
	send := func(_ context.Context, _ string, evt interface{}) bool {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
		return true
	}

	p := &initProcess{
		process: &process{ns: "testns", id: id, events: newEventOutbox("testns", send)},
		execs:   &processManager{ls: make(map[string]Process)},
		shimLog: io.Discard,
	}
	p.cond = sync.NewCond(&p.mu)

	return p, func() []interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]interface{}(nil), events...)
	}
}

func assertEventTypes(t *testing.T, got []interface{}, want ...interface{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("published %v, want %v", eventTypes(got), eventTypes(want))
	}
	for i := range want {
		if eventTypeName(got[i]) != eventTypeName(want[i]) {
			t.Fatalf("published %v, want %v", eventTypes(got), eventTypes(want))
		}
	}
}

func eventTypes(evts []interface{}) []string {
	names := make([]string, 0, len(evts))
	for _, e := range evts {
		names = append(names, eventTypeName(e))
	}
	return names
}

func eventTypeName(e interface{}) string {
	switch e.(type) {
	case *eventsapi.TaskCreate:
		return "TaskCreate"
	case *eventsapi.TaskExecAdded:
		return "TaskExecAdded"
	case *eventsapi.TaskStart:
		return "TaskStart"
	case *eventsapi.TaskExecStarted:
		return "TaskExecStarted"
	case *eventsapi.TaskExit:
		return "TaskExit"
	case *eventsapi.TaskDelete:
		return "TaskDelete"
	default:
		return "unknown"
	}
}
