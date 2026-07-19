package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	eventsapi "github.com/containerd/containerd/api/events"
)

func TestProcessLifecycleStatus(t *testing.T) {
	t.Run("systemd running state remains created until Start succeeds", func(t *testing.T) {
		p := &process{}
		p.cond = sync.NewCond(&p.mu)

		state := p.SetState(context.Background(), pState{Pid: 42, Status: "running"})
		if state.Status != "created" {
			t.Fatalf("status before Start = %q, want created", state.Status)
		}

		p.markStarted()
		state = p.SetState(context.Background(), pState{Pid: 42, Status: "running"})
		if state.Status != "running" {
			t.Fatalf("status after Start = %q, want running", state.Status)
		}
	})
}

// These tests exercise the real initProcess/execProcess SetState, so the
// exactly-once TaskExit guarantee is verified against the actual emit path, not
// a fake. Every observed exit funnels through SetState, so a single exit must
// yield a single TaskExit even when observed more than once or concurrently.

func TestInitProcessTaskExitIsEmittedOnce(t *testing.T) {
	exited := pState{Pid: 42, ExitCode: 1}

	t.Run("repeated SetState calls for one exit emit a single TaskExit", func(t *testing.T) {
		p, exits := newTestInitProcess("c1")

		for i := 0; i < 4; i++ {
			p.SetState(context.Background(), exited)
		}

		if got := exits(); got != 1 {
			t.Fatalf("expected exactly one TaskExit, got %d", got)
		}
	})

	t.Run("concurrent SetState calls for one exit emit a single TaskExit", func(t *testing.T) {
		p, exits := newTestInitProcess("c2")

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.SetState(context.Background(), exited)
			}()
		}
		wg.Wait()

		if got := exits(); got != 1 {
			t.Fatalf("expected exactly one TaskExit under concurrency, got %d", got)
		}
	})

	t.Run("an init helper exit does not emit a TaskExit", func(t *testing.T) {
		p, exits := newTestInitProcess("c3")

		p.SetState(context.Background(), pState{Pid: 42, Status: exitedInit})

		if got := exits(); got != 0 {
			t.Fatalf("expected no TaskExit for an init-helper exit, got %d", got)
		}
	})
}

func TestExecProcessTaskExitIsEmittedOnce(t *testing.T) {
	exited := pState{Pid: 99, ExitCode: 7}

	t.Run("repeated SetState calls for one exec exit emit a single TaskExit", func(t *testing.T) {
		parent, exits := newTestInitProcess("c4")
		ep := newTestExecProcess(parent, "exec1")

		for i := 0; i < 4; i++ {
			ep.SetState(context.Background(), exited)
		}

		if got := exits(); got != 1 {
			t.Fatalf("expected exactly one exec TaskExit, got %d", got)
		}
	})

	t.Run("concurrent SetState calls for one exec exit emit a single TaskExit", func(t *testing.T) {
		parent, exits := newTestInitProcess("c5")
		ep := newTestExecProcess(parent, "exec1")

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ep.SetState(context.Background(), exited)
			}()
		}
		wg.Wait()

		if got := exits(); got != 1 {
			t.Fatalf("expected exactly one exec TaskExit under concurrency, got %d", got)
		}
	})
}

// --- helpers ---

// newTestInitProcess builds an initProcess wired with a TaskExit counter. The
// returned func reports how many TaskExit events have been emitted so far.
func newTestInitProcess(id string) (*initProcess, func() int32) {
	var taskExits int32
	p := &initProcess{
		process: &process{ns: "testns", id: id},
		execs:   &processManager{ls: make(map[string]Process)},
		shimLog: io.Discard,
		sendEvent: func(_ context.Context, _ string, evt interface{}) {
			if _, ok := evt.(*eventsapi.TaskExit); ok {
				atomic.AddInt32(&taskExits, 1)
			}
		},
	}
	p.cond = sync.NewCond(&p.mu)
	p.markStartEventPublished()
	return p, func() int32 { return atomic.LoadInt32(&taskExits) }
}

func newTestExecProcess(parent *initProcess, execID string) *execProcess {
	ep := &execProcess{
		process: &process{ns: parent.ns, id: execID},
		parent:  parent,
		execID:  execID,
	}
	ep.cond = sync.NewCond(&ep.mu)
	ep.markStartEventPublished()
	return ep
}
