package main

import (
	"testing"
	"time"

	"github.com/containerd/containerd/api/types/task"
)

func TestPStateExited(t *testing.T) {
	t.Run("a non-zero exit code means exited", func(t *testing.T) {
		if !(pState{ExitCode: 1}).Exited() {
			t.Fatal("expected a non-zero exit code to be exited")
		}
	})

	t.Run("a dead substate means exited", func(t *testing.T) {
		if !(pState{Status: "dead"}).Exited() {
			t.Fatal("expected a dead substate to be exited")
		}
	})

	t.Run("a failed substate means exited", func(t *testing.T) {
		if !(pState{Status: "failed"}).Exited() {
			t.Fatal("expected a failed substate to be exited")
		}
	})

	t.Run("an exit timestamp after the epoch means exited", func(t *testing.T) {
		if !(pState{ExitedAt: time.UnixMicro(1)}).Exited() {
			t.Fatal("expected an exit timestamp to be exited")
		}
	})

	t.Run("a running process is not exited", func(t *testing.T) {
		if (pState{Pid: 42, Status: "running", ExitedAt: timeZero}).Exited() {
			t.Fatal("expected a running process not to be exited")
		}
	})
}

func TestPStateStarted(t *testing.T) {
	t.Run("a process with a pid is started", func(t *testing.T) {
		if !(pState{Pid: 1}).Started() {
			t.Fatal("expected a process with a pid to be started")
		}
	})

	t.Run("a process without a pid is not started", func(t *testing.T) {
		if (pState{}).Started() {
			t.Fatal("expected a process without a pid not to be started")
		}
	})
}

func TestToStatus(t *testing.T) {
	running := map[string]bool{"running": true, "start-post": true}
	stopped := map[string]bool{
		"stopped": true, "dead": true, "failed": true, "stop-post": true,
		"exited": true, exitedInit: true, "exit-code": true,
	}

	for sub := range running {
		sub := sub
		t.Run("the substate "+sub+" maps to running", func(t *testing.T) {
			if got := toStatus(sub); got != task.Status_RUNNING {
				t.Fatalf("expected %q to map to running, got %v", sub, got)
			}
		})
	}

	for sub := range stopped {
		sub := sub
		t.Run("the substate "+sub+" maps to stopped", func(t *testing.T) {
			if got := toStatus(sub); got != task.Status_STOPPED {
				t.Fatalf("expected %q to map to stopped, got %v", sub, got)
			}
		})
	}

	t.Run("an unknown substate maps to unknown", func(t *testing.T) {
		if got := toStatus("no-such-substate"); got != task.Status_UNKNOWN {
			t.Fatalf("expected an unknown substate to map to unknown, got %v", got)
		}
	})
}

func TestPStateCopyTo(t *testing.T) {
	t.Run("copying a zero-pid state leaves the destination untouched", func(t *testing.T) {
		dst := pState{Pid: 7, Status: "running"}
		src := pState{Status: "dead"}
		src.CopyTo(&dst)
		if dst.Pid != 7 || dst.Status != "running" {
			t.Fatalf("expected zero-pid source to be a no-op, got %+v", dst)
		}
	})

	t.Run("copying fills an empty destination pid", func(t *testing.T) {
		dst := pState{}
		src := pState{Pid: 99, Status: "running"}
		src.CopyTo(&dst)
		if dst.Pid != 99 {
			t.Fatalf("expected pid to be copied into an empty destination, got %d", dst.Pid)
		}
	})

	t.Run("copying does not overwrite an existing exit code", func(t *testing.T) {
		dst := pState{Pid: 1, ExitCode: 2}
		src := pState{Pid: 1, ExitCode: 9}
		src.CopyTo(&dst)
		if dst.ExitCode != 2 {
			t.Fatalf("expected existing exit code to be preserved, got %d", dst.ExitCode)
		}
	})

	t.Run("copying propagates a terminal status", func(t *testing.T) {
		dst := pState{Pid: 1, Status: "running"}
		src := pState{Pid: 1, Status: "dead"}
		src.CopyTo(&dst)
		if dst.Status != "dead" {
			t.Fatalf("expected status to be propagated, got %q", dst.Status)
		}
	})

	t.Run("copying a stale running state preserves a terminal destination", func(t *testing.T) {
		exitedAt := time.Now()
		dst := pState{Pid: 1, Status: "exited", ExitCode: 9, ExitedAt: exitedAt}
		src := pState{Pid: 1, Status: "running"}
		src.CopyTo(&dst)

		if dst.Status != "exited" {
			t.Fatalf("terminal status = %q, want exited", dst.Status)
		}
		if dst.ExitCode != 9 {
			t.Fatalf("exit code = %d, want 9", dst.ExitCode)
		}
		if !dst.ExitedAt.Equal(exitedAt) {
			t.Fatalf("exit time = %s, want %s", dst.ExitedAt, exitedAt)
		}
	})
}

func TestReadExecStatusExit(t *testing.T) {
	t.Run("the exit code is read from field nine and the timestamp from field five", func(t *testing.T) {
		exitedAtMicros := uint64(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro())
		row := []interface{}{
			"path", []string{"arg"}, false,
			uint64(0), uint64(0), exitedAtMicros, uint64(0),
			uint32(1234), int32(0), int32(37),
		}

		code, exitedAt := readExecStatusExit(row)
		if code != 37 {
			t.Fatalf("expected exit code 37, got %d", code)
		}
		if got := uint64(exitedAt.UnixMicro()); got != exitedAtMicros {
			t.Fatalf("expected exit timestamp %d, got %d", exitedAtMicros, got)
		}
	})
}
