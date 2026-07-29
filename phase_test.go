package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
)

func TestProcessPhaseTransitions(t *testing.T) {
	t.Run("a created process can be started", func(t *testing.T) {
		p := newPhasedProcess(t, phaseCreated)

		if err := p.beginStart(); err != nil {
			t.Fatalf("start a created process: %v", err)
		}
		if _, err := p.advance(phaseRunning); err != nil {
			t.Fatalf("finish starting a created process: %v", err)
		}
		if got := p.currentPhase(); got != phaseRunning {
			t.Fatalf("phase after a successful start = %s, want running", got)
		}
	})

	t.Run("a start already in flight rejects a second one", func(t *testing.T) {
		p := newPhasedProcess(t, phaseStarting)

		if err := p.beginStart(); err == nil {
			t.Fatal("a second concurrent start was admitted")
		}
	})

	t.Run("a start can be attempted again after the last one was abandoned", func(t *testing.T) {
		p := newPhasedProcess(t, phaseStarting)

		p.abortStart(context.Background())

		if got := p.currentPhase(); got != phaseCreated {
			t.Fatalf("phase after an abandoned start = %s, want created", got)
		}
		if err := p.beginStart(); err != nil {
			t.Fatalf("retry a failed start: %v", err)
		}
	})

	t.Run("abandoning a start that already exited leaves it exited", func(t *testing.T) {
		p := newPhasedProcess(t, phaseStarting)
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		p.abortStart(context.Background())

		if got := p.currentPhase(); got != phaseExited {
			t.Fatalf("phase after abandoning an exited start = %s, want exited", got)
		}
	})

	t.Run("a running process cannot be started again", func(t *testing.T) {
		p := newPhasedProcess(t, phaseRunning)

		if err := p.beginStart(); err == nil {
			t.Fatal("starting an already running process was allowed")
		}
	})

	t.Run("an exited process cannot be started", func(t *testing.T) {
		p := newPhasedProcess(t, phaseExited)

		if err := p.beginStart(); err == nil {
			t.Fatal("starting an exited process was allowed")
		}
	})

	t.Run("a deleted process cannot be started", func(t *testing.T) {
		p := newPhasedProcess(t, phaseDeleted)

		if err := p.beginStart(); err == nil {
			t.Fatal("starting a deleted process was allowed")
		}
	})

	t.Run("a process that exited while starting cannot finish its start", func(t *testing.T) {
		p := newPhasedProcess(t, phaseStarting)
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		if _, err := p.advance(phaseRunning); err == nil {
			t.Fatal("a process that exited while starting finished its start")
		}
		if got := p.currentPhase(); got != phaseExited {
			t.Fatalf("phase after a rejected start completion = %s, want exited", got)
		}
	})

	t.Run("an exit from running reports that the process had been running", func(t *testing.T) {
		p := newPhasedProcess(t, phaseRunning)

		u := p.applyState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		if !u.wasRunning() {
			t.Fatal("an exit from running did not report the process as having been running")
		}
		if got := p.currentPhase(); got != phaseExited {
			t.Fatalf("phase after an exit = %s, want exited", got)
		}
	})

	t.Run("an exit while starting does not report the process as having been running", func(t *testing.T) {
		p := newPhasedProcess(t, phaseStarting)

		u := p.applyState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		if u.wasRunning() {
			t.Fatal("a process that exited mid-start was reported as having been running")
		}
	})

	for name, phase := range map[string]procPhase{
		"a created process can be deleted without ever running": phaseCreated,
		"a process deleted mid-start can be deleted":            phaseStarting,
		"a running process can be deleted":                      phaseRunning,
		"an exited process can be deleted":                      phaseExited,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPhasedProcess(t, phase)

			p.advance(phaseDeleted)

			if got := p.currentPhase(); got != phaseDeleted {
				t.Fatalf("phase after delete = %s, want deleted", got)
			}
		})
	}

	t.Run("a deleted process rejects every earlier phase", func(t *testing.T) {
		for _, earlier := range []procPhase{phaseCreated, phaseStarting, phaseRunning, phaseExited} {
			p := newPhasedProcess(t, phaseDeleted)

			if _, err := p.advance(earlier); err == nil {
				t.Fatalf("a deleted process was allowed to become %s", earlier)
			}
		}
	})

	t.Run("a rejected transition leaves the phase unchanged", func(t *testing.T) {
		p := newPhasedProcess(t, phaseExited)

		if _, err := p.advance(phaseRunning); err == nil {
			t.Fatal("an exited process was allowed to become running")
		}
		if got := p.currentPhase(); got != phaseExited {
			t.Fatalf("phase after a rejected transition = %s, want exited", got)
		}
	})

	t.Run("a rejected transition reports a failed precondition", func(t *testing.T) {
		p := newPhasedProcess(t, phaseExited)

		err := p.beginStart()
		if !errors.Is(err, errdefs.ErrFailedPrecondition) {
			t.Fatalf("rejected start error = %v, want a failed precondition", err)
		}
		if want := "cannot go from exited to starting"; err.Error() != want {
			t.Fatalf("rejected start error = %q, want %q", err, want)
		}
	})
}

// The exit transition is what makes an exit observable exactly once: every state
// path funnels an exit through SetState, and only one of them can be the call
// that moves the process into the exited phase.
func TestProcessExitTransitionHappensOnce(t *testing.T) {
	exited := pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()}

	t.Run("repeated exit observations report a single exit transition", func(t *testing.T) {
		p := newPhasedProcess(t, phaseRunning)

		var claimed int
		for i := 0; i < 4; i++ {
			if p.applyState(context.Background(), exited).justExited() {
				claimed++
			}
		}

		if claimed != 1 {
			t.Fatalf("exit transitions claimed = %d, want 1", claimed)
		}
	})

	t.Run("concurrent exit observations report a single exit transition", func(t *testing.T) {
		p := newPhasedProcess(t, phaseRunning)

		var claimed int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if p.applyState(context.Background(), exited).justExited() {
					atomic.AddInt32(&claimed, 1)
				}
			}()
		}
		wg.Wait()

		if claimed != 1 {
			t.Fatalf("exit transitions claimed under concurrency = %d, want 1", claimed)
		}
	})

	t.Run("an exit observed after delete is not claimed", func(t *testing.T) {
		p := newPhasedProcess(t, phaseDeleted)

		if p.applyState(context.Background(), exited).justExited() {
			t.Fatal("an exit observed after delete was claimed")
		}
	})
}

// These start a process with no runc configured, so a start that is not rejected
// up front would panic rather than quietly succeed.
func TestStartRejectsAProcessThatCannotRun(t *testing.T) {
	t.Run("starting an exited container reports a failed precondition", func(t *testing.T) {
		p, _ := newTestInitProcess("container")
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})

		_, err := p.Start(context.Background())
		if !errors.Is(err, errdefs.ErrFailedPrecondition) {
			t.Fatalf("start of an exited container = %v, want a failed precondition", err)
		}
	})

	t.Run("starting an exited exec reports a failed precondition", func(t *testing.T) {
		parent, _ := newTestInitProcess("container")
		ep := newTestExecProcess(parent, "exec1")
		ep.SetState(context.Background(), pState{Pid: 99, ExitCode: 1, ExitedAt: time.Now()})

		_, err := ep.Start(context.Background())
		if !errors.Is(err, errdefs.ErrFailedPrecondition) {
			t.Fatalf("start of an exited exec = %v, want a failed precondition", err)
		}
	})

	t.Run("starting a running container reports a failed precondition", func(t *testing.T) {
		p, _ := newTestInitProcess("container")
		runTestProcess(t, p.process)

		_, err := p.Start(context.Background())
		if !errors.Is(err, errdefs.ErrFailedPrecondition) {
			t.Fatalf("start of a running container = %v, want a failed precondition", err)
		}
	})
}

// newPhasedProcess builds a process sitting in the given phase, driving it there
// through the same transitions the shim uses rather than assigning the field.
func newPhasedProcess(t *testing.T, phase procPhase) *process {
	t.Helper()

	p := &process{id: "test"}
	p.cond = sync.NewCond(&p.mu)

	switch phase {
	case phaseCreated:
	case phaseStarting:
		if err := p.beginStart(); err != nil {
			t.Fatalf("reach %s: %v", phase, err)
		}
	case phaseRunning:
		runTestProcess(t, p)
	case phaseExited:
		runTestProcess(t, p)
		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 1, ExitedAt: time.Now()})
	case phaseDeleted:
		p.advance(phaseDeleted)
	default:
		t.Fatalf("unknown phase %s", phase)
	}

	if got := p.currentPhase(); got != phase {
		t.Fatalf("phase = %s, want %s", got, phase)
	}
	return p
}
