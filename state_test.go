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
	t.Run("copying a zero-pid non-terminal state leaves the destination untouched", func(t *testing.T) {
		dst := pState{Pid: 7, Status: "running"}
		src := pState{Status: "activating"}
		src.CopyTo(&dst)
		if dst.Pid != 7 || dst.Status != "running" {
			t.Fatalf("expected zero-pid non-terminal source to be a no-op, got %+v", dst)
		}
	})

	t.Run("copying a zero-pid terminal exit records the exit", func(t *testing.T) {
		exitedAt := time.Now()
		dst := pState{Pid: 16073, Status: "created"}
		src := pState{Status: "failed", ExitedAt: exitedAt}
		src.CopyTo(&dst)
		if !dst.Exited() {
			t.Fatalf("expected a pid-less terminal exit to record the exit, got %+v", dst)
		}
		if dst.Pid != 16073 {
			t.Fatalf("expected the destination pid to be preserved, got %d", dst.Pid)
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

func TestApplyUnitProperties(t *testing.T) {
	t.Run("a running unit reports its main pid", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID": uint32(42),
			"SubState":    "running",
			"StatusText":  "",
			"StatusErrno": int32(0),
		}, &st)

		if st.Pid != 42 || st.Status != "running" || st.Exited() {
			t.Fatalf("state = %s, want a running pid 42", st)
		}
	})

	t.Run("an exit systemd reaped is taken from the unit's own record", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(42),
			"ExecMainCode":   int32(cldExited),
			"ExecMainStatus": int32(17),
			"SubState":       "failed",
			"StatusText":     "",
		}, &st)

		if st.Pid != 42 || st.ExitCode != 17 || !st.Exited() {
			t.Fatalf("state = %s, want pid 42 and exit 17", st)
		}
	})

	// The create re-exec is the unit's main process when it reaps a workload
	// systemd never adopted, so ExecMain* describes the re-exec's own exit.
	t.Run("a reported exit outranks the unit's record of the re-exec", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(7),
			"ExecMainCode":   int32(cldExited),
			"ExecMainStatus": int32(3),
			"SubState":       "failed",
			"StatusText":     "exited 42",
			"StatusErrno":    int32(19),
		}, &st)

		if st.Pid != 42 || st.ExitCode != 19 || !st.Exited() {
			t.Fatalf("state = %s, want the reported pid 42 and exit 19", st)
		}
	})

	t.Run("a reported init failure reports no pid", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(7),
			"ExecMainCode":   int32(cldExited),
			"ExecMainStatus": int32(3),
			"SubState":       "failed",
			"StatusText":     exitedInit,
			"StatusErrno":    int32(1),
		}, &st)

		if st.Pid != 0 || st.ExitCode != 1 || st.Status != exitedInit {
			t.Fatalf("state = %s, want no pid, exit 1, status %s", st, exitedInit)
		}
	})

	t.Run("a reported exit of zero does not inherit the re-exec's exit code", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(7),
			"ExecMainCode":   int32(cldExited),
			"ExecMainStatus": int32(3),
			"SubState":       "failed",
			"StatusText":     "exited 42",
			"StatusErrno":    int32(0),
		}, &st)

		if st.Pid != 42 || st.ExitCode != 0 || !st.Exited() {
			t.Fatalf("state = %s, want the reported pid 42 and exit 0", st)
		}
	})

	t.Run("a signaled exit is reported as 128 plus the signal", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(42),
			"ExecMainCode":   int32(cldKilled),
			"ExecMainStatus": int32(9),
			"SubState":       "failed",
		}, &st)

		if want := uint32(exitSignalOffset + 9); st.ExitCode != want {
			t.Fatalf("state = %s, want exit %d", st, want)
		}
	})

	// Every unit the reactor tracks emits one of these before it starts, and
	// reading it as an exit would kill the task before it ever ran.
	t.Run("a unit that has not started yet reports nothing", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(0),
			"ExecMainCode":   int32(0),
			"ExecMainStatus": int32(0),
			"SubState":       "dead",
			"Result":         "success",
			"StatusText":     "",
			"StatusErrno":    int32(0),
		}, &st)

		if st.Exited() || st.Status != "" || st.Pid != 0 {
			t.Fatalf("state = %s, want nothing reported for a unit that never ran", st)
		}
	})

	// Identical properties to the case above apart from Result. A unit that fails
	// before it ever has a main process -- an ExecStartPre that is not allowed to
	// fail -- must not be discarded as "not started yet", or the task looks alive
	// with nothing left to report it.
	t.Run("a unit that failed before it had a main process reports a failure", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID":    uint32(0),
			"ExecMainCode":   int32(0),
			"ExecMainStatus": int32(0),
			"SubState":       "failed",
			"Result":         "exit-code",
			"StatusText":     "",
			"StatusErrno":    int32(0),
		}, &st)

		if !st.Exited() {
			t.Fatalf("state = %s, want a terminal state", st)
		}
		if st.ExitCode != 255 {
			t.Fatalf("state = %s, want exit 255", st)
		}
		if !st.ExitedAt.After(timeZero) {
			t.Fatalf("state = %s, want an exit time", st)
		}
	})

	// systemd applies the "-" ignore-failure prefix before setting Result, so a
	// pre-start command that is allowed to fail leaves the unit running.
	t.Run("a command allowed to fail does not make the unit failed", func(t *testing.T) {
		var st pState
		applyUnitProperties(map[string]interface{}{
			"ExecMainPID": uint32(42),
			"SubState":    "running",
			"Result":      "success",
		}, &st)

		if st.Exited() || st.Pid != 42 {
			t.Fatalf("state = %s, want a running pid 42", st)
		}
	})
}

func TestParseStatusText(t *testing.T) {
	t.Run("a status with a pid reports both", func(t *testing.T) {
		status, pid := parseStatusText("exited 1234")
		if status != "exited" || pid != 1234 {
			t.Fatalf("parsed %q/%d, want exited/1234", status, pid)
		}
	})

	t.Run("a status without a pid reports no pid", func(t *testing.T) {
		status, pid := parseStatusText(exitedInit)
		if status != exitedInit || pid != 0 {
			t.Fatalf("parsed %q/%d, want %s/0", status, pid, exitedInit)
		}
	})

	t.Run("an unreadable pid still reports the exit", func(t *testing.T) {
		status, pid := parseStatusText("exited not-a-pid")
		if status != "exited" || pid != 0 {
			t.Fatalf("parsed %q/%d, want exited/0", status, pid)
		}
	})

	t.Run("an empty status text reports nothing", func(t *testing.T) {
		status, pid := parseStatusText("")
		if status != "" || pid != 0 {
			t.Fatalf("parsed %q/%d, want empty", status, pid)
		}
	})
}
