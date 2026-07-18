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
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	"github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
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
		if !waited.ExitedAt.After(timeZero) {
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
		if state.Status != tasktypes.StatusStopped {
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
			if !waited.ExitedAt.After(timeZero) {
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

type serviceIntegrationHarness struct {
	service   *Service
	conn      *systemd.Conn
	unitDir   string
	namespace string
}

func newServiceIntegrationHarness(t *testing.T) *serviceIntegrationHarness {
	t.Helper()
	busPath, unitDir := integrationSystemdManager(t)
	ctx := context.Background()
	conn := dialPrivate(t, ctx, busPath)

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
		runcBin: filepath.Join(helperDir, runcStubHelperName),
		exe:     filepath.Join(helperDir, shimCreateHelperName),
		unitDir: unitDir,
	})
	if err != nil {
		conn.Close()
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
		reactor.consume(reactorCtx, unitUpdates(reactorCtx, signals))
	}()

	t.Cleanup(func() {
		cancelReactor()
		signalConn.Close()
		select {
		case <-reactorDone:
		case <-time.After(5 * time.Second):
			t.Error("event reactor did not stop")
		}
		conn.Close()
	})

	return &serviceIntegrationHarness{
		service:   service,
		conn:      conn,
		unitDir:   unitDir,
		namespace: fmt.Sprintf("itest-%d-%d", os.Getpid(), time.Now().UnixNano()),
	}
}

func (h *serviceIntegrationHarness) task(t *testing.T, id string, cfg runcStubConfig) (context.Context, *taskapi.CreateTaskRequest, string) {
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
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal OCI spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatalf("write OCI spec: %v", err)
	}

	createOptions, err := typeurl.MarshalAny(&options.CreateOptions{
		LogMode:        options.LogMode_NULL,
		SdNotifyEnable: true,
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
		Spec:   &ptypes.Any{Value: data},
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
