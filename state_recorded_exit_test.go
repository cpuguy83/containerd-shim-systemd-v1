package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLoadExitStateUsesRecordedSystemdState(t *testing.T) {
	t.Run("an exec exit remains available after its systemd unit unloads", func(t *testing.T) {
		parent := &initProcess{process: &process{id: "task", events: discardEvents()}}
		process := &process{id: "exec", events: discardEvents()}
		process.cond = sync.NewCond(&process.mu)
		exec := &execProcess{
			process: process,
			parent:  parent,
			execID:  "exec",
		}
		exitedAt := time.Now()
		exec.RecordSystemdExitState(pState{
			Pid:      42,
			ExitCode: 17,
			ExitedAt: exitedAt,
			Status:   "exited",
		})

		if !exec.LoadRecordedExitState(context.Background()) {
			t.Fatal("no exit state was recorded to load")
		}

		state := exec.ProcessState()
		if state.Pid != 42 || state.ExitCode != 17 || !state.ExitedAt.Equal(exitedAt) || state.Status != "exited" {
			t.Fatalf("state = %s, want pid 42, exit 17, timestamp %s, status exited", state, exitedAt)
		}
	})

	t.Run("a later service exit supersedes an earlier terminal signal", func(t *testing.T) {
		parent := &initProcess{process: &process{id: "task", events: discardEvents()}}
		process := &process{id: "exec", events: discardEvents()}
		process.cond = sync.NewCond(&process.mu)
		exec := &execProcess{
			process: process,
			parent:  parent,
			execID:  "exec",
		}
		earlier := time.Now()
		later := earlier.Add(time.Second)
		exec.RecordSystemdExitState(pState{Pid: 41, ExitCode: 1, ExitedAt: earlier, Status: "exited"})
		exec.RecordSystemdExitState(pState{Pid: 42, ExitCode: 17, ExitedAt: later, Status: "exited"})

		if !exec.LoadRecordedExitState(context.Background()) {
			t.Fatal("no exit state was recorded to load")
		}

		state := exec.ProcessState()
		if state.Pid != 42 || state.ExitCode != 17 || !state.ExitedAt.Equal(later) {
			t.Fatalf("state = %s, want the later pid 42, exit 17, timestamp %s", state, later)
		}
	})

	// The create re-exec is still the unit's main process when it reports, so
	// the ExecMain properties systemd publishes moments later describe the
	// re-exec's own exit. Letting those win reports the wrong code for every
	// workload that exits before systemd can adopt its pid.
	t.Run("a reported exit is not superseded by the unit's later systemd record", func(t *testing.T) {
		exec := newRecordedExitTestExec()
		reported := time.Now()
		exec.RecordHelperExitState(pState{Pid: 42, ExitCode: 19, ExitedAt: reported, Status: "exited"})
		exec.RecordSystemdExitState(pState{Pid: 7, ExitCode: 3, ExitedAt: reported.Add(time.Second), Status: "exited"})

		if !exec.LoadRecordedExitState(context.Background()) {
			t.Fatal("no exit state was recorded to load")
		}

		state := exec.ProcessState()
		if state.Pid != 42 || state.ExitCode != 19 {
			t.Fatalf("state = %s, want the reported pid 42 and exit 19", state)
		}
	})

	t.Run("a reported init failure is not superseded either", func(t *testing.T) {
		exec := newRecordedExitTestExec()
		reported := time.Now()
		exec.RecordHelperExitState(pState{ExitCode: 1, ExitedAt: reported, Status: exitedInit})
		exec.RecordSystemdExitState(pState{Pid: 7, ExitCode: 3, ExitedAt: reported.Add(time.Second), Status: "exited"})

		if !exec.LoadRecordedExitState(context.Background()) {
			t.Fatal("no exit state was recorded to load")
		}

		if state := exec.ProcessState(); state.Status != exitedInit || state.ExitCode != 1 {
			t.Fatalf("state = %s, want status %s and exit 1", state, exitedInit)
		}
	})

	t.Run("restarting a process clears the report that outranked systemd", func(t *testing.T) {
		exec := newRecordedExitTestExec()
		exec.RecordHelperExitState(pState{Pid: 42, ExitCode: 19, ExitedAt: time.Now(), Status: "exited"})
		exec.clearRecordedSystemdExitState()

		if _, ok := exec.loadRecordedSystemdExitState(); ok {
			t.Fatal("a cleared report was still recorded")
		}

		later := time.Now()
		exec.RecordSystemdExitState(pState{Pid: 7, ExitCode: 3, ExitedAt: later, Status: "exited"})
		if state, ok := exec.loadRecordedSystemdExitState(); !ok || state.ExitCode != 3 {
			t.Fatalf("state = %s (recorded %v), want exit 3", state, ok)
		}
	})
}

func newRecordedExitTestExec() *execProcess {
	parent := &initProcess{process: &process{id: "task", events: discardEvents()}}
	process := &process{id: "exec", events: discardEvents()}
	process.cond = sync.NewCond(&process.mu)
	return &execProcess{process: process, parent: parent, execID: "exec"}
}
