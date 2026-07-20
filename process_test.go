package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/go-runc"
	"github.com/opencontainers/runtime-spec/specs-go"
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

func TestRuncCommandArguments(t *testing.T) {
	t.Run("a debug log is passed as a separate option value", func(t *testing.T) {
		p := &process{
			runc: &runc.Runc{
				Command: "runc-fp",
				Debug:   true,
				Log:     "/tmp/runc.log",
				Root:    "/tmp/runc",
			},
		}

		got, err := p.runcCmd([]string{"state", "container"})
		if err != nil {
			t.Fatalf("build runc command: %v", err)
		}
		want := []string{
			"runc-fp",
			"--debug=true",
			"--systemd-cgroup=false",
			"--root", "/tmp/runc",
			"--log", "/tmp/runc.log",
			"state", "container",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("runc command = %q, want %q", got, want)
		}
	})
}

func TestExecProcessPIDFallback(t *testing.T) {
	for _, status := range []string{"running", "exited"} {
		t.Run("a "+status+" helper state supplies the workload PID after systemd removes PIDFile", func(t *testing.T) {
			parent, _ := newTestInitProcess("container")
			parent.Bundle = t.TempDir()
			exec := newTestExecProcess(parent, "exec")
			writeTestProcessState(t, exec.exitStatePath(), pState{Pid: 42, Status: status})

			pid, err := exec.getPid(context.Background())
			if err != nil {
				t.Fatalf("get exec PID: %v", err)
			}
			if pid != 42 {
				t.Fatalf("exec PID = %d, want 42", pid)
			}
		})
	}

	t.Run("an init-helper failure cannot masquerade as a workload PID", func(t *testing.T) {
		parent, _ := newTestInitProcess("container")
		parent.Bundle = t.TempDir()
		exec := newTestExecProcess(parent, "exec")
		writeTestProcessState(t, exec.exitStatePath(), pState{Pid: 42, Status: exitedInit})

		if _, err := exec.getPid(context.Background()); err == nil {
			t.Fatal("expected missing workload PID to fail")
		}
	})
}

func TestInitExitCleanup(t *testing.T) {
	t.Run("a private PID namespace relies on kernel cleanup", func(t *testing.T) {
		spec := &specs.Spec{Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{{Type: specs.PIDNamespace}},
		}}
		if shouldKillAllOnExit(spec) {
			t.Fatal("private PID namespace unexpectedly requires runtime cleanup")
		}
	})

	for name, spec := range map[string]*specs.Spec{
		"the host PID namespace requires runtime cleanup": {
			Linux: &specs.Linux{},
		},
		"a joined PID namespace requires runtime cleanup": {
			Linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{{
					Type: specs.PIDNamespace,
					Path: "/proc/1/ns/pid",
				}},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if !shouldKillAllOnExit(spec) {
				t.Fatal("shared PID namespace did not require runtime cleanup")
			}
		})
	}

	t.Run("a started init exit kills remaining processes in a shared PID namespace", func(t *testing.T) {
		runcPath := newRuncStub(t)
		runcRoot := t.TempDir()
		processRoot := filepath.Join(runcRoot, "container")
		if err := os.MkdirAll(processRoot, 0700); err != nil {
			t.Fatalf("create runc state directory: %v", err)
		}

		p, _ := newTestInitProcess("container")
		p.exe = testExecutable(t)
		p.root = t.TempDir()
		p.runc = &runc.Runc{Command: runcPath, Root: runcRoot}
		p.killAllOnExit = true
		p.markStarted()

		p.SetState(context.Background(), pState{
			Pid:      42,
			Status:   "exited",
			ExitedAt: time.Now(),
		})

		if _, err := os.Stat(filepath.Join(processRoot, runcStubKillAllMarker)); err != nil {
			t.Fatalf("find runc kill-all marker: %v", err)
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

func writeTestProcessState(t *testing.T, path string, state pState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create process state directory: %v", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal process state: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write process state: %v", err)
	}
}

func newRuncStub(t *testing.T) string {
	t.Helper()
	testBinary := testExecutable(t)
	path := filepath.Join(t.TempDir(), runcStubHelperName)
	if err := os.Symlink(testBinary, path); err != nil {
		t.Fatalf("create runc helper: %v", err)
	}
	return path
}

func testExecutable(t *testing.T) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("find test executable: %v", err)
	}
	return testBinary
}
