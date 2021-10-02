package systemdshim

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/containerd/console"
	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/pipes"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// We always use the same grouping so there is a single shim for all containers
const grouping = "systemd-shim"

var (
	timeZero = time.UnixMicro(0)
)

func New(ctx context.Context, ns, root string, publisher events.Publisher) (*Service, error) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, err
	}

	return &Service{
		conn:       conn,
		root:       root,
		ns:         ns,
		publisher:  publisher,
		events:     make(chan interface{}, 128),
		waitEvents: make(chan struct{}),
		runc: &runc.Runc{
			// Root:          filepath.Join(root, "runc"),
			SystemdCgroup: true,
			PdeathSignal:  syscall.SIGKILL,
		}}, nil
}

type Service struct {
	conn       *systemd.Conn
	runc       *runc.Runc
	root       string
	ns         string
	publisher  events.Publisher
	events     chan interface{}
	waitEvents chan struct{}

	mu       sync.Mutex
	consoles map[string]*cio
}

type cio struct {
	m      console.Console
	f      io.Closer
	stdin  io.ReadCloser
	stdout io.WriteCloser
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
	name := unitName(s.ns, r.ID, "service")
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Create started")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Create end")
	}()
	ch := make(chan string, 1)
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	// See https://github.com/opencontainers/runc/issues/3202
	if err := os.MkdirAll("/run/runc", 0700); err != nil {
		return nil, err
	}

	pidFile := filepath.Join(s.root, r.ID, "pid")

	execStart := []string{runcPath, "create", "--bundle=" + r.Bundle, "--pid-file", pidFile}

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

	properties := []systemd.Property{
		systemd.PropType("forking"),
		{Name: "PIDFile", Value: dbus.MakeVariant(pidFile)},
	}

	if r.Stdin != "" {
		properties = append(properties, systemd.Property{Name: "StandardInputFile", Value: dbus.MakeVariant(r.Stdin)})
	}
	if r.Stdout != "" {
		properties = append(properties, systemd.Property{Name: "StandardOutputFile", Value: dbus.MakeVariant(r.Stdout)})
	}
	if r.Stderr != "" {
		properties = append(properties, systemd.Property{Name: "StandardErrorFile", Value: dbus.MakeVariant(r.Stderr)})
	}
	if r.Terminal {
		socket, err := runc.NewTempConsoleSocket()
		if err != nil {
			return nil, fmt.Errorf("error creating console socket: %w", err)
		}
		var (
			stdin  io.ReadCloser
			stdout io.WriteCloser
		)
		if r.Stdin != "" {
			r, _, err := pipes.OpenFifo(r.Stdin, os.O_RDWR, 0)
			if err != nil {
				socket.Close()
				return nil, fmt.Errorf("error opening stdin pipe: %w", err)
			}
			stdin = r
		}
		if r.Stdout != "" {
			_, w, err := pipes.OpenFifo(r.Stdout, os.O_RDWR, 0)
			if err != nil {
				socket.Close()
				return nil, fmt.Errorf("error opening stdout pipe: %w", err)
			}
			stdout = w
		}
		s.mu.Lock()
		if s.consoles == nil {
			s.consoles = make(map[string]*cio)
		}
		c := &cio{stdin: stdin, stdout: stdout}
		s.consoles[r.ID] = c
		s.mu.Unlock()
		go func(console *runc.Socket, stdin io.ReadCloser, stdout io.WriteCloser) {
			defer func() {
				stdin.Close()
				stdout.Close()
				s.Close()
			}()

			m, err := socket.ReceiveMaster()
			if err != nil {
				log.G(ctx).WithError(err).Error("Error setting up console master")
				return
			}

			s.mu.Lock()
			c.m = m
			s.mu.Unlock()
			defer m.Close()

			var f io.ReadWriteCloser = m

			nfd, err := unix.Dup(int(m.Fd()))
			if err != nil {
				log.G(ctx).WithError(err).Error("Error duping console master")
			} else {
				f = os.NewFile(uintptr(nfd), r.ID+"_console")
				s.mu.Lock()
				c.f = f
				s.mu.Unlock()
			}

			defer f.Close()

			var wg sync.WaitGroup
			if stdin != nil {
				wg.Add(1)
				go func() {
					io.Copy(f, stdin)
					wg.Done()
				}()
			}
			if stdout != nil {
				wg.Add(1)
				go func() {
					io.Copy(stdout, f)
					wg.Done()
				}()
			}

			wg.Wait()
		}(socket, stdin, stdout)
		execStart = append(execStart, "--console-socket", socket.Path())
	}

	defer func() {
		if retErr != nil {
			s.runc.Delete(ctx, runcName(s.ns, r.ID), &runc.DeleteOpts{Force: true})
			s.conn.ResetFailedUnitContext(ctx, name)
		}
	}()

	properties = append(properties, systemd.PropExecStart(append(execStart, runcName(s.ns, r.ID)), false))

	_, err = s.conn.StartTransientUnitContext(ctx, name, "replace", properties, ch)
	if err != nil {
		if e := s.conn.ResetFailedUnitContext(ctx, name); e == nil {
			_, err2 := s.conn.StartTransientUnitContext(ctx, name, "replace", properties, ch)
			if err2 == nil {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}

	select {
	case <-ctx.Done():
	case <-ch:
	}

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}

	pid := p.Value.Value().(uint32)

	s.send(&eventsapi.TaskCreate{
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

	name := unitName(s.ns, r.ID, "service")

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Start end")
	}()

	if err := s.runc.Start(ctx, runcName(s.ns, r.ID)); err != nil {
		return nil, err
	}

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}

	pid := p.Value.Value().(uint32)
	s.send(&eventsapi.TaskStart{
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
	log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns))

	log.G(ctx).Info("systemd.Delete begin")
	name := unitName(s.ns, r.ID, "service")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Delete end")
	}()

	defer func() {
		mount.UnmountAll(filepath.Join(s.root, r.ID, "rootfs"), 0)
	}()
	if err := s.runc.Delete(ctx, runcName(s.ns, r.ID), &runc.DeleteOpts{Force: true}); err != nil {
		return nil, err
	}

	var st execState
	if err := s.getState(ctx, name, &st); err != nil && !errdefs.IsNotFound(err) {
		log.G(ctx).WithError(err).Error("Error getting unit state")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("statusCode", st.ExitCode))

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

// ResizePty of a process
func (s *Service) ResizePty(ctx context.Context, r *taskapi.ResizePtyRequest) (*ptypes.Empty, error) {
	s.mu.Lock()
	c, ok := s.consoles[r.ID]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	if c.m == nil {
		return &ptypes.Empty{}, nil
	}
	s.mu.Unlock()

	if err := c.m.Resize(console.WinSize{Width: uint16(r.Width), Height: uint16(r.Height)}); err != nil {
		return nil, err
	}

	return &ptypes.Empty{}, nil
}

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	log.G(ctx).Info("systemd.State")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.State end")
	}()

	st, err := s.runc.State(ctx, runcName(s.ns, r.ID))
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
		if err := s.getState(ctx, unitName(s.ns, r.ID, "service"), &sdSt); err != nil {
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
	err := s.runc.Pause(ctx, runcName(s.ns, r.ID))
	if err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (*ptypes.Empty, error) {
	if err := s.runc.Resume(ctx, runcName(s.ns, r.ID)); err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Kill a process
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (*ptypes.Empty, error) {
	s.conn.KillUnitContext(ctx, unitName(s.ns, r.ID, ""), int32(r.Signal))
	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *Service) Pids(ctx context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	ls, err := s.runc.Ps(ctx, runcName(s.ns, r.ID))
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

// CloseIO of a process
func (s *Service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (_ *ptypes.Empty, retErr error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID))
	log.G(ctx).Info("CloseIO start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("CloseIO end")
	}()

	s.mu.Lock()
	c, ok := s.consoles[r.ID]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	delete(s.consoles, r.ID)
	s.mu.Unlock()

	if r.Stdin {
		c.stdin.Close()
	}
	c.stdout.Close()
	if c.m != nil {
		c.m.Close()
	}
	if c.f != nil {
		c.f.Close()
	}
	return &ptypes.Empty{}, nil
}

// Checkpoint the container
func (s *Service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Connect returns shim information of the underlying Service
func (s *Service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	var st execState
	if err := s.getState(ctx, unitName(s.ns, r.ID, "service"), &st); err != nil {
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

	var st execState
	if err := s.getState(ctx, unitName(s.ns, r.ID, "service"), &st); err != nil {
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
