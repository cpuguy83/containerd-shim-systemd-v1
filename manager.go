package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/moby/locker"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go4.org/syncutil/singleflight"
)

var (
	timeZero = time.UnixMicro(0)
)

type Config struct {
	Root      string
	Publisher events.Publisher
	LogMode   options.LogMode
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, err
	}

	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, fmt.Errorf("error looking up runc path: %w", err)
	}

	runcRoot := filepath.Join(cfg.Root, "runc")
	if err := os.MkdirAll(runcRoot, 0710); err != nil {
		return nil, err
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	debug := logrus.GetLevel() >= logrus.DebugLevel
	return &Service{
		conn:           conn,
		exe:            exe,
		root:           cfg.Root,
		publisher:      cfg.Publisher,
		events:         make(chan eventEnvelope, 128),
		waitEvents:     make(chan struct{}),
		defaultLogMode: cfg.LogMode,
		single:         &singleflight.Group{},
		ttyMu:          locker.New(),
		ttys:           make(map[string]net.Conn),
		runc: &runc.Runc{
			Debug:         debug,
			Command:       runcPath,
			SystemdCgroup: false,
			PdeathSignal:  syscall.SIGKILL,
			Root:          runcRoot,
		}}, nil
}

type Service struct {
	conn       *systemd.Conn
	runc       *runc.Runc
	root       string
	publisher  events.Publisher
	events     chan eventEnvelope
	waitEvents chan struct{}

	single *singleflight.Group

	ttyMu *locker.Locker
	ttys  map[string]net.Conn

	defaultLogMode options.LogMode

	// exe is used to re-exec the shim binary to start up a pty copier
	exe string
}

// Cleanup is a binary call that cleans up any resources used by the shim when the Service crashes
func (s *Service) Cleanup(ctx context.Context) (*taskapi.DeleteResponse, error) {
	return &taskapi.DeleteResponse{}, nil
}

func unitName(ns, id, t string) string {
	if t == "" {
		t = "service"
	}
	return "containerd-" + ns + "-" + id + "." + t
}

func runcName(ns, id string) string {
	return ns + "-" + id
}

// Create a new container
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	name := unitName(ns, r.ID, "service")
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Create started")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Create end")
	}()

	var opts options.CreateOptions
	if r.Options != nil {
		if err := typeurl.UnmarshalTo(r.Options, &opts); err != nil {
			return nil, errdefs.ToGRPC(err)
		}
	}

	if opts.LogMode == options.LogMode_DEFAULT {
		opts.LogMode = s.defaultLogMode
	}

	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	pidFile := filepath.Join(r.Bundle, "pid")

	execStart := []string{runcPath, "--debug=" + strconv.FormatBool(s.runc.Debug), "--systemd-cgroup=" + strconv.FormatBool(s.runc.SystemdCgroup), "--root", s.runc.Root, "create", "--bundle=" + r.Bundle}
	if !opts.SdNotifyEnable {
		execStart = append(execStart, []string{"--pid-file", pidFile}...)
	}

	if len(r.Rootfs) > 0 {
		var mounts []mount.Mount
		for _, m := range r.Rootfs {
			mounts = append(mounts, mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			})
		}

		rootfs := filepath.Join(r.Bundle, "rootfs")
		if err := os.Mkdir(rootfs, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if err := mount.All(mounts, rootfs); err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				mount.UnmountAll(rootfs, 0)
			}
		}()
	}

	var properties []systemd.Property
	if opts.SdNotifyEnable {
		properties = []systemd.Property{systemd.PropType("notify")}
	} else {
		properties = []systemd.Property{
			systemd.PropType("forking"),
			{Name: "PIDFile", Value: dbus.MakeVariant(pidFile)},
		}
	}

	var ttyHandler []systemd.Property
	if r.Terminal {
		// TODO: We need to bind the pty copier to the container's lifecycle
		// This would ensure that once the container exits, the pty copier exits

		sockPath := ttySockPath(s.root, ns, r.ID, "")

		ttyHandler = []systemd.Property{
			systemd.PropType("notify"),
			systemd.PropExecStart([]string{s.exe}, false),
			systemd.PropDescription("TTY Handshake for " + name),
			{Name: "Environment", Value: dbus.MakeVariant([]string{
				ttyHandshakeEnv + "=1",
				ttySockPathEnv + "=" + sockPath,
			})},
			{Name: "StandardInputFile", Value: dbus.MakeVariant(r.Stdin)},
			{Name: "StandardOutputFile", Value: dbus.MakeVariant(r.Stdout)},
			{Name: "StandardError", Value: dbus.MakeVariant("journal")},
		}

		// Add the console socket option to runc's exec-start
		execStart = append(execStart, "--console-socket", sockPath)

		ttyUnit := unitName(ns, r.ID+"-tty", "service")
		defer func() {
			if retErr != nil {
				s.conn.StopUnitContext(ctx, ttyUnit, "replace", nil)
				s.conn.ResetFailedUnitContext(ctx, ttyUnit)
			}
		}()

		chTTY := make(chan string, 1)
		if _, err := s.conn.StartTransientUnitContext(ctx, ttyUnit, "replace", ttyHandler, chTTY); err != nil {
			if e := s.conn.ResetFailedUnitContext(ctx, ttyUnit); e == nil {
				_, err2 := s.conn.StartTransientUnitContext(ctx, ttyUnit, "replace", ttyHandler, chTTY)
				if err2 == nil {
					err = nil
				}
			} else {
				log.G(ctx).WithField("unit", ttyUnit).WithError(e).Warn("Error reseting failed unit")
			}
			if err != nil {
				return nil, fmt.Errorf("error starting tty service: %w", err)
			}
		}
		select {
		case <-ctx.Done():
		case status := <-chTTY:
			if status != "done" {
				return nil, fmt.Errorf("failed to start tty service: %s", status)
			}
		}

	}

	if r.Stdin != "" {
		properties = append(properties, systemd.Property{Name: "StandardInputFile", Value: dbus.MakeVariant(r.Stdin)})
	}

	switch opts.LogMode {
	case options.LogMode_STDIO:
		if r.Stdout != "" {
			properties = append(properties, systemd.Property{Name: "StandardOutputFile", Value: dbus.MakeVariant(r.Stdout)})
		}
		if r.Stderr != "" {
			properties = append(properties, systemd.Property{Name: "StandardErrorFile", Value: dbus.MakeVariant(r.Stderr)})
		}
	case options.LogMode_NULL:
		properties = append(properties, systemd.Property{Name: "StandardOutput", Value: dbus.MakeVariant("null")})
		properties = append(properties, systemd.Property{Name: "StandardError", Value: dbus.MakeVariant("null")})
	case options.LogMode_JOURNALD:
	default:
		return nil, fmt.Errorf("%w: invalid log mode: %s", errdefs.ErrInvalidArgument, opts.LogMode)
	}

	defer func() {
		if retErr != nil {
			s.conn.StopUnitContext(ctx, name, "replace", nil)
			s.runc.Delete(ctx, runcName(ns, r.ID), &runc.DeleteOpts{Force: true})
			s.conn.ResetFailedUnitContext(ctx, name)
		}
	}()

	properties = append(properties, systemd.PropExecStart(append(execStart, runcName(ns, r.ID)), false))

	chMain := make(chan string, 1)
	_, err = s.conn.StartTransientUnitContext(ctx, name, "replace", properties, chMain)
	if err != nil {
		if e := s.conn.ResetFailedUnitContext(ctx, name); e == nil {
			_, err2 := s.conn.StartTransientUnitContext(ctx, name, "replace", properties, chMain)
			if err2 == nil {
				err = nil
			}
		} else {
			log.G(ctx).WithField("unit", name).WithError(e).Warn("Error reseting failed unit")
		}
		if err != nil {
			return nil, fmt.Errorf("error starting systemd runc unit: %w", err)
		}
	}

	select {
	case <-ctx.Done():
	case status := <-chMain:
		if status != "done" {
			return nil, fmt.Errorf("failed to start runc init: %s", status)
		}
	}

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}

	pid := p.Value.Value().(uint32)

	s.send(ns, &eventsapi.TaskCreate{
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

// Start the primary user process inside the container
func (s *Service) Start(ctx context.Context, r *taskapi.StartRequest) (_ *taskapi.StartResponse, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = errors.Wrap(retErr, "start")
		}
	}()

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	name := unitName(ns, r.ID, "service")

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Start end")
	}()

	if err := s.runc.Start(ctx, runcName(ns, r.ID)); err != nil {
		return nil, err
	}

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}

	pid := p.Value.Value().(uint32)
	s.send(ns, &eventsapi.TaskStart{
		ContainerID: r.ID,
		Pid:         pid,
	})
	return &taskapi.StartResponse{Pid: pid}, nil
}

func (s *Service) Close() {
	s.conn.Unsubscribe()
	s.conn.Close()
	close(s.events)
	<-s.waitEvents
}

// Delete a process or container
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (_ *taskapi.DeleteResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns))

	log.G(ctx).Info("systemd.Delete begin")
	name := unitName(ns, r.ID, "service")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Delete end")
	}()

	defer func() {
		mount.UnmountAll(filepath.Join(s.root, r.ID, "rootfs"), 0)
		if err := os.RemoveAll(filepath.Join(s.root, ns, r.ID)); err != nil {
			log.G(ctx).WithError(err).Error("Error removing container root directory")
		}
	}()
	if err := s.runc.Delete(ctx, runcName(ns, r.ID), &runc.DeleteOpts{Force: true}); err != nil {
		return nil, err
	}

	var st execState
	if err := s.getState(ctx, name, &st); err != nil && !errdefs.IsNotFound(err) {
		log.G(ctx).WithError(err).Error("Error getting unit state")
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("statusCode", st.ExitCode))

	if st.ExitCode != 0 {
		if err := s.conn.ResetFailedUnitContext(ctx, name); err != nil {
			log.G(ctx).WithError(err).Debug("Error reseting failed unit")
		}
		s.conn.ResetFailedUnitContext(ctx, unitName(ns, r.ID+"-tty", "service"))
	}

	return &taskapi.DeleteResponse{
		Pid:        st.Pid,
		ExitStatus: st.ExitCode,
		ExitedAt:   st.ExitedAt,
	}, nil
}

// Exec an additional process inside the container
func (s *Service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	log.G(ctx).Info("systemd.State")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.State end")
	}()

	st, err := s.runc.State(ctx, runcName(ns, r.ID))
	if err != nil {
		return nil, err
	}

	status := toStatus(st.Status)
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("status", status))

	resp := &taskapi.StateResponse{
		ID:     st.ID,
		Pid:    uint32(st.Pid),
		Status: status,
		Bundle: st.Bundle,
	}

	if status == task.StatusStopped {
		var sdSt execState
		if err := s.getState(ctx, unitName(ns, r.ID, "service"), &sdSt); err != nil {
			return nil, err
		}

		resp.ExitStatus = uint32(sdSt.ExitCode)
		resp.ExitedAt = sdSt.ExitedAt
		ctx = log.WithLogger(ctx, log.G(ctx).WithField("exitStatus", sdSt.ExitCode))
	}

	return resp, nil
}

// Pause the container
func (s *Service) Pause(ctx context.Context, r *taskapi.PauseRequest) (*ptypes.Empty, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	err = s.runc.Pause(ctx, runcName(ns, r.ID))
	if err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (*ptypes.Empty, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	if err := s.runc.Resume(ctx, runcName(ns, r.ID)); err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Kill a process
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (*ptypes.Empty, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	s.conn.KillUnitContext(ctx, unitName(ns, r.ID, ""), int32(r.Signal))
	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *Service) Pids(ctx context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	ls, err := s.runc.Ps(ctx, runcName(ns, r.ID))
	if err != nil {
		return nil, err
	}

	procs := make([]*task.ProcessInfo, 0, len(ls))

	for _, p := range ls {
		procs = append(procs, &task.ProcessInfo{Pid: uint32(p)})
	}

	return &taskapi.PidsResponse{
		Processes: procs,
	}, nil
}

// Checkpoint the container
func (s *Service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Connect returns shim information of the underlying Service
func (s *Service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	var st execState
	if err := s.getState(ctx, unitName(ns, r.ID, "service"), &st); err != nil {
		return nil, err
	}
	return &taskapi.ConnectResponse{TaskPid: st.Pid, ShimPid: uint32(os.Getpid())}, nil
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the Service can be stopped
func (s *Service) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*ptypes.Empty, error) {
	// We ignore this call because we don't actually want containerd to shut us down since systemd manages our lifecycle.
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *Service) Stats(ctx context.Context, r *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	// TODO: caching?

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	var st execState
	if err := s.getState(ctx, unitName(ns, r.ID, "service"), &st); err != nil {
		return nil, err
	}

	var stats interface{}
	if cgroups.Mode() == cgroups.Unified {
		g, err := cgroupsv2.PidGroupPath(int(st.Pid))
		if err != nil {
			return nil, err
		}
		cg, err := cgroupsv2.LoadManager("/sys/fs/cgroup", g)
		if err != nil {
			return nil, err
		}
		m, err := cg.Stat()
		if err != nil {
			return nil, err
		}
		stats = m
	} else {
		cg, err := cgroups.Load(cgroups.V1, cgroups.PidPath(int(st.Pid)))
		if err != nil {
			return nil, err
		}
		m, err := cg.Stat(cgroups.IgnoreNotExist)
		if err != nil {
			return nil, err
		}
		stats = m
	}

	data, err := typeurl.MarshalAny(stats)
	if err != nil {
		return nil, err
	}
	return &taskapi.StatsResponse{
		Stats: data,
	}, nil
}

// Update the live container
func (s *Service) Update(ctx context.Context, r *taskapi.UpdateTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}
