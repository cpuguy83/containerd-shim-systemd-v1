package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkloadState(t *testing.T) {
	t.Run("a workload that exited before it could be reported reports its exit status", func(t *testing.T) {
		pid := startTestChild(t, runcStubConfig{ExitCode: 19})
		waitForTestChildExit(t, pid)

		st, err := workloadState(pid)
		if err != nil {
			t.Fatalf("read workload state: %v", err)
		}
		if st.Pid != uint32(pid) || st.ExitCode != 19 || st.Status != "exited" {
			t.Fatalf("state = %s, want pid %d, exit 19, status exited", st, pid)
		}
		if st.ExitedAt.IsZero() {
			t.Fatal("exited state has no exit timestamp")
		}
	})

	t.Run("a workload killed by a signal reports 128 plus the signal", func(t *testing.T) {
		pid := startTestChild(t, runcStubConfig{ExitDelay: time.Hour})
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			t.Fatalf("kill child %d: %v", pid, err)
		}
		waitForTestChildExit(t, pid)

		st, err := workloadState(pid)
		if err != nil {
			t.Fatalf("read workload state: %v", err)
		}
		// containerd reports termination by signal as 128+signal, which is what
		// its clients render as 137 for SIGKILL.
		if want := uint32(exitSignalOffset + int(unix.SIGKILL)); st.ExitCode != want || st.Status != "exited" {
			t.Fatalf("state = %s, want exit %d, status exited", st, want)
		}
	})

	t.Run("a workload that is still running is left for systemd to reap", func(t *testing.T) {
		pid := startTestChild(t, runcStubConfig{ExitDelay: time.Hour})

		st, err := workloadState(pid)
		if err != nil {
			t.Fatalf("read workload state: %v", err)
		}
		if st.Pid != uint32(pid) || st.Status != "running" {
			t.Fatalf("state = %s, want pid %d, status running", st, pid)
		}

		// systemd inherits the workload when the create re-exec exits and reports
		// its exit status, so that status must still be there to collect.
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			t.Fatalf("kill child %d: %v", pid, err)
		}
		waitForTestChildExit(t, pid)
		var ws unix.WaitStatus
		reaped, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		if reaped != pid || err != nil {
			t.Fatalf("wait4 for running workload %d = %d, %v; its status was consumed here instead of being left for systemd", pid, reaped, err)
		}
	})

	t.Run("a pid this process never adopted is an error, not a zero exit", func(t *testing.T) {
		// Pid 1 is never a child of the create re-exec, so its status cannot be
		// read here. Reporting that as an exit of 0 is the failure to avoid.
		st, err := workloadState(1)
		if err == nil {
			t.Fatalf("state of pid 1 = %s, want an error", st)
		}
	})
}

func TestReadPidFile(t *testing.T) {
	ctx := context.Background()

	t.Run("the pid runc recorded is the pid reported", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pid")
		if err := os.WriteFile(path, []byte("4321\n"), 0600); err != nil {
			t.Fatalf("write pid file: %v", err)
		}

		pid, err := readPidFile(ctx, path)
		if err != nil {
			t.Fatalf("read pid file: %v", err)
		}
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
	})

	t.Run("a pid file runc never wrote is an error rather than a wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()

		if pid, err := readPidFile(ctx, filepath.Join(t.TempDir(), "pid")); err == nil {
			t.Fatalf("read of a missing pid file returned pid %d, want an error", pid)
		}
	})
}

// startTestChild forks a child that runs to the given config and returns its
// pid. os/exec is deliberately not used: these tests assert on what wait4
// reports, so the child must have no waiter of its own.
func startTestChild(t *testing.T, cfg runcStubConfig) int {
	t.Helper()
	cfg.StartImmediately = true

	dir := t.TempDir()
	if err := writeRuncStubConfig(dir, cfg); err != nil {
		t.Fatalf("write child config: %v", err)
	}
	helper := filepath.Join(dir, managedProcessHelperName)
	if err := os.Symlink(testExecutable(t), helper); err != nil {
		t.Fatalf("create child helper: %v", err)
	}

	pid, err := syscall.ForkExec(helper, []string{managedProcessHelperName, dir}, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
	})
	if err != nil {
		t.Fatalf("fork child: %v", err)
	}

	// Every case here is an assertion about which children this process has, so
	// no child may outlive the case that started it.
	t.Cleanup(func() {
		// Only signal a pid this process still owns: once the child has been
		// reaped the number belongs to whatever process gets it next.
		var ws unix.WaitStatus
		if reaped, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil); err != nil || reaped == pid {
			return
		}
		unix.Kill(pid, unix.SIGKILL)
		unix.Wait4(pid, nil, 0, nil)
	})
	return pid
}

func waitForTestChildExit(t *testing.T, pid int) {
	t.Helper()
	if err := waitForRuncStubProcessExit(pid, 30*time.Second); err != nil {
		t.Fatalf("wait for child %d to exit: %v", pid, err)
	}
}
