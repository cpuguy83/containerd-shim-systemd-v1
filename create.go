package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3"
	cgroupsv1 "github.com/containerd/cgroups/v3/cgroup1"
	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2"
	eventsapi "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	v2runcopts "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	"github.com/coreos/go-systemd/v22/daemon"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

const legacyRuncOptionsTypeURL = "containerd.linux.runc.CreateOptions"

func unmarshalCreateOptions(value *anypb.Any) (interface{}, error) {
	if value.TypeUrl == legacyRuncOptionsTypeURL {
		var opts v2runcopts.Options
		if err := proto.Unmarshal(value.Value, &opts); err != nil {
			return nil, err
		}
		return &opts, nil
	}
	return typeurl.UnmarshalAny(value)
}

// Create a new container
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Create", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("create: %w", retErr))
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns))
	shimLog := OpenShimLog(ctx, r.Bundle)
	ctx = WithShimLog(ctx, shimLog)

	var opts CreateOptions
	if r.Options != nil && r.Options.TypeUrl != "" {
		v, err := unmarshalCreateOptions(r.Options)
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
			opts.SystemdCgroup = vv.SystemdCgroup
			opts.CriuImagePath = vv.CriuImagePath
			opts.CriuWorkPath = vv.CriuWorkPath
			opts.ShimCgroup = vv.ShimCgroup
		}
		log.G(ctx).WithField("typeurl", r.Options.TypeUrl).Debug("Decoding create options")
	}

	if opts.Root == "" {
		opts.Root = filepath.Join(s.root, "runc")
	}

	if opts.LogMode == "" {
		opts.LogMode = s.defaultLogMode.String()
	}

	runcCommand := s.runcBin
	if opts.BinaryName != "" {
		runcCommand, err = exec.LookPath(opts.BinaryName)
		if err != nil {
			return nil, fmt.Errorf("failed to look up runc binary %q: %w", opts.BinaryName, err)
		}
	}

	var logPath string
	if s.debug {
		logPath = filepath.Join(r.Bundle, "init-runc-debug.log")
	}

	specData, err := ioutil.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("error reading spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return nil, fmt.Errorf("error unmarshalling spec: %w", err)
	}

	noNewNamespace := s.noNewNamespace

	if !noNewNamespace {
		// If the container rootfs is set to shared propagation we must not create use a private namespace.
		// Otherwise this could prevent the container from legitimately propoagating mounts to the host.
		if spec.Linux != nil && spec.Linux.RootfsPropagation == "shared" {
			noNewNamespace = true
		}
	}

	p := &initProcess{
		process: &process{
			ns:       ns,
			id:       r.ID,
			opts:     opts,
			events:   newEventOutbox(ns, s.send),
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
			systemd:  s.conn,
			pinConn:  s.pinConn,
			pinRef:   s.pinRef,
			runc: &runc.Runc{
				Debug:         s.debug,
				Command:       runcCommand,
				SystemdCgroup: opts.SystemdCgroup,
				PdeathSignal:  syscall.SIGKILL,
				Root:          filepath.Join(opts.Root, ns),
				Log:           logPath,
				WorkDir:       r.Bundle,
			},
			exe:        s.exe,
			root:       r.Bundle,
			shimCgroup: opts.ShimCgroup,
		},
		Bundle:           r.Bundle,
		Rootfs:           r.Rootfs,
		noNewNamespace:   noNewNamespace,
		killAllOnExit:    shouldKillAllOnExit(&spec),
		checkpoint:       r.Checkpoint,
		parentCheckpoint: r.ParentCheckpoint,
		execs: &processManager{
			ls: make(map[string]Process),
		},
		shimLog: shimLog,
	}
	p.process.cond = sync.NewCond(&p.process.mu)
	p.process.pathName = systemd.PathBusEscape(p.Name())

	if err := s.processes.Add(path.Join(ns, r.ID), p); err != nil {
		return nil, err
	}
	s.units.Add(p)

	defer func() {
		if retErr != nil {
			ctx, cancel := cleanupContext(ctx)
			defer cancel()

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
	p.SetState(ctx, pState{Pid: pid, Status: "created"})

	p.events.Send(ctx, &eventsapi.TaskCreate{
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
func (s *Service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Exec", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("exec: %w", retErr))
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
			events:   newEventOutbox(ns, s.send),
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
			systemd:  s.conn,
			pinConn:  s.pinConn,
			pinRef:   s.pinRef,
			exe:      s.exe,
			opts:     CreateOptions{LogMode: s.defaultLogMode.String()},
			runc: &runc.Runc{
				Debug:         s.debug,
				Command:       pInit.runc.Command,
				SystemdCgroup: pInit.runc.SystemdCgroup,
				PdeathSignal:  syscall.SIGKILL,
				Root:          pInit.runc.Root,
				WorkDir:       pInit.Bundle,
			},
		}}

	ep.runc.Log = filepath.Join(ep.stateDir(), "runc-debug.log")
	ep.process.cond = sync.NewCond(&ep.process.mu)
	ep.process.pathName = systemd.PathBusEscape(ep.Name())
	err = pInit.execs.Add(r.ExecID, ep)
	if err != nil {
		return nil, fmt.Errorf("process %s: %w", r.ExecID, err)
	}

	s.units.Add(ep)

	// The container may have started being deleted while this exec was being
	// registered, after the delete had already swept the exec list. Registering
	// then checking means one of the two always sees the other: either the sweep
	// finds this exec, or this sees the flag the delete set before sweeping. An
	// exec left behind here would never be reachable to clean up.
	if pInit.isDeleting() {
		s.units.Delete(ep)
		pInit.execs.Delete(r.ExecID)
		return nil, fmt.Errorf("container %s is being deleted: %w", pInit.id, errdefs.ErrFailedPrecondition)
	}

	if err := ep.Create(ctx); err != nil {
		s.units.Delete(ep)
		pInit.execs.Delete(r.ExecID)
		return nil, err
	}

	ep.events.Send(ctx, &eventsapi.TaskExecAdded{
		ContainerID: pInit.id,
		ExecID:      r.ExecID,
	})
	return &emptypb.Empty{}, nil
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

	// The transient unit is registered and started together in Start; nothing
	// to create in systemd here.
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

	unitProps, err := p.startProperties(execStart)
	if err != nil {
		return err
	}
	// startUnit runs later (from Start -> restore); hold the properties until then.
	p.unitProps = unitProps

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
			cleanupCtx, cancel := cleanupContext(ctx)
			defer cancel()
			p.runc.Delete(cleanupCtx, p.id, &runc.DeleteOpts{Force: true})
			// Creation failed, so nothing will ever run under this process.
			// Record that as its exit: it wakes anyone waiting on a process that
			// can no longer start, and it leaves the caller's cleanup (which
			// refines the exit and then tears the unit down) working against a
			// process that has exited rather than one already marked deleted.
			p.SetState(cleanupCtx, pState{ExitCode: 255, ExitedAt: time.Now(), Status: "failed"})
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

	unitProps, err := p.startProperties(rcmd)
	if err != nil {
		return 0, err
	}

	if p.Terminal || p.opts.Terminal {
		sockPath, err := p.ttySockPath()
		if err != nil {
			return 0, err
		}
		u, _, err := p.makePty(ctx, sockPath)
		if err != nil {
			return 0, err
		}

		defer func() {
			if retErr != nil {
				cleanupCtx, cancel := cleanupContext(ctx)
				defer cancel()
				p.systemd.KillUnitContext(cleanupCtx, u, int32(syscall.SIGKILL))
			}
		}()
	}

	p.unitProps = unitProps
	pid, err := p.startUnit(ctx)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func (p *initProcess) startUnit(ctx context.Context) (uint32, error) {
	uName := p.Name()

	do := func() error {
		p.clearRecordedSystemdExitState()
		ch := make(chan string, 1)
		p.systemd.ResetFailedUnitContext(ctx, p.Name())
		if err := p.startTransientUnit(ctx, uName, "replace", p.unitProps, ch); err != nil {
			if err := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err != nil && !strings.Contains(err.Error(), "not found") {
				log.G(ctx).WithError(err).Info("Error deleting container in runc")
			}
			if err := p.systemd.ResetFailedUnitContext(ctx, uName); err != nil {
				log.G(ctx).WithError(err).Info("Error resetting failed unit")
			}

			ch = make(chan string, 1)
			if err := p.startTransientUnit(ctx, uName, "replace", p.unitProps, ch); err != nil {
				return fmt.Errorf("error starting unit: %w", err)
			}
		}

		select {
		case <-ctx.Done():
			p.Kill(ctx, int(syscall.SIGKILL), true)
			return ctx.Err()
		case status := <-ch:
			log.G(ctx).WithField("status", status).Info("Unit Status")
			if status != "done" {
				return fmt.Errorf("error starting systemd unit: %s", status)
			}

			if err := p.LoadState(ctx); err != nil {
				return err
			}

			if p.ProcessState().Exited() {
				return fmt.Errorf("container exited immediately, code: %d", p.ProcessState().ExitCode)
			}
		}

		return nil
	}

	handlePid := func() (uint32, error) {
		if err := p.LoadState(ctx); err != nil {
			log.G(ctx).WithError(err).Error("Error loading state")
		}
		for retries := 0; retries < 10 && p.Pid() == 0 && !p.ProcessState().Exited(); retries++ {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}

			time.Sleep(10 * time.Millisecond)
			if err := p.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).Error("Error loading state")
			}
		}
		pid := p.Pid()

		p.mu.Lock()
		if p.state.Pid == 0 {
			p.state.Pid = uint32(pid)
		}
		p.mu.Unlock()
		var err error
		if p.ProcessState().Exited() {
			p.cond.Broadcast()
			err = fmt.Errorf("container exited immediately, code: %d", p.ProcessState().ExitCode)
		}
		return uint32(pid), err
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

		// The stopped attempt above still holds its AddRef reference, which keeps
		// the unit loaded and would make the re-create below fail "already exists".
		// Release it now that the unit is stopped so systemd can collect it; the
		// retry's start takes a fresh reference.
		p.releaseUnit(ctx, p.Name())

		// Clean up old state and try again
		if err2 := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err2 != nil {
			log.G(ctx).WithError(err2).Info("Error deleting container in runc")
		}
		if err := do(); err != nil {
			ret := err
			if p.runc.Debug {
				ret = fmt.Errorf("%w:\n%s\n%s", err, p.Name(), formatUnitProperties(p.unitProps))
				logData, err := os.ReadFile(p.runc.Log)
				if err == nil {
					ret = fmt.Errorf("%w\n%s", ret, string(logData))
				}

				ttyData, err := os.ReadFile(filepath.Join(p.root, p.id+"-tty.log"))
				if err == nil {
					ret = fmt.Errorf("%w\n%s", ret, string(ttyData))
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

func createCmd(ctx context.Context, bundle string, cmdLine []string, tty, noReap bool) (retErr error) {
	log.G(ctx).Debugf("%s %s", cmdLine[0], cmdLine[1:])

	if err := setCgroup(); err != nil {
		log.G(ctx).WithError(err).Error("Error setting cgroup")
	}

	cmd := exec.Command(cmdLine[0], cmdLine[1:]...)

	// Open all fifos with O_RDWR first so that we don't block trying to open
	// Then open with the correct permissions which get passed to runc.
	// Very important to use the correct open perms so that when one side of the fifo closes the process gets the close notification.
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

	if !noReap {
		// runc detaches, so it exits 0 as soon as the workload is started and its
		// own status says nothing about the workload. Become the workload's
		// subreaper so that if it exits before systemd is told about it, its status
		// is delivered here rather than to a parent that will discard it.
		var i uintptr = 1
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, i, 0, 0, 0); err != nil {
			log.G(ctx).WithError(err).Error("failed to set child subreaper")
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	log.G(ctx).Debugf("runc pid: %d", cmd.Process.Pid)

	var st pState

	defer func() {
		if retErr != nil {
			return
		}
		var t time.Time
		if st.ExitCode == 0 {
			return
		}
		if st.ExitedAt.Equal(t) {
			return
		}
		retErr = fmt.Errorf("process exited with code %d", st.ExitCode)
	}()

	writeFile := func() error {
		data, err := json.Marshal(st)
		if err != nil {
			return fmt.Errorf("error marshalling state: %v", err)
		}

		if err := os.WriteFile(os.Getenv("EXIT_STATE_PATH"), data, 0600); err != nil {
			return fmt.Errorf("error writing state: %v", err)
		}
		return nil
	}

	if err := cmd.Wait(); err != nil {
		// runc exited non-zero
		st.ExitCode = uint32(cmd.ProcessState.ExitCode())
		st.ExitedAt = time.Now()
		st.Status = exitedInit
		st.Pid = uint32(cmd.Process.Pid)
		err = writeFile()
		sdNotify(ctx, notifyErrno(st.ExitCode), notifyStatus(st.Status))
		return err
	}

	// runc detaches, so it has written the pid of the process it left behind
	// before exiting.
	pid, err := readPidFile(ctx, os.Getenv("PIDFILE"))
	if err != nil {
		return err
	}

	st = pState{Pid: uint32(pid)}
	if !noReap {
		if st, err = workloadState(pid); err != nil {
			return err
		}
	}

	if err := writeFile(); err != nil {
		return err
	}
	notifyWorkload(ctx, st)

	if st.Status != "running" {
		return nil
	}

	// sd_notify is a datagram with no reply, so the handoff above is not yet a
	// fact. Wait for systemd to have processed it, then look once more: systemd
	// refuses a main pid that has already become a zombie, so a workload that
	// died while the handoff was in flight is still this process' to report.
	if err := sdNotifyBarrier(ctx); err != nil {
		// Without the barrier there is no ordering guarantee, but the re-check
		// below is what actually recovers a workload that died during the
		// handoff. Skipping it because the barrier failed would leave that exit
		// reported by nobody, which is the failure this whole path exists for.
		log.G(ctx).WithError(err).Warn("Error waiting for systemd to take the main pid; re-checking anyway")
	}
	if st, err = workloadState(pid); err != nil {
		log.G(ctx).WithError(err).Warn("Error re-checking the workload after handoff")
		return nil
	}
	if st.Status == "running" {
		// systemd has the pid and the pid is alive, so systemd owns its exit.
		return nil
	}
	if err := writeFile(); err != nil {
		return err
	}
	notifyWorkload(ctx, st)
	return nil
}

// pidFileTimeout bounds the wait for runc's pid file. runc writes it before it
// exits, so this only covers a write that has not landed yet.
const pidFileTimeout = 5 * time.Second

// readPidFile reads the pid runc recorded for the process it left behind.
func readPidFile(ctx context.Context, path string) (int, error) {
	deadline := time.Now().Add(pidFileTimeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if pid, err = strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid, nil
			}
			err = fmt.Errorf("invalid pid in %s: %w", path, err)
		}

		if time.Now().After(deadline) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// workloadState reports the state of the process runc left behind: whether it
// has already exited, and with what status.
//
// This is the only wait4 this process makes. runc always exits 0 once it has
// detached, so its status says nothing about the workload; the workload was
// reparented here when runc exited -- this process is its subreaper -- so while
// it is a child here its exit status is delivered here and nowhere else.
//
// A workload that is still running belongs to systemd instead, and must not be
// waited on again: systemd inherits it when this process exits and reports its
// exit status, which a later wait4 here would consume and throw away.
func workloadState(pid int) (pState, error) {
	var ws unix.WaitStatus
	reaped, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
	if err != nil {
		// ECHILD included: the workload is not this process' child, so nothing
		// here can say whether it ran or what it returned, and reporting that as
		// an exit status of 0 is the failure worth avoiding.
		return pState{}, fmt.Errorf("error checking on process %d: %w", pid, err)
	}
	if reaped != pid {
		return pState{Pid: uint32(pid), Status: "running"}, nil
	}

	st := pState{
		Pid:      uint32(pid),
		ExitCode: uint32(ws.ExitStatus()),
		ExitedAt: time.Now(),
		Status:   "exited",
	}
	if ws.Signaled() {
		// A killed workload has no exit code. containerd reports termination by
		// signal as 128+signal (see its reaper's exitStatus), which is what its
		// clients render as e.g. 137 for SIGKILL, so report the same here rather
		// than the bare signal number systemd puts in ExecMainStatus.
		st.ExitCode = uint32(exitSignalOffset + int(ws.Signal()))
	}
	return st, nil
}

func notifyWorkload(ctx context.Context, st pState) {
	switch st.Status {
	case "exited":
		sdNotify(ctx, notifyStatus(st.Status), notifyErrno(st.ExitCode), notifyMainPID(st.Pid))
	case "running":
		sdNotify(ctx, daemon.SdNotifyReady, notifyMainPID(st.Pid))
		log.G(ctx).Debug("Process is up!")
	}
}

func shouldKillAllOnExit(spec *specs.Spec) bool {
	if spec.Linux != nil {
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.PIDNamespace && ns.Path == "" {
				return false
			}
		}
	}
	return true
}

func notifyMainPID(pid uint32) string {
	return fmt.Sprintf("MAINPID=%d", pid)
}

func notifyStatus(status string) string {
	return fmt.Sprintf("STATUS=%s", status)
}

func notifyErrno(errno uint32) string {
	return fmt.Sprintf("ERRNO=%d", errno)
}

func sdNotify(ctx context.Context, status ...string) {
	if _, err := daemon.SdNotify(false, strings.Join(status, "\n")); err != nil {
		log.G(ctx).WithError(err).Error("failed to notify systemd")
	}
}

// notifyBarrierTimeout bounds the wait for systemd to catch up on this process'
// notifications. It only has to cover systemd getting round to a queued
// datagram; a manager too busy for that is a manager whose answer would be stale
// anyway.
const notifyBarrierTimeout = 5 * time.Second

// sdNotifyBarrier blocks until systemd has processed every notification this
// process has sent, implementing sd_notify(3)'s BARRIER=1.
//
// A notification is a datagram with no reply, so sending one says nothing about
// whether systemd has acted on it. systemd holds the pipe end passed here until
// it has drained everything queued ahead of it, so reading EOF means the earlier
// notifications have been handled.
func sdNotifyBarrier(ctx context.Context) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}

	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("error opening notify socket: %w", err)
	}
	defer unix.Close(fd)

	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()

	// A leading '@' is how systemd advertises an abstract socket, which is what
	// SockaddrUnix takes it to mean too.
	err = unix.Sendmsg(fd, []byte("BARRIER=1"), unix.UnixRights(int(w.Fd())), &unix.SockaddrUnix{Name: addr}, 0)
	// Only systemd's copy of the write end may keep the pipe open.
	w.Close()
	if err != nil {
		return fmt.Errorf("error sending notify barrier: %w", err)
	}

	deadline := time.Now().Add(notifyBarrierTimeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := r.SetReadDeadline(deadline); err != nil {
		return err
	}
	if _, err := r.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("error waiting on notify barrier: %w", err)
	}
	return nil
}

type cgMode cgroups.CGMode

func (m cgMode) String() string {
	switch cgroups.CGMode(m) {
	case cgroups.Unified:
		return "unified"
	case cgroups.Hybrid:
		return "hybrid"
	case cgroups.Legacy:
		return "legacy"
	default:
		return "unknown"
	}
}

func setCgroup() error {
	cgPath := os.Getenv("SHIM_CGROUP")
	if cgPath == "" {
		return nil
	}

	if cgroups.Mode() == cgroups.Unified {
		cg, err := cgroupsv2.Load(cgPath)
		if err != nil {
			return fmt.Errorf("cgroups v2 mode %s: error loading cgroup: %w", cgMode(cgroups.Mode()), err)
		}
		if err := cg.AddProc(uint64(os.Getpid())); err != nil {
			return fmt.Errorf("error adding proc to cgroup: %v", err)
		}
	} else {
		cg, err := cgroupsv1.Load(cgroupsv1.StaticPath(cgPath))
		if err != nil {
			return fmt.Errorf("cgroups v1 mode %s: error loading cgroup: %w", cgMode(cgroups.Mode()), err)
		}
		if err := cg.AddProc(uint64(os.Getpid())); err != nil {
			return fmt.Errorf("error adding proc to cgroup: %v", err)
		}
	}

	return nil
}
