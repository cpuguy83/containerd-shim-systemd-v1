package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLoadExitStateUsesRecordedSystemdState(t *testing.T) {
	t.Run("an exec exit remains available after its systemd unit unloads", func(t *testing.T) {
		parent := &initProcess{process: &process{id: "task"}}
		process := &process{id: "exec"}
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

		if err := exec.LoadExitState(context.Background()); err != nil {
			t.Fatalf("load recorded exit state: %v", err)
		}

		state := exec.ProcessState()
		if state.Pid != 42 || state.ExitCode != 17 || !state.ExitedAt.Equal(exitedAt) || state.Status != "exited" {
			t.Fatalf("state = %s, want pid 42, exit 17, timestamp %s, status exited", state, exitedAt)
		}
	})

	t.Run("a later service exit supersedes an earlier terminal signal", func(t *testing.T) {
		parent := &initProcess{process: &process{id: "task"}}
		process := &process{id: "exec"}
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

		if err := exec.LoadExitState(context.Background()); err != nil {
			t.Fatalf("load latest recorded exit state: %v", err)
		}

		state := exec.ProcessState()
		if state.Pid != 42 || state.ExitCode != 17 || !state.ExitedAt.Equal(later) {
			t.Fatalf("state = %s, want the later pid 42, exit 17, timestamp %s", state, later)
		}
	})
}
