package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	v2runcopts "github.com/containerd/containerd/api/types/runc/options"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	"github.com/godbus/dbus/v5"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestServiceTaskLifecycleAgainstSystemd(t *testing.T) {
	t.Run("a started task reports its systemd exit through wait, event, and delete", func(t *testing.T) {
		h := newServiceIntegrationHarness(t)
		ctx, req, unit := h.task(t, "exit-seven", runcStubConfig{ExitCode: 7, ExitDelay: 100 * time.Millisecond})

		created, err := h.service.Create(ctx, req)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		if created.Pid == 0 {
			t.Fatal("create returned a zero pid")
		}
		createdState, err := h.service.State(ctx, &taskapi.StateRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("read created task state: %v", err)
		}
		if createdState.Status != tasktypes.Status_CREATED {
			t.Fatalf("created task status = %s, want %s", createdState.Status, tasktypes.Status_CREATED)
		}

		started, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("start task: %v", err)
		}
		if started.Pid != created.Pid {
			t.Fatalf("start pid = %d, want create pid %d", started.Pid, created.Pid)
		}

		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("wait for task: %v", err)
		}
		if waited.ExitStatus != 7 {
			t.Fatalf("wait exit status = %d, want 7", waited.ExitStatus)
		}
		if !waited.ExitedAt.AsTime().After(timeZero) {
			t.Fatalf("wait exit time = %s, want a real exit time", waited.ExitedAt)
		}

		exited := waitForProcessExit(t, h.service.events, req.ID, req.ID)
		if exited.ExitStatus != 7 {
			t.Fatalf("event exit status = %d, want 7", exited.ExitStatus)
		}
		if exited.Pid != created.Pid {
			t.Fatalf("event pid = %d, want %d", exited.Pid, created.Pid)
		}

		deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("delete task: %v", err)
		}
		if deleted.ExitStatus != 7 {
			t.Fatalf("delete exit status = %d, want 7", deleted.ExitStatus)
		}
		if _, err := os.Stat(filepath.Join(h.unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit file still exists after delete: %v", err)
		}
		assertNoProcessExit(t, h.service.events, req.ID, req.ID, 300*time.Millisecond)
	})

	t.Run("a runc create failure is returned and its task is cleaned up", func(t *testing.T) {
		h := newServiceIntegrationHarness(t)
		ctx, req, unit := h.task(t, "create-failure", runcStubConfig{Failpoint: "create"})

		_, err := h.service.Create(ctx, req)
		if err == nil {
			t.Fatal("create succeeded despite the runc create failpoint")
		}
		if !strings.Contains(err.Error(), "error starting systemd unit: failed") {
			t.Fatalf("create error = %q, want failed systemd start job", err)
		}
		if got := h.service.processes.Get(path.Join(h.namespace, req.ID)); got != nil {
			t.Fatalf("failed task remains registered: %#v", got)
		}
		if _, err := os.Stat(filepath.Join(h.unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit file remains after failed create: %v", err)
		}
	})

	t.Run("a runc start failure stops the task and permits deletion", func(t *testing.T) {
		h := newServiceIntegrationHarness(t)
		ctx, req, unit := h.task(t, "start-failure", runcStubConfig{Failpoint: "start"})

		if _, err := h.service.Create(ctx, req); err != nil {
			t.Fatalf("create task: %v", err)
		}
		_, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID})
		if err == nil {
			t.Fatal("start succeeded despite the runc start failpoint")
		}
		if !strings.Contains(err.Error(), strconv.Itoa(runcStubStartFailure)) {
			t.Fatalf("start error %q does not include runc status %d", err, runcStubStartFailure)
		}

		state, err := h.service.State(ctx, &taskapi.StateRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("read failed task state: %v", err)
		}
		if state.Status != tasktypes.Status_STOPPED {
			t.Fatalf("task status = %s, want stopped", state.Status)
		}
		if state.ExitStatus != 255 {
			t.Fatalf("task exit status = %d, want 255", state.ExitStatus)
		}

		deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID})
		if err != nil {
			t.Fatalf("delete failed task: %v", err)
		}
		if deleted.ExitStatus != 255 {
			t.Fatalf("delete exit status = %d, want 255", deleted.ExitStatus)
		}
		if _, err := os.Stat(filepath.Join(h.unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit file still exists after delete: %v", err)
		}
	})
}

func TestServiceRuntimeOptionsAgainstSystemd(t *testing.T) {
	tests := []struct {
		name           string
		optionsTypeURL string
	}{
		{name: "modern runc options select the runtime used by init and exec processes"},
		{name: "containerd 1.7 runc options select the runtime used by init and exec processes", optionsTypeURL: legacyRuncOptionsTypeURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceIntegrationHarness(t)
			ctx, req, taskUnit := h.task(t, "selected-runc", runcStubConfig{ExitDelay: 30 * time.Second})

			selectedRunc := h.service.runcBin
			h.service.runcBin = filepath.Join(filepath.Dir(h.service.runcBin), "missing-default-runc")

			createOptions, err := typeurl.MarshalAnyToProto(&v2runcopts.Options{BinaryName: selectedRunc})
			if err != nil {
				t.Fatalf("marshal runc options: %v", err)
			}
			if tc.optionsTypeURL != "" {
				createOptions.TypeUrl = tc.optionsTypeURL
			}
			req.Options = createOptions

			if _, err := h.service.Create(ctx, req); err != nil {
				t.Fatalf("create task: %v", err)
			}
			assertUnitWorkingDirectory(t, h.conn, taskUnit, req.Bundle)
			if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
				t.Fatalf("start task: %v", err)
			}

			const execID = "selected-runc-exec"
			execUnit := h.exec(t, ctx, req, execID, runcStubConfig{ExitDelay: 100 * time.Millisecond})
			if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID, ExecID: execID}); err != nil {
				t.Fatalf("start exec: %v", err)
			}
			assertUnitWorkingDirectory(t, h.conn, execUnit, req.Bundle)

			execWaitCtx, cancelExecWait := context.WithTimeout(ctx, 10*time.Second)
			defer cancelExecWait()
			if _, err := h.service.Wait(execWaitCtx, &taskapi.WaitRequest{ID: req.ID, ExecID: execID}); err != nil {
				t.Fatalf("wait for exec: %v", err)
			}
			if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID, ExecID: execID}); err != nil {
				t.Fatalf("delete exec: %v", err)
			}

			if _, err := h.service.Kill(ctx, &taskapi.KillRequest{ID: req.ID, Signal: uint32(unix.SIGKILL), All: true}); err != nil {
				t.Fatalf("kill task: %v", err)
			}
			taskWaitCtx, cancelTaskWait := context.WithTimeout(ctx, 10*time.Second)
			defer cancelTaskWait()
			if _, err := h.service.Wait(taskWaitCtx, &taskapi.WaitRequest{ID: req.ID}); err != nil {
				t.Fatalf("wait for task: %v", err)
			}
			if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
				t.Fatalf("delete task: %v", err)
			}
		})
	}

	t.Run("a missing selected runc binary fails create", func(t *testing.T) {
		h := newServiceIntegrationHarness(t)
		ctx, req, _ := h.task(t, "missing-runc", runcStubConfig{})
		missingRunc := filepath.Join(t.TempDir(), "missing-runc")

		createOptions, err := typeurl.MarshalAnyToProto(&v2runcopts.Options{BinaryName: missingRunc})
		if err != nil {
			t.Fatalf("marshal runc options: %v", err)
		}
		req.Options = createOptions

		_, err = h.service.Create(ctx, req)
		if err == nil {
			t.Fatal("create succeeded with a missing runc binary")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("failed to look up runc binary %q", missingRunc)) {
			t.Fatalf("create error = %q, want missing runc binary", err)
		}
	})
}

func assertUnitWorkingDirectory(t *testing.T, conn *systemd.Conn, unit, workingDirectory string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prop, err := conn.GetUnitTypePropertyContext(ctx, unit, "Service", "WorkingDirectory")
	if err != nil {
		t.Fatalf("get WorkingDirectory for %s: %v", unit, err)
	}
	got, ok := prop.Value.Value().(string)
	if !ok {
		t.Fatalf("WorkingDirectory for %s is %T, want string", unit, prop.Value.Value())
	}
	if got != workingDirectory {
		t.Fatalf("unit %s WorkingDirectory = %q, want %q", unit, got, workingDirectory)
	}
}

func TestServiceExecLifecycleAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, _ := h.task(t, "exec-parent", runcStubConfig{ExitDelay: 30 * time.Second})
	if _, err := h.service.Create(ctx, req); err != nil {
		t.Fatalf("create parent task: %v", err)
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start parent task: %v", err)
	}

	tests := []struct {
		name              string
		execID            string
		cfg               runcStubConfig
		exitCode          uint32
		releaseAfterStart bool
	}{
		{
			name:     "an exec that exits zero before runc returns is started, waitable, and deletable",
			execID:   "fast-zero",
			cfg:      runcStubConfig{ExitBeforeDetach: true},
			exitCode: 0,
		},
		{
			name:     "an exec that exits non-zero before runc returns is started, waitable, and deletable",
			execID:   "fast-seventeen",
			cfg:      runcStubConfig{ExitCode: 17, ExitBeforeDetach: true},
			exitCode: 17,
		},
		{
			name:              "an exec that exits as soon as start returns is waitable and deletable",
			execID:            "exit-after-start",
			cfg:               runcStubConfig{WaitForRelease: true},
			exitCode:          0,
			releaseAfterStart: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			execUnit := h.exec(t, ctx, req, tc.execID, tc.cfg)

			started, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID, ExecID: tc.execID})
			if err != nil {
				t.Fatalf("start exec: %v", err)
			}
			if started.Pid == 0 {
				t.Fatal("start exec returned a zero pid")
			}
			if tc.releaseAfterStart {
				h.releaseExec(t, req, tc.execID)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID, ExecID: tc.execID})
			if err != nil {
				t.Fatalf("wait for exec: %v", err)
			}
			if waited.ExitStatus != tc.exitCode {
				t.Fatalf("wait exit status = %d, want %d", waited.ExitStatus, tc.exitCode)
			}
			if !waited.ExitedAt.AsTime().After(timeZero) {
				t.Fatalf("wait exit time = %s, want a real exit time", waited.ExitedAt)
			}

			startedEvent, exited := waitForExecStartedThenExit(t, h.service.events, req.ID, tc.execID)
			if startedEvent.Pid != started.Pid {
				t.Fatalf("started event pid = %d, want %d", startedEvent.Pid, started.Pid)
			}
			if exited.ExitStatus != tc.exitCode {
				t.Fatalf("event exit status = %d, want %d", exited.ExitStatus, tc.exitCode)
			}
			if exited.Pid != started.Pid {
				t.Fatalf("event pid = %d, want %d", exited.Pid, started.Pid)
			}

			deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID, ExecID: tc.execID})
			if err != nil {
				t.Fatalf("delete exec: %v", err)
			}
			if deleted.ExitStatus != tc.exitCode {
				t.Fatalf("delete exit status = %d, want %d", deleted.ExitStatus, tc.exitCode)
			}
			if _, err := os.Stat(filepath.Join(h.unitDir, execUnit)); !os.IsNotExist(err) {
				t.Fatalf("exec unit file still exists after delete: %v", err)
			}
			if _, err := os.Stat(filepath.Join(req.Bundle, "execs", tc.execID)); !os.IsNotExist(err) {
				t.Fatalf("exec state directory still exists after delete: %v", err)
			}
			assertNoProcessExit(t, h.service.events, req.ID, tc.execID, 300*time.Millisecond)
		})
	}

	t.Run("a runc exec failure fails start without publishing process lifecycle events", func(t *testing.T) {
		const execID = "exec-failure"
		execUnit := h.exec(t, ctx, req, execID, runcStubConfig{Failpoint: "exec"})

		_, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID, ExecID: execID})
		if err == nil {
			t.Fatal("start exec succeeded despite the runc exec failpoint")
		}
		if !strings.Contains(err.Error(), "error starting exec process") {
			t.Fatalf("start error = %q, want an exec start failure", err)
		}
		assertNoExecLifecycleEvent(t, h.service.events, req.ID, execID, 300*time.Millisecond)

		deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID, ExecID: execID})
		if err != nil {
			t.Fatalf("delete failed exec: %v", err)
		}
		if deleted.ExitStatus != runcStubExecFailure {
			t.Fatalf("delete exit status = %d, want %d", deleted.ExitStatus, runcStubExecFailure)
		}
		if _, err := os.Stat(filepath.Join(h.unitDir, execUnit)); !os.IsNotExist(err) {
			t.Fatalf("exec unit file still exists after delete: %v", err)
		}
	})

	if _, err := h.service.Kill(ctx, &taskapi.KillRequest{ID: req.ID, Signal: uint32(unix.SIGKILL)}); err != nil {
		t.Fatalf("kill parent task: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID}); err != nil {
		t.Fatalf("wait for parent task: %v", err)
	}
	if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
		t.Fatalf("delete parent task: %v", err)
	}
}

// Transient units are registered under the task's unit name, so a task id can
// only be recreated once the shim's Delete has released the previous unit. These
// tests reuse a single id across successive runs -- normal, non-zero, and fast
// (immediate) exits -- driving each run's Delete before the next Create. h.task
// registers its unit cleanup against the enclosing test, not each subtest, so it
// is the shim's own Delete that must free the name between runs.
func TestServiceTaskIDReuseAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	const id = "reused-task"

	runs := []struct {
		name     string
		cfg      runcStubConfig
		exitCode uint32
	}{
		{"a successful zero-exit run reuses the task id", runcStubConfig{ExitDelay: 100 * time.Millisecond}, 0},
		{"a non-zero exit run reuses the task id", runcStubConfig{ExitCode: 9, ExitDelay: 100 * time.Millisecond}, 9},
		{"a fast zero-exit run reuses the task id", runcStubConfig{}, 0},
		{"a fast non-zero exit run reuses the task id", runcStubConfig{ExitCode: 11}, 11},
	}
	for _, tc := range runs {
		ctx, req, unit := h.task(t, id, tc.cfg)
		t.Run(tc.name, func(t *testing.T) {
			assertTaskRunAndExit(t, h, ctx, req, unit, tc.exitCode)
		})
	}
}

// TestServiceExecIDReuseAgainstSystemd is the exec analogue: a single exec id is
// run repeatedly against one long-lived parent, so the shim's exec Delete must
// release the exec's transient unit before it can be recreated.
func TestServiceExecIDReuseAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, _ := h.task(t, "exec-reuse-parent", runcStubConfig{ExitDelay: 30 * time.Second})
	if _, err := h.service.Create(ctx, req); err != nil {
		t.Fatalf("create parent task: %v", err)
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start parent task: %v", err)
	}
	// Tear the long-lived parent down on every exit path -- including a failed
	// subtest that aborts before the loop finishes -- so it never lingers.
	t.Cleanup(func() {
		_, _ = h.service.Kill(ctx, &taskapi.KillRequest{ID: req.ID, Signal: uint32(unix.SIGKILL), All: true})
		_, _ = h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID})
	})

	const execID = "reused-exec"
	runs := []struct {
		name     string
		cfg      runcStubConfig
		exitCode uint32
	}{
		{"a successful zero-exit run reuses the exec id", runcStubConfig{ExitDelay: 100 * time.Millisecond}, 0},
		{"a non-zero exit run reuses the exec id", runcStubConfig{ExitCode: 17, ExitDelay: 100 * time.Millisecond}, 17},
		{"a fast zero-exit run reuses the exec id", runcStubConfig{ExitBeforeDetach: true}, 0},
		{"a fast non-zero exit run reuses the exec id", runcStubConfig{ExitCode: 19, ExitBeforeDetach: true}, 19},
	}
	for _, tc := range runs {
		execUnit := h.exec(t, ctx, req, execID, tc.cfg)
		t.Run(tc.name, func(t *testing.T) {
			assertExecRunAndExit(t, h, ctx, req, execID, execUnit, tc.exitCode)
		})
	}
}

// A checkpoint restore is not a from-scratch create: Create records the restore
// command and defers the runc invocation to Start, which runs
// `runc restore --image-path=<checkpoint>`. This drives that through the public
// task API and proves it is a restore -- Create returns no pid (a create would
// return the runc pid), and the runc stub fails unless it is handed the
// checkpoint image at --image-path.
func TestServiceTaskRestoreAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, _ := h.task(t, "restore-target", runcStubConfig{ExitDelay: 100 * time.Millisecond})

	checkpoint := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkpoint, runcStubCheckpointImage), nil, 0600); err != nil {
		t.Fatalf("write checkpoint image: %v", err)
	}
	req.Checkpoint = checkpoint

	created, err := h.service.Create(ctx, req)
	if err != nil {
		t.Fatalf("create restore task: %v", err)
	}
	// Restore defers its runc invocation to Start, so Create returns no pid; a
	// from-scratch create would return the runc pid here.
	if created.Pid != 0 {
		t.Fatalf("restore create returned pid %d, want 0 (restore defers to start)", created.Pid)
	}

	started, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("start restore task: %v", err)
	}
	if started.Pid == 0 {
		t.Fatal("restore start returned a zero pid")
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("wait for restored task: %v", err)
	}
	if waited.ExitStatus != 0 {
		t.Fatalf("wait exit status = %d, want 0", waited.ExitStatus)
	}

	if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
		t.Fatalf("delete restored task: %v", err)
	}
}

// TestServiceTaskRootfsMountAgainstSystemd exercises the no-new-namespace rootfs
// path end to end: a non-empty Rootfs with "shared" propagation makes the shim
// mount the rootfs via an ExecStartPre re-exec (in the host namespace) and
// unmount it via ExecStopPost. Both are hand-built transient exec tuples. The
// mount is checked from the host and from inside the container (a marker file),
// and the unmount is checked after delete. (The PrivateMounts variant's property
// construction is covered by TestInitProcessRootfsMountProperties; its runtime
// needs cross-namespace runc state the in-process stub cannot model.)
// TestServiceUnitNotifyPropertiesAgainstSystemd pins the notify configuration
// the shim's exit reporting depends on, for both unit kinds.
//
// Type=notify alone is not enough. It implies NotifyAccess=main, and once
// systemd accepts MAINPID= the workload is the main process -- so the create
// re-exec's report of a workload that died during the handoff is refused, and
// systemd has no record of that exit either because the re-exec reaped it. The
// exit status is lost outright. Dropping NotifyAccess=all would not fail any
// other test in this suite; it would just start losing exit codes under load.
func TestServiceUnitNotifyPropertiesAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, initUnit := h.task(t, "notify-props", runcStubConfig{ExitDelay: 30 * time.Second})
	if _, err := h.service.Create(ctx, req); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start task: %v", err)
	}
	execUnit := h.exec(t, ctx, req, "notify-props-exec", runcStubConfig{ExitDelay: 30 * time.Second})
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID, ExecID: "notify-props-exec"}); err != nil {
		t.Fatalf("start exec: %v", err)
	}

	for name, unit := range map[string]string{"a container unit": initUnit, "an exec unit": execUnit} {
		t.Run(name+" accepts the create re-exec's reports after the main pid moves", func(t *testing.T) {
			props, err := h.conn.GetAllPropertiesContext(ctx, unit)
			if err != nil {
				t.Fatalf("read unit properties: %v", err)
			}
			if got := props["Type"]; got != "notify" {
				t.Fatalf("unit Type = %v, want notify", got)
			}
			if got := props["NotifyAccess"]; got != "all" {
				t.Fatalf("unit NotifyAccess = %v, want all", got)
			}
		})
	}

	if _, err := h.service.Kill(ctx, &taskapi.KillRequest{ID: req.ID, Signal: uint32(unix.SIGKILL), All: true}); err != nil {
		t.Fatalf("kill task: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID}); err != nil {
		t.Fatalf("wait for task: %v", err)
	}
	if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID, ExecID: "notify-props-exec"}); err != nil {
		t.Fatalf("delete exec: %v", err)
	}
	if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
		t.Fatalf("delete task: %v", err)
	}
}

// TestServiceWaitRecoversMissedExitAgainstSystemd is the counterpart to
// TestServiceTaskMissedExitSignalAgainstSystemd for the Wait path. Wait skips
// its property read while the signal stream is up, on the grounds that nothing
// can have been missed; with the stream down that reasoning does not hold, so it
// must still look. waitForExit parks in sync.Cond.Wait, which no context can
// interrupt, so a regression here hangs rather than failing.
func TestServiceWaitRecoversMissedExitAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, unit := h.task(t, "missed-exit-wait", runcStubConfig{ExitCode: 7, ExitDelay: 300 * time.Millisecond})

	if _, err := h.service.Create(ctx, req); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// Drop the stream before the workload exits, so nothing reconciles it.
	h.stopReactor()

	if !eventually(15*time.Second, 20*time.Millisecond, func() bool {
		st, err := loadExitFromUnit(ctx, h.conn, unit)
		return err == nil && st.Exited()
	}) {
		t.Fatal("unit never exited in systemd")
	}
	if h.service.processes.Get(path.Join(h.namespace, req.ID)).ProcessState().Exited() {
		t.Skip("shim recorded the exit without the event stream; cannot exercise the missed-signal path")
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type waitResult struct {
		resp *taskapi.WaitResponse
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		resp, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID})
		done <- waitResult{resp, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("wait for task: %v", res.err)
		}
		if res.resp.ExitStatus != 7 {
			t.Errorf("wait reported exit status %d, want 7 recovered from the still-loaded unit", res.resp.ExitStatus)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wait blocked: the exit on the still-loaded unit was never read with the stream down")
	}

	if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
		t.Fatalf("delete task: %v", err)
	}
}

// TestServiceTaskRootfsMountFailureAgainstSystemd covers the unit failing before
// it ever has a main process. ExecStartPre mounts the rootfs on the
// no-new-namespace path, and a mount that cannot succeed fails the unit while
// ExecMainPID is still 0 -- properties identical to a unit that has not started
// yet, apart from Result. Reading that as "not started" makes Create report
// success with pid 0 for a container whose rootfs was never mounted.
//
// Unlike the success path this needs no privileges: the mount has to fail, and
// it fails unprivileged just as well.
func TestServiceTaskRootfsMountFailureAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)

	ctx, req, _ := h.task(t, "rootfs-mount-failure", runcStubConfig{}, func(s *specs.Spec) {
		s.Linux.RootfsPropagation = "shared" // selects the ExecStartPre mount path
	})
	req.Rootfs = []*types.Mount{{
		Type:    "bind",
		Source:  filepath.Join(t.TempDir(), "does-not-exist"),
		Options: []string{"rbind"},
	}}

	created, err := h.service.Create(ctx, req)
	if err == nil {
		t.Fatalf("create succeeded with an unmountable rootfs, pid %d", created.Pid)
	}

	state, err := h.service.State(ctx, &taskapi.StateRequest{ID: req.ID})
	if err != nil {
		// Create may have torn the task down entirely, which is also a correct
		// answer -- what must not happen is a live task with no exit.
		return
	}
	if state.Status != tasktypes.Status_STOPPED {
		t.Fatalf("task status = %s, want stopped after a failed rootfs mount", state.Status)
	}
	if state.ExitStatus == 0 {
		t.Fatal("task reported exit status 0 after a failed rootfs mount")
	}
}

func TestServiceTaskRootfsMountAgainstSystemd(t *testing.T) {
	requireBindMount(t)

	// The two rootfs paths differ in who mounts and where. A shared propagation
	// keeps the container in the host mount namespace, so ExecStartPre mounts
	// the rootfs where the host can see it. Otherwise the unit gets
	// PrivateMounts and the create re-exec mounts inside it, invisible to the
	// host -- and that re-exec cannot reap the workload, so it reports readiness
	// and nothing else.
	for _, tc := range []struct {
		name        string
		specOpt     func(*specs.Spec)
		hostMounted bool
	}{
		{
			name:        "a shared rootfs propagation mounts the rootfs where the host can see it",
			specOpt:     func(s *specs.Spec) { s.Linux.RootfsPropagation = "shared" },
			hostMounted: true,
		},
		{
			name:        "a private mount namespace mounts the rootfs inside the unit",
			specOpt:     func(s *specs.Spec) {},
			hostMounted: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceIntegrationHarness(t)

			const marker = "mounted"
			src := t.TempDir()
			if err := os.WriteFile(filepath.Join(src, marker), nil, 0600); err != nil {
				t.Fatalf("write rootfs marker: %v", err)
			}

			ctx, req, _ := h.task(t, "rootfs-mount", runcStubConfig{ExitDelay: 100 * time.Millisecond}, func(s *specs.Spec) {
				s.Annotations[runcStubRootfsMarkerAnnotation] = marker
				tc.specOpt(s)
			})
			req.Rootfs = []*types.Mount{{Type: "bind", Source: src, Options: []string{"rbind"}}}
			rootfs := filepath.Join(req.Bundle, "rootfs")

			if _, err := h.service.Create(ctx, req); err != nil {
				t.Fatalf("create task: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rootfs, marker)); tc.hostMounted != (err == nil) {
				t.Fatalf("rootfs visible in host namespace = %v, want %v (%v)", err == nil, tc.hostMounted, err)
			}

			if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
				t.Fatalf("start task: %v", err)
			}

			// A clean zero exit proves the container saw its mounted rootfs; a
			// missing mount would have exited runcStubRootfsMissing instead.
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID})
			if err != nil {
				t.Fatalf("wait for task: %v", err)
			}
			if waited.ExitStatus != 0 {
				t.Fatalf("task exit status = %d, want 0 (container saw the rootfs marker)", waited.ExitStatus)
			}

			if _, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID}); err != nil {
				t.Fatalf("delete task: %v", err)
			}
			// ExecStopPost unmounted the rootfs, so the host no longer sees it.
			if _, err := os.Stat(filepath.Join(rootfs, marker)); !os.IsNotExist(err) {
				t.Fatalf("rootfs still mounted after delete: %v", err)
			}
		})
	}
}

// TestServiceTaskMissedExitSignalAgainstSystemd asserts a task whose exit signal
// the shim never observed still reports its real exit status through delete.
// This is the case the unit reference exists for: with the exited unit kept
// loaded, delete recovers the terminal state from systemd instead of blocking on
// an in-memory state that never saw the exit.
func TestServiceTaskMissedExitSignalAgainstSystemd(t *testing.T) {
	h := newServiceIntegrationHarness(t)
	ctx, req, unit := h.task(t, "missed-exit", runcStubConfig{ExitCode: 7, ExitDelay: 300 * time.Millisecond})

	if _, err := h.service.Create(ctx, req); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// Drop the event stream before the workload exits, so nothing reconciles the
	// exit into process state.
	h.stopReactor()

	if !eventually(15*time.Second, 20*time.Millisecond, func() bool {
		st, err := loadExitFromUnit(ctx, h.conn, unit)
		return err == nil && st.Exited()
	}) {
		t.Fatal("unit never exited in systemd")
	}
	state, err := h.service.State(ctx, &taskapi.StateRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Status == tasktypes.Status_STOPPED {
		t.Skip("shim recorded the exit without the event stream; cannot exercise the missed-signal path")
	}

	// Delete must not be left waiting on an exit that already happened. Bound it
	// here rather than relying on the request context: waitForExit parks in
	// sync.Cond.Wait, which no context can interrupt, so a regression hangs
	// forever instead of returning a deadline error.
	delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type deleteResult struct {
		resp *taskapi.DeleteResponse
		err  error
	}
	done := make(chan deleteResult, 1)
	go func() {
		resp, err := h.service.Delete(delCtx, &taskapi.DeleteRequest{ID: req.ID})
		done <- deleteResult{resp, err}
	}()

	var deleted *taskapi.DeleteResponse
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("delete task: %v", res.err)
		}
		deleted = res.resp
	case <-time.After(30 * time.Second):
		t.Fatal("delete blocked: the exit recorded on the still-loaded unit was never applied to the task")
	}

	if deleted.ExitStatus != 7 {
		t.Errorf("delete reported exit status %d, want 7 recovered from the still-loaded unit", deleted.ExitStatus)
	}
	if !deleted.ExitedAt.AsTime().After(timeZero) {
		t.Errorf("delete reported exit time %s, want the recovered exit stamped", deleted.ExitedAt.AsTime())
	}
}

// requireBindMount skips the test when this environment cannot bind mount, e.g.
// a user systemd manager whose units run without CAP_SYS_ADMIN. The shim's
// rootfs mount runs in a systemd unit with the same privileges as this process,
// so probing here is representative.
func requireBindMount(t *testing.T) {
	t.Helper()
	src, dst := t.TempDir(), t.TempDir()
	if err := mount.All([]mount.Mount{{Type: "bind", Source: src, Options: []string{"rbind"}}}, dst); err != nil {
		t.Skipf("cannot bind mount (need CAP_SYS_ADMIN): %v", err)
	}
	if err := mount.UnmountAll(dst, 0); err != nil {
		t.Logf("unmount probe mount %s: %v", dst, err)
	}
}

type serviceIntegrationHarness struct {
	service   *Service
	conn      *systemd.Conn
	unitDir   string
	namespace string
	// stopReactor drops the D-Bus event stream, so unit state changes stop being
	// reconciled into process state. Tests use it to simulate a missed exit
	// signal. It is idempotent and also runs at cleanup.
	stopReactor func()
}

func newServiceIntegrationHarness(t *testing.T) *serviceIntegrationHarness {
	t.Helper()
	busPath, unitDir := integrationSystemdManager(t)
	ctx := context.Background()
	conn := dialPrivate(t, ctx, busPath)
	pinConn, pinRef, err := newPinConnection(ctx)
	if err != nil {
		t.Logf("no D-Bus message bus for unit references; pinning disabled: %v", err)
		pinConn, pinRef = nil, nil
	}

	helperDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("find test executable: %v", err)
	}
	for _, name := range []string{shimCreateHelperName, runcStubHelperName, managedProcessHelperName} {
		if err := os.Symlink(testBinary, filepath.Join(helperDir, name)); err != nil {
			t.Fatalf("create %s helper: %v", name, err)
		}
	}

	service, err := newServiceWithConfig(ctx, serviceConfig{
		Config: Config{
			Root:    t.TempDir(),
			LogMode: options.LogMode_NULL,
		},
		conn:    conn,
		pinConn: pinConn,
		pinRef:  pinRef,
		runcBin: filepath.Join(helperDir, runcStubHelperName),
		exe:     filepath.Join(helperDir, shimCreateHelperName),
	})
	if err != nil {
		conn.Close()
		if pinConn != nil {
			pinConn.Close()
		}
		t.Fatalf("create service: %v", err)
	}

	signalConn := dialSignalConn(t, ctx, busPath)
	signals := make(chan *dbus.Signal, signalBufferSize)
	signalConn.Signal(signals)
	reactorCtx, cancelReactor := context.WithCancel(ctx)
	reactorDone := make(chan struct{})
	go func() {
		defer close(reactorDone)
		reactor := newEventReactor(service.units)
		stop := reactor.start(reactorCtx)
		defer stop()
		// Mirror runEventReactor: while the stream is up, callers trust in-memory
		// state instead of reading systemd. stopReactor waits for this goroutine,
		// so the flag is false by the time a test that drops the stream proceeds.
		service.reactorUp.Store(true)
		reactor.consume(reactorCtx, unitUpdates(reactorCtx, signals))
		service.reactorUp.Store(false)
	}()

	var stopReactorOnce sync.Once
	stopReactor := func() {
		stopReactorOnce.Do(func() {
			cancelReactor()
			select {
			case <-reactorDone:
			case <-time.After(5 * time.Second):
				t.Error("event reactor did not stop")
			}
		})
	}

	t.Cleanup(func() {
		stopReactor()
		signalConn.Close()
		conn.Close()
		if pinConn != nil {
			pinConn.Close()
		}
	})

	return &serviceIntegrationHarness{
		service:     service,
		conn:        conn,
		unitDir:     unitDir,
		namespace:   fmt.Sprintf("itest-%d-%d", os.Getpid(), time.Now().UnixNano()),
		stopReactor: stopReactor,
	}
}

func (h *serviceIntegrationHarness) task(t *testing.T, id string, cfg runcStubConfig, specOpts ...func(*specs.Spec)) (context.Context, *taskapi.CreateTaskRequest, string) {
	t.Helper()
	bundle := t.TempDir()
	spec := specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{},
		Root:    &specs.Root{Path: "rootfs"},
		Linux:   &specs.Linux{},
		Annotations: map[string]string{
			runcStubExitCodeAnnotation:  strconv.Itoa(cfg.ExitCode),
			runcStubExitDelayAnnotation: cfg.ExitDelay.String(),
			runcStubFailpointAnnotation: cfg.Failpoint,
		},
	}
	for _, o := range specOpts {
		o(&spec)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal OCI spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatalf("write OCI spec: %v", err)
	}

	createOptions, err := typeurl.MarshalAnyToProto(&options.CreateOptions{
		LogMode: options.LogMode_NULL,
	})
	if err != nil {
		t.Fatalf("marshal create options: %v", err)
	}
	req := &taskapi.CreateTaskRequest{
		ID:      id,
		Bundle:  bundle,
		Options: createOptions,
	}
	unit := unitName(h.namespace, id, "init")
	h.cleanupUnit(t, unit)
	return namespaces.WithNamespace(context.Background(), h.namespace), req, unit
}

func (h *serviceIntegrationHarness) exec(t *testing.T, ctx context.Context, task *taskapi.CreateTaskRequest, execID string, cfg runcStubConfig) string {
	t.Helper()
	process := specs.Process{
		Args: []string{"runc-stub-exec"},
		Env: []string{
			runcStubExitCodeEnv + "=" + strconv.Itoa(cfg.ExitCode),
			runcStubExitDelayEnv + "=" + cfg.ExitDelay.String(),
			runcStubFailpointEnv + "=" + cfg.Failpoint,
			runcStubExitBeforeDetachEnv + "=" + strconv.FormatBool(cfg.ExitBeforeDetach),
			runcStubWaitForReleaseEnv + "=" + strconv.FormatBool(cfg.WaitForRelease),
		},
	}
	data, err := json.Marshal(process)
	if err != nil {
		t.Fatalf("marshal exec process: %v", err)
	}
	if _, err := h.service.Exec(ctx, &taskapi.ExecProcessRequest{
		ID:     task.ID,
		ExecID: execID,
		Spec:   &anypb.Any{Value: data},
	}); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	unit := unitName(h.namespace, task.ID+"-"+execID, "exec")
	h.cleanupUnit(t, unit)
	return unit
}

func (h *serviceIntegrationHarness) releaseExec(t *testing.T, task *taskapi.CreateTaskRequest, execID string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(task.Bundle, "execs", execID, "release"), nil, 0600); err != nil {
		t.Fatalf("release exec process: %v", err)
	}
}

func (h *serviceIntegrationHarness) cleanupUnit(t *testing.T, unit string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _ = h.conn.StopUnitContext(ctx, unit, "replace", nil)
		if err := os.Remove(filepath.Join(h.unitDir, unit)); err != nil && !os.IsNotExist(err) {
			t.Logf("remove test unit %s: %v", unit, err)
		}
		if err := h.conn.ReloadContext(ctx); err != nil {
			t.Logf("reload systemd after cleaning %s: %v", unit, err)
		}
		if err := h.conn.ResetFailedUnitContext(ctx, unit); err != nil && !strings.Contains(err.Error(), "not loaded") {
			t.Logf("reset failed test unit %s: %v", unit, err)
		}
	})
}

func integrationSystemdManager(t *testing.T) (busPath, unitDir string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping systemd service integration test in short mode")
	}

	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		busPath := filepath.Join(runtimeDir, "systemd", "private")
		unitDir := filepath.Join(runtimeDir, "systemd", "user")
		if busInfo, busErr := os.Stat(busPath); busErr == nil && busInfo.Mode()&os.ModeSocket != 0 {
			if unitInfo, unitErr := os.Stat(unitDir); unitErr == nil && unitInfo.IsDir() {
				return "unix:path=" + busPath, unitDir
			}
		}
	}

	if os.Geteuid() == 0 {
		if info, err := os.Stat("/run/systemd/private"); err == nil && info.Mode()&os.ModeSocket != 0 {
			return systemdPrivateBus, systemUnitDir
		}
	}
	t.Skip("no writable user systemd manager is available")
	return "", ""
}

func waitForProcessExit(t *testing.T, events <-chan eventEnvelope, containerID, processID string) *eventsapi.TaskExit {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case envelope := <-events:
			event, ok := envelope.e.(*eventsapi.TaskExit)
			if ok && event.ContainerID == containerID && event.ID == processID {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for process exit event for %s/%s", containerID, processID)
		}
	}
}

func waitForExecStartedThenExit(t *testing.T, events <-chan eventEnvelope, containerID, execID string) (*eventsapi.TaskExecStarted, *eventsapi.TaskExit) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var started *eventsapi.TaskExecStarted
	for {
		select {
		case envelope := <-events:
			switch event := envelope.e.(type) {
			case *eventsapi.TaskExecStarted:
				if event.ContainerID == containerID && event.ExecID == execID {
					started = event
				}
			case *eventsapi.TaskExit:
				if event.ContainerID == containerID && event.ID == execID {
					if started == nil {
						t.Fatalf("received TaskExit before TaskExecStarted for %s/%s", containerID, execID)
					}
					return started, event
				}
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for exec lifecycle events for %s/%s", containerID, execID)
		}
	}
}

func assertNoExecLifecycleEvent(t *testing.T, events <-chan eventEnvelope, containerID, execID string, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case envelope := <-events:
			switch event := envelope.e.(type) {
			case *eventsapi.TaskExecStarted:
				if event.ContainerID == containerID && event.ExecID == execID {
					t.Fatalf("received TaskExecStarted for failed exec %s/%s", containerID, execID)
				}
			case *eventsapi.TaskExit:
				if event.ContainerID == containerID && event.ID == execID {
					t.Fatalf("received TaskExit for failed exec %s/%s", containerID, execID)
				}
			}
		case <-timer.C:
			return
		}
	}
}

func assertNoProcessExit(t *testing.T, events <-chan eventEnvelope, containerID, processID string, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case envelope := <-events:
			event, ok := envelope.e.(*eventsapi.TaskExit)
			if ok && event.ContainerID == containerID && event.ID == processID {
				t.Fatalf("received duplicate process exit event for %s/%s", containerID, processID)
			}
		case <-timer.C:
			return
		}
	}
}

func assertTaskRunAndExit(t *testing.T, h *serviceIntegrationHarness, ctx context.Context, req *taskapi.CreateTaskRequest, unit string, wantExit uint32) {
	t.Helper()

	created, err := h.service.Create(ctx, req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if created.Pid == 0 {
		t.Fatal("create returned a zero pid")
	}
	if _, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID}); err != nil {
		t.Fatalf("start task: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}
	if waited.ExitStatus != wantExit {
		t.Fatalf("wait exit status = %d, want %d", waited.ExitStatus, wantExit)
	}

	exited := waitForProcessExit(t, h.service.events, req.ID, req.ID)
	if exited.ExitStatus != wantExit {
		t.Fatalf("event exit status = %d, want %d", exited.ExitStatus, wantExit)
	}

	deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if deleted.ExitStatus != wantExit {
		t.Fatalf("delete exit status = %d, want %d", deleted.ExitStatus, wantExit)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, unit)); !os.IsNotExist(err) {
		t.Fatalf("unit file still exists after delete: %v", err)
	}
	assertNoProcessExit(t, h.service.events, req.ID, req.ID, 300*time.Millisecond)
}

func assertExecRunAndExit(t *testing.T, h *serviceIntegrationHarness, ctx context.Context, req *taskapi.CreateTaskRequest, execID, execUnit string, wantExit uint32) {
	t.Helper()

	started, err := h.service.Start(ctx, &taskapi.StartRequest{ID: req.ID, ExecID: execID})
	if err != nil {
		t.Fatalf("start exec: %v", err)
	}
	if started.Pid == 0 {
		t.Fatal("start exec returned a zero pid")
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waited, err := h.service.Wait(waitCtx, &taskapi.WaitRequest{ID: req.ID, ExecID: execID})
	if err != nil {
		t.Fatalf("wait for exec: %v", err)
	}
	if waited.ExitStatus != wantExit {
		t.Fatalf("wait exit status = %d, want %d", waited.ExitStatus, wantExit)
	}

	startedEvent, exited := waitForExecStartedThenExit(t, h.service.events, req.ID, execID)
	if startedEvent.Pid != started.Pid {
		t.Fatalf("started event pid = %d, want %d", startedEvent.Pid, started.Pid)
	}
	if exited.ExitStatus != wantExit {
		t.Fatalf("event exit status = %d, want %d", exited.ExitStatus, wantExit)
	}

	deleted, err := h.service.Delete(ctx, &taskapi.DeleteRequest{ID: req.ID, ExecID: execID})
	if err != nil {
		t.Fatalf("delete exec: %v", err)
	}
	if deleted.ExitStatus != wantExit {
		t.Fatalf("delete exit status = %d, want %d", deleted.ExitStatus, wantExit)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, execUnit)); !os.IsNotExist(err) {
		t.Fatalf("exec unit file still exists after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.Bundle, "execs", execID)); !os.IsNotExist(err) {
		t.Fatalf("exec state directory still exists after delete: %v", err)
	}
	assertNoProcessExit(t, h.service.events, req.ID, execID, 300*time.Millisecond)
}
