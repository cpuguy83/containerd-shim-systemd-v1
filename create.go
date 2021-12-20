package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	v2runcopts "github.com/containerd/containerd/runtime/v2/runc/options"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/containerd/typeurl"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/golang/protobuf/proto"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
)

// Create a new container
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Create", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "create")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns))
	shimLog := OpenShimLog(ctx, r.Bundle)
	ctx = WithShimLog(ctx, shimLog)

	var opts CreateOptions
	if r.Options != nil && r.Options.TypeUrl != "" {
		v, err := typeurl.UnmarshalAny(r.Options)
		if err != nil {
			log.G(ctx).WithError(err).WithField("typeurl", r.Options.TypeUrl).Debug("invalid create options")
			return nil, fmt.Errorf("error unmarshalling options: %w", err)
		}

		switch vv := v.(type) {
		case *options.CreateOptions:
			opts.LogMode = vv.LogMode.String()
			opts.SdNotifyEnable = vv.SdNotifyEnable
			// TODO: Add other runc options to our CreateOptions.
		case *v2runcopts.Options:
			opts.NoPivotRoot = vv.NoPivotRoot
			opts.NoNewKeyring = vv.NoNewKeyring
			opts.IoUid = vv.IoUid
			opts.IoGid = vv.IoGid
			opts.BinaryName = vv.BinaryName
			opts.Root = vv.Root
			opts.CriuPath = vv.CriuPath
			opts.SystemdCgroup = vv.SystemdCgroup
			opts.CriuImagePath = vv.CriuImagePath
			opts.CriuWorkPath = vv.CriuWorkPath
		case *runctypes.CreateOptions:
			opts.NoPivotRoot = vv.NoPivotRoot
			opts.NoNewKeyring = vv.NoNewKeyring
			opts.IoUid = vv.IoUid
			opts.IoGid = vv.IoGid
			opts.CriuImagePath = vv.CriuImagePath
			opts.CriuWorkPath = vv.CriuWorkPath
			opts.ExternalUnixSockets = vv.ExternalUnixSockets
			opts.FileLocks = vv.FileLocks
			opts.Terminal = vv.Terminal
			opts.EmptyNamespaces = vv.EmptyNamespaces
		}
		log.G(ctx).WithField("typeurl", r.Options.TypeUrl).Debug("Decoding create options")
	}

	if opts.Root == "" {
		opts.Root = filepath.Join(s.root, "runc")
	}

	if opts.LogMode == "" {
		opts.LogMode = s.defaultLogMode.String()
	}

	var logPath string
	if s.debug {
		logPath = filepath.Join(r.Bundle, "init-runc-debug.log")
	}

	specData, err := ioutil.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("error reading spec: %w", err)
	}
	noNewNamespace := s.noNewNamespace

	if !noNewNamespace {
		// If the container rootfs is set to shared propagation we must not create use a private namespace.
		// Otherwise this could prevent the container from legitimately propoagating mounts to the host.
		var spec specs.Spec
		if err := json.Unmarshal(specData, &spec); err != nil {
			return nil, fmt.Errorf("error unmarshalling spec: %w", err)
		}
		if spec.Linux.RootfsPropagation == "shared" {
			noNewNamespace = true
		}
	}

	p := &initProcess{
		process: &process{
			ns:       ns,
			id:       r.ID,
			opts:     opts,
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
			systemd:  s.conn,
			runc: &runc.Runc{
				Debug:         s.debug,
				Command:       s.runcBin,
				SystemdCgroup: opts.SystemdCgroup,
				PdeathSignal:  syscall.SIGKILL,
				Root:          filepath.Join(opts.Root, ns),
				Log:           logPath,
			},
			exe:  s.exe,
			root: r.Bundle,
		},
		Bundle:           r.Bundle,
		Rootfs:           r.Rootfs,
		noNewNamespace:   noNewNamespace,
		checkpoint:       r.Checkpoint,
		parentCheckpoint: r.ParentCheckpoint,
		sendEvent:        s.send,
		execs: &processManager{
			ls: make(map[string]Process),
		},
		shimLog: shimLog,
	}
	p.process.cond = sync.NewCond(&p.process.mu)

	if err := s.processes.Add(path.Join(ns, r.ID), p); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			p.SetState(ctx, pState{ExitCode: 255, ExitedAt: time.Now(), Status: "failed"})
			log.G(ctx).WithError(retErr).Debug("Set state to failed")
			s.processes.Delete(path.Join(ns, r.ID))
			s.units.Delete(p)
			if _, err := p.Delete(ctx); err != nil {
				log.G(ctx).WithError(err).Error("error cleaning up failed process")
			}
		}
	}()

	pid, err := p.Create(ctx)
	if err != nil {
		return nil, err
	}
	s.units.Add(p)

	s.send(ctx, ns, &eventsapi.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventsapi.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Checkpoint: r.Checkpoint,
		Pid:        pid,
	})

	return &taskapi.CreateTaskResponse{Pid: pid}, nil
}

// Exec an additional process inside the container
func (s *Service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Exec", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "exec")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: process %s does not exist", errdefs.ErrNotFound, r.ID)
	}
	ctx = WithShimLog(ctx, p.LogWriter())
	pInit := p.(*initProcess)

	if r.Terminal {
		r.Stderr = ""
	}

	// TODO: In order to support shim restarts we need to persist this.
	ep := &execProcess{
		Spec:   r.Spec,
		parent: pInit,
		execID: r.ExecID,
		process: &process{
			ns:       ns,
			root:     pInit.root,
			id:       r.ExecID,
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
			systemd:  s.conn,
			exe:      s.exe,
			opts:     CreateOptions{LogMode: s.defaultLogMode.String()},
			runc: &runc.Runc{
				Debug:         s.debug,
				Command:       s.runcBin,
				SystemdCgroup: pInit.runc.SystemdCgroup,
				PdeathSignal:  syscall.SIGKILL,
				Root:          pInit.runc.Root,
			},
		}}

	ep.runc.Log = filepath.Join(ep.stateDir(), "runc-debug.log")
	ep.process.cond = sync.NewCond(&ep.process.mu)
	err = pInit.execs.Add(r.ExecID, ep)
	if err != nil {
		return nil, fmt.Errorf("process %s: %w", r.ExecID, err)
	}

	s.units.Add(ep)
	if err := ep.Create(ctx); err != nil {
		s.units.Delete(ep)
		pInit.execs.Delete(r.ExecID)
		return nil, err
	}

	s.send(ctx, ns, &eventsapi.TaskExecAdded{
		ContainerID: pInit.id,
		ExecID:      r.ExecID,
	})
	return &ptypes.Empty{}, nil
}

func (p *execProcess) pidFile() string {
	return filepath.Join(p.stateDir(), "pid")
}

func (p *execProcess) Create(ctx context.Context) error {
	if err := os.MkdirAll(p.stateDir(), 0700); err != nil {
		return err
	}

	v := p.Spec.Value
	if p.Terminal || p.opts.Terminal {
		var spec specs.Process
		if err := json.Unmarshal(p.Spec.Value, &spec); err != nil {
			return fmt.Errorf("error unmarshaling spec: %w", err)
		}
		spec.Terminal = true

		var err error
		v, err = json.Marshal(spec)
		if err != nil {
			return fmt.Errorf("error marshaling spec: %w", err)
		}
	}

	if err := os.WriteFile(p.processFilePath(), v, 0600); err != nil {
		return err
	}

	opts, err := p.startOptions()
	if err != nil {
		return err
	}

	if err := writeUnit(p.Name(), opts); err != nil {
		return err
	}
	if err := p.systemd.ReloadContext(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("failed to reload systemd")
	}
	// Make sure we don't have some old state from a past run.
	if err := p.systemd.ResetFailedUnitContext(ctx, p.Name()); err != nil && !strings.Contains(err.Error(), "not loaded") {
		log.G(ctx).WithError(err).Warn("Failed to reset systemd unit")
	}

	return nil
}

func (p *execProcess) stateDir() string {
	return filepath.Join(p.parent.Bundle, "execs", p.execID)
}

func (p *execProcess) processFilePath() string {
	return filepath.Join(p.stateDir(), "process.json")
}

func (p *initProcess) mountConfigPath() string {
	return filepath.Join(p.Bundle, "mounts.pb")
}

func (p *initProcess) writeMountConfig() error {
	req := taskapi.CreateTaskRequest{Bundle: p.Bundle, Rootfs: p.Rootfs}
	data, err := proto.Marshal(&req)
	if err != nil {
		return fmt.Errorf("error marshaling task create config")
	}

	if err := os.WriteFile(p.mountConfigPath(), data, 0600); err != nil {
		return err
	}
	return nil
}

func (p *initProcess) createRestore(ctx context.Context) error {
	if p.opts.CriuWorkPath == "" {
		p.opts.CriuWorkPath = filepath.Join(p.root, "criu-work")
	}
	// We seem to be missing Terminal info when doing a restore, so get that from the spec.
	data, err := os.ReadFile(filepath.Join(p.Bundle, "config.json"))
	if err != nil {
		return fmt.Errorf("could not read config.json: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("error unmarshalling config.json")
	}
	p.Terminal = spec.Process.Terminal

	execStart := []string{
		"restore",
		"--image-path=" + p.checkpoint,
		"--work-path=" + p.opts.CriuWorkPath,
		"--bundle=" + p.Bundle,
		"--no-pivot=" + strconv.FormatBool(p.opts.NoPivotRoot),
		"--no-subreaper",
	}

	if p.Terminal || p.opts.Terminal {
		execStart = append(execStart, "--detach")
		s, err := p.ttySockPath()
		if err != nil {
			return err
		}
		execStart = append(execStart, "--console-socket="+s)
		p.opts.ExternalUnixSockets = true
	}
	execStart = append(execStart, p.opts.RestoreArgs()...)

	unitOpts, err := p.startOptions(execStart)
	if err != nil {
		return err
	}

	if err := writeUnit(p.Name(), unitOpts); err != nil {
		return err
	}
	if err := p.systemd.ReloadContext(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("Error reloading systemd")
	}

	return nil
}

// For init processes we start a unit immediately.
// runc will hold a process open in the background and wait for the caller to setup namespaces and so on.
// Then once that is complete the caller will call "start", which we will just call `runc start`.
func (p *initProcess) Create(ctx context.Context) (_ uint32, retErr error) {
	ctx, span := StartSpan(ctx, "InitProcess.Create")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
			p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true})
			p.mu.Lock()
			p.deleted = true
			p.cond.Broadcast()
			p.mu.Unlock()
		}
		span.End()
	}()

	if err := p.writeMountConfig(); err != nil {
		return 0, err
	}

	if p.checkpoint != "" {
		return 0, p.createRestore(ctx)

	}

	rcmd := []string{
		"create",
		"--bundle=" + p.Bundle,
		"--no-pivot=" + strconv.FormatBool(p.opts.NoPivotRoot),
		"--no-new-keyring=" + strconv.FormatBool(p.opts.NoNewKeyring),
		"--pid-file=" + p.pidFile(),
	}
	if p.Terminal || p.opts.Terminal {
		s, err := p.ttySockPath()
		if err != nil {
			return 0, err
		}
		rcmd = append(rcmd, "--console-socket="+s)
	}

	unitOpts, err := p.startOptions(rcmd)
	if err != nil {
		return 0, err
	}

	if p.Terminal || p.opts.Terminal {
		u, _, err := p.makePty(ctx)
		if err != nil {
			return 0, err
		}

		defer func() {
			if retErr != nil {
				p.systemd.KillUnitContext(ctx, u, int32(syscall.SIGKILL))
			}
		}()
	}

	if err := writeUnit(p.Name(), unitOpts); err != nil {
		return 0, err
	}
	if err := p.systemd.ReloadContext(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("Error reloading systemd")
	}
	// Make sure we don't have some old state from a past run.
	if err := p.systemd.ResetFailedUnitContext(ctx, p.Name()); err != nil && !strings.Contains(err.Error(), "not loaded") {
		log.G(ctx).WithError(err).Warn("Failed to reset systemd unit")
	}

	return p.startUnit(ctx)
}

func (p *initProcess) startUnit(ctx context.Context) (uint32, error) {
	uName := p.Name()

	do := func() error {
		ch := make(chan string, 1)
		p.systemd.ResetFailedUnitContext(ctx, p.Name())
		if _, err := p.systemd.StartUnitContext(ctx, uName, "replace", ch); err != nil {
			if err := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err != nil && !strings.Contains(err.Error(), "not found") {
				log.G(ctx).WithError(err).Info("Error deleting container in runc")
			}
			if err := p.systemd.ResetFailedUnitContext(ctx, uName); err != nil {
				log.G(ctx).WithError(err).Info("Error resetting failed unit")
			}

			ch = make(chan string, 1)
			if _, err := p.systemd.StartUnitContext(ctx, uName, "replace", ch); err != nil {
				return fmt.Errorf("error starting unit: %w", err)
			}
		}

		select {
		case <-ctx.Done():
			p.Kill(ctx, int(syscall.SIGKILL), true)
			return ctx.Err()
		case status := <-ch:
			if status != "done" {
				return fmt.Errorf("error starting systemd unit: %s", status)
			}
		}

		return nil
	}

	handlePid := func() (uint32, error) {
		p.LoadState(ctx)
		pid := p.Pid()
		if pid == 0 {
			var err error
			pid, err = p.readPidFile()
			if err != nil {
				return 0, fmt.Errorf("error reading pid file: %w", err)
			}
		}

		p.mu.Lock()
		if p.state.Pid == 0 {
			p.state.Pid = uint32(pid)
		}
		p.mu.Unlock()
		if pid > 0 {
			if p.ProcessState().Exited() {
				p.cond.Broadcast()
			}
		}
		return uint32(pid), nil
	}

	if err := do(); err != nil {
		if pid, err := handlePid(); err == nil {
			return pid, nil
		} else {
			log.G(ctx).WithError(err).Debug("Error getting pid")
		}

		ch := make(chan string, 1)
		if _, err := p.systemd.StopUnitContext(ctx, p.Name(), "replace", ch); err != nil {
			log.G(ctx).WithError(err).Info("Error stopping unit")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ch:
		}

		// Clean up old state and try again
		if err2 := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err2 != nil {
			log.G(ctx).WithError(err2).Info("Error deleting container in runc")
		}
		if err := do(); err != nil {
			ret := err
			if p.runc.Debug {
				ret = fmt.Errorf("%w:\n%s", err, p.Name())
				unitData, err := os.ReadFile("/run/systemd/system/" + uName)
				if err == nil {
					ret = fmt.Errorf("%w:\n%s", ret, string(unitData))
				}
				logData, err := os.ReadFile(p.runc.Log)
				if err == nil {
					ret = fmt.Errorf("%w\n%s", ret, string(logData))
				}
			}
			if err2 := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err2 != nil {
				log.G(ctx).WithError(err2).Debug("Error deleting container in runc")
			}
			return 0, ret
		}
	}

	return handlePid()
}

func (p *initProcess) readPidFile() (uint32, error) {
	pidData, err := os.ReadFile(p.pidFile())
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		return 0, fmt.Errorf("error parsing pid file: %w", err)
	}

	return uint32(pid), nil
}

type waitStatus struct {
	Status int
	Err    error
	Pid    uint32
}

func reap(ctx context.Context, chChld chan os.Signal, wait chan waitStatus, chProc <-chan *os.Process) {
	signal.Notify(chChld, syscall.SIGCHLD)

	// Wait for process start
	var proc *os.Process
	select {
	case <-ctx.Done():
	case proc = <-chProc:

	}

	for range chChld {
		var ws unix.WaitStatus

		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		log.G(ctx).WithError(err).Debugf("wait4: %d / %d", pid, ws.ExitStatus())
		if err != nil {
			if err != unix.EINTR {
				wait <- waitStatus{Err: err}
				return
			}
			continue
		}

		if pid == 0 {
			// I don't think this should happen, but worth checking anyway.
			continue
		}

		log.G(ctx).WithField("pid", pid).WithField("exitStatus", ws.ExitStatus()).Debug("wait4")

		if ws.ExitStatus() != 0 {
			wait <- waitStatus{Status: ws.ExitStatus(), Pid: uint32(pid)}
			return
		}

		if proc.Pid == pid {
			continue
		}

		wait <- waitStatus{Status: ws.ExitStatus(), Pid: uint32(pid)}
		return
	}
}

func createCmd(ctx context.Context, bundle string, cmdLine []string, tty, noReap bool) error {
	log.G(ctx).Debugf("%s %s", cmdLine[0], cmdLine[1:])

	cmd := exec.Command(cmdLine[0], cmdLine[1:]...)

	// Open all fifos with O_RDWR first so that we don't block trying to open
	// Then open with the correct permissions which get passed to runc.
	// Very important to use the correct open perms so that when one side of the fifo closes the process gets the close notification.
	//
	// TODO: Do we need to use O_RDWR? I recall there was some issues early on that we were having so I'm leaving it in for now.
	if p := os.Getenv("STDIN_FIFO"); p != "" {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()

		f2, err := os.OpenFile(p, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer f2.Close()
		cmd.Stdin = f2
	} else {
		log.G(ctx).Debug("No stdin pipe")
	}

	if p := os.Getenv("STDOUT_FIFO"); p != "" {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()

		f2, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f2.Close()

		cmd.Stdout = f2
	} else {
		log.G(ctx).Debug("No stdout pipe")
	}

	if p := os.Getenv("STDERR_FIFO"); p != "" {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			// Ignore errors on this if we have a TTY
			// Often we'll get a file path here but no actual fifo is created with TTY's.
			// Reason being that there is no stderr for TTY.
			if !tty {
				return err
			}
		} else {
			defer f.Close()

			f2, err := os.OpenFile(p, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f2.Close()
			cmd.Stderr = f2
		}
	} else {
		log.G(ctx).Debug("No stderr pipe")
	}

	wait := make(chan waitStatus, 1)
	chChld := make(chan os.Signal, 1)
	chProc := make(chan *os.Process, 1)
	defer signal.Stop(chChld)

	var readPid uint32
	if !noReap {
		go reap(ctx, chChld, wait, chProc)

		var i uintptr = 1
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(i), 0, 0, 0); err != nil {
			log.G(ctx).WithError(err).Error("failed to set child subreaper")
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	chProc <- cmd.Process

	defer cmd.Wait()

	if noReap {
		return nil
	}

	chPid := make(chan int)
	go func() {
		pidFile := os.Getenv("PIDFILE")
		for {
			done := func() bool {
				_, err := os.Stat(pidFile)
				if err != nil {
					return false
				}

				f, err := os.Open(os.Getenv("PIDFILE"))
				if err != nil {
					log.G(ctx).WithError(err).Debug("Error opening pidfile")
					return false
				}
				defer f.Close()

				pidData, err := ioutil.ReadAll(f)
				if err != nil {
					log.G(ctx).WithError(err).Debug("Error reading pidfile")
					return false
				}

				pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
				if err != nil {
					log.G(ctx).WithError(err).Debug("Error parsing pidfile")
					return false
				}

				atomic.StoreUint32(&readPid, uint32(pid))
				chPid <- pid
				return true
			}()

			if done {
				return
			}
			<-time.After(100 * time.Millisecond)
		}
	}()

	var st pState

	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-wait:
		st.ExitCode = uint32(status.Status)
		st.ExitedAt = time.Now()
		st.Pid = status.Pid
	case <-time.After(time.Second):
		log.G(ctx).Debug("Process is up")
	case pid := <-chPid:
		st.Pid = uint32(pid)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-wait:
			st.ExitCode = uint32(status.Status)
			st.ExitedAt = time.Now()
		case <-time.After(time.Second):
			log.G(ctx).Debug("Process is up")
		}
	}

	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("error marshalling state: %v", err)
	}

	if err := os.WriteFile(os.Getenv("EXIT_STATE_PATH"), data, 0600); err != nil {
		return fmt.Errorf("error writing state: %v", err)
	}

	return nil
}
