package main

import (
	"context"
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

		if err := p.beginStart(); err != nil {
			t.Fatalf("begin start: %v", err)
		}
		state = p.SetState(context.Background(), pState{Pid: 42, Status: "running"})
		if state.Status != "created" {
			t.Fatalf("status while Start is in flight = %q, want created", state.Status)
		}

		if _, err := p.advance(phaseRunning); err != nil {
			t.Fatalf("finish start: %v", err)
		}
		state = p.SetState(context.Background(), pState{Pid: 42, Status: "running"})
		if state.Status != "running" {
			t.Fatalf("status after Start = %q, want running", state.Status)
		}
	})

	t.Run("a recorded exit is not overwritten by a later non-terminal update", func(t *testing.T) {
		p := &process{}
		p.cond = sync.NewCond(&p.mu)
		runTestProcess(t, p)

		p.SetState(context.Background(), pState{Pid: 42, ExitCode: 9, Status: "exited", ExitedAt: time.Now()})

		state := p.SetState(context.Background(), pState{Pid: 42, Status: "created"})
		if !state.Exited() {
			t.Fatalf("state after stale created update = %+v, want it to stay exited", state)
		}
		if state.ExitCode != 9 {
			t.Fatalf("exit code after stale created update = %d, want 9", state.ExitCode)
		}
	})

	t.Run("a recorded exit can be refined by a later terminal update", func(t *testing.T) {
		p := &process{}
		p.cond = sync.NewCond(&p.mu)
		runTestProcess(t, p)

		p.SetState(context.Background(), pState{Pid: 42, Status: "exited", ExitedAt: time.Now()})

		state := p.SetState(context.Background(), pState{Pid: 42, ExitCode: 137, Status: "exited", ExitedAt: time.Now()})
		if state.ExitCode != 137 {
			t.Fatalf("exit code after terminal refinement = %d, want 137", state.ExitCode)
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
	for _, status := range []string{"running", "exited", "failed"} {
		t.Run("a "+status+" state supplies the workload PID after systemd removes PIDFile", func(t *testing.T) {
			parent, _ := newTestInitProcess("container")
			parent.Bundle = t.TempDir()
			exec := newTestExecProcess(parent, "exec")
			exec.SetState(context.Background(), pState{Pid: 42, Status: status})

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
		exec.SetState(context.Background(), pState{Pid: 42, Status: exitedInit})

		if _, err := exec.getPid(context.Background()); err == nil {
			t.Fatal("expected missing workload PID to fail")
		}
	})

	t.Run("a process that never ran reports no PID", func(t *testing.T) {
		parent, _ := newTestInitProcess("container")
		parent.Bundle = t.TempDir()
		exec := newTestExecProcess(parent, "exec")

		if _, err := exec.getPid(context.Background()); err == nil {
			t.Fatal("expected missing workload PID to fail")
		}
	})
}

// The create re-exec's argv puts the subcommand before its own flags:
// `<exe> --bundle=B create --mounts=M --tty <runc> ...`. A single flag.Parse
// stops at "create", so --mounts is never seen and is handed to exec as the
// container command instead -- which fails the unit, and only on the
// PrivateMounts path, since the other path has no --mounts to misplace.
func TestParseShimCreateArgs(t *testing.T) {
	runc := []string{"/bin/runc-stub", "--root", "/run/runc", "create", "--bundle=/b", "ctr"}

	t.Run("the private-mounts argv yields the mount config and the runc command", func(t *testing.T) {
		argv := append([]string{"--debug=false", "--bundle=/b", "create", "--mounts=/b/mounts.json"}, runc...)

		bundle, mounts, tty, cmd, err := parseShimCreateArgs(argv)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if bundle != "/b" {
			t.Fatalf("bundle = %q, want /b", bundle)
		}
		if mounts != "/b/mounts.json" {
			t.Fatalf("mounts = %q, want /b/mounts.json", mounts)
		}
		if tty {
			t.Fatal("tty set without --tty")
		}
		if !slices.Equal(cmd, runc) {
			t.Fatalf("command = %q, want %q", cmd, runc)
		}
	})

	t.Run("the shared-propagation argv carries no mount config", func(t *testing.T) {
		argv := append([]string{"--debug=false", "--bundle=/b", "create"}, runc...)

		_, mounts, _, cmd, err := parseShimCreateArgs(argv)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if mounts != "" {
			t.Fatalf("mounts = %q, want empty", mounts)
		}
		if !slices.Equal(cmd, runc) {
			t.Fatalf("command = %q, want %q", cmd, runc)
		}
	})

	t.Run("a tty argv sets tty without consuming the runc command", func(t *testing.T) {
		argv := append([]string{"--bundle=/b", "create", "--mounts=/b/mounts.json", "--tty"}, runc...)

		_, mounts, tty, cmd, err := parseShimCreateArgs(argv)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !tty || mounts != "/b/mounts.json" {
			t.Fatalf("tty = %v, mounts = %q", tty, mounts)
		}
		if !slices.Equal(cmd, runc) {
			t.Fatalf("command = %q, want %q", cmd, runc)
		}
	})

	t.Run("argv without a command is rejected", func(t *testing.T) {
		if _, _, _, _, err := parseShimCreateArgs([]string{"--bundle=/b", "create", "--mounts=/b/m.json"}); err == nil {
			t.Fatal("expected an argv with no runc command to be rejected")
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
		runTestProcess(t, p.process)

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

func TestExecStatusWhenInitExits(t *testing.T) {
	initExit := pState{Pid: 42, ExitCode: 137, ExitedAt: time.Now(), Status: "exited"}

	t.Run("an exec that was never started reports created", func(t *testing.T) {
		parent, _ := newTestInitProcess("c-created")
		ep := newTestExecProcess(parent, "exec1")

		st, err := ep.State(context.Background())
		if err != nil {
			t.Fatalf("exec state: %v", err)
		}
		if st.State.Status != "created" {
			t.Fatalf("unstarted exec status = %q, want created", st.State.Status)
		}
	})

	t.Run("an init exit leaves a never-started exec created", func(t *testing.T) {
		parent, _ := newTestInitProcess("c-unstarted")
		ep := newTestExecProcess(parent, "exec1")
		if err := parent.execs.Add("exec1", ep); err != nil {
			t.Fatalf("register exec: %v", err)
		}

		parent.SetState(context.Background(), initExit)

		st, err := ep.State(context.Background())
		if err != nil {
			t.Fatalf("exec state: %v", err)
		}
		if st.State.Status != "created" {
			t.Fatalf("never-started exec status after init exit = %q, want created", st.State.Status)
		}
	})

	t.Run("an init exit reaps a started exec", func(t *testing.T) {
		parent, _ := newTestInitProcess("c-started")
		ep := newTestExecProcess(parent, "exec1")
		runTestProcess(t, ep.process)
		ep.SetState(context.Background(), pState{Pid: 99, Status: "running"})
		if err := parent.execs.Add("exec1", ep); err != nil {
			t.Fatalf("register exec: %v", err)
		}

		parent.SetState(context.Background(), initExit)

		st, err := ep.State(context.Background())
		if err != nil {
			t.Fatalf("exec state: %v", err)
		}
		if !st.State.Exited() {
			t.Fatalf("started exec after init exit = %+v, want a recorded exit", st.State)
		}
	})

	t.Run("an exec whose init exited before its process ran reports created", func(t *testing.T) {
		parent, _ := newTestInitProcess("c-exitedinit")
		ep := newTestExecProcess(parent, "exec1")
		ep.SetState(context.Background(), pState{Pid: 55, ExitCode: 1, ExitedAt: time.Now(), Status: exitedInit})
		if err := parent.execs.Add("exec1", ep); err != nil {
			t.Fatalf("register exec: %v", err)
		}

		parent.SetState(context.Background(), initExit)

		st, err := ep.State(context.Background())
		if err != nil {
			t.Fatalf("exec state: %v", err)
		}
		if st.State.Status != "created" {
			t.Fatalf("exec status after container init died = %q, want created", st.State.Status)
		}
	})
}

// runTestProcess moves p through the lifecycle a successful Start would: it
// claims the start and then completes it.
func runTestProcess(t *testing.T, p *process) {
	t.Helper()
	if err := p.beginStart(); err != nil {
		t.Fatalf("begin start: %v", err)
	}
	if _, err := p.advance(phaseRunning); err != nil {
		t.Fatalf("finish start: %v", err)
	}
}

// newTestInitProcess builds an initProcess wired with a TaskExit counter. The
// returned func reports how many TaskExit events have been emitted so far. Its
// start has already gone out, so exits publish immediately rather than being
// held by the outbox.
func newTestInitProcess(id string) (*initProcess, func() int32) {
	var taskExits int32
	send := func(_ context.Context, _ string, evt interface{}) bool {
		if _, ok := evt.(*eventsapi.TaskExit); ok {
			atomic.AddInt32(&taskExits, 1)
		}
		return true
	}
	p := &initProcess{
		process: &process{ns: "testns", id: id, events: newEventOutbox("testns", send)},
		execs:   &processManager{ls: make(map[string]Process)},
		shimLog: io.Discard,
	}
	p.cond = sync.NewCond(&p.mu)
	p.events.Send(context.Background(), &eventsapi.TaskStart{ContainerID: id})
	return p, func() int32 { return atomic.LoadInt32(&taskExits) }
}

// newTestExecProcess builds an exec with its own outbox feeding the parent's
// event sink, as Service.Exec does, so the parent's TaskExit counter sees the
// exec's exits. Its start has already gone out.
func newTestExecProcess(parent *initProcess, execID string) *execProcess {
	ep := &execProcess{
		process: &process{ns: parent.ns, id: execID, events: newEventOutbox(parent.ns, parent.events.send)},
		parent:  parent,
		execID:  execID,
	}
	ep.cond = sync.NewCond(&ep.mu)
	ep.events.Send(context.Background(), &eventsapi.TaskExecStarted{ExecID: execID})
	return ep
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
