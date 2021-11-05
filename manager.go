package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go4.org/syncutil/singleflight"
)

const shimName = "io.containerd.systemd.v1"

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

	log.L = log.G(ctx).WithFields(logrus.Fields{
		"root":      cfg.Root,
		"runc.root": runcRoot,
	})

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
		processes:      &processManager{ls: make(map[string]Process)},
		units:          &unitManager{idx: make(map[string]Process)},
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

	processes *processManager
	units     *unitManager

	defaultLogMode options.LogMode

	// exe is used to re-exec the shim binary to start up a pty copier
	exe string
}

// Cleanup is a binary call that cleans up any resources used by the shim when the Service crashes
func (s *Service) Cleanup(ctx context.Context) (*taskapi.DeleteResponse, error) {
	return &taskapi.DeleteResponse{}, nil
}

func unitName(ns, id string) string {
	return "containerd-" + ns + "-" + id + ".service"
}

func runcName(ns, id string) string {
	return ns + "-" + id
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

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("execID", r.ExecID))

	log.G(ctx).Info("systemd.Start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Start end")
	}()

	p := s.processes.Get(path.Join(ns, r.ID))

	var pid uint32
	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "exec id %v not found", r.ExecID)
		}
		pid, err = ep.Start(ctx)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		s.send(ns, &eventsapi.TaskExecStarted{
			ContainerID: r.ID,
			ExecID:      r.ExecID,
			Pid:         pid,
		})
	} else {
		pid, err = p.Start(ctx)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		s.send(ns, &eventsapi.TaskStart{
			ContainerID: r.ID,
			Pid:         pid,
		})
	}

	return &taskapi.StartResponse{Pid: pid}, nil
}

func (s *Service) Close() {
	s.conn.Unsubscribe()
	s.conn.Close()
	close(s.events)
	<-s.waitEvents
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

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
	}

	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "exec id %v not found", r.ExecID)
		}
		if err := ep.Kill(ctx, int(r.Signal), r.All); err != nil {
			return nil, errdefs.ToGRPC(err)
		}
	} else {
		if err := p.Kill(ctx, int(r.Signal), r.All); err != nil {
			return nil, errdefs.ToGRPC(err)
		}
	}
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

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
	}

	return &taskapi.ConnectResponse{TaskPid: p.Pid(), ShimPid: uint32(os.Getpid())}, nil
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

	p := s.processes.Get(path.Join(ns, r.ID))

	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
	}

	pid := p.Pid()

	var stats interface{}
	if cgroups.Mode() == cgroups.Unified {
		g, err := cgroupsv2.PidGroupPath(int(pid))
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
		cg, err := cgroups.Load(cgroups.V1, cgroups.PidPath(int(pid)))
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
