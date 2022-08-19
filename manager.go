package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const shimName = "io.containerd.systemd.v1"

var (
	timeZero = time.UnixMicro(0)
)

type Config struct {
	Root           string
	Publisher      events.Publisher
	LogMode        options.LogMode
	NoNewNamespace bool
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
		noNewNamespace: cfg.NoNewNamespace,
		publisher:      cfg.Publisher,
		events:         make(chan eventEnvelope, 128),
		waitEvents:     make(chan struct{}),
		defaultLogMode: cfg.LogMode,
		processes:      &processManager{ls: make(map[string]Process)},
		units:          newUnitManager(conn),
		runcBin:        runcPath,
		debug:          debug,
	}, nil
}

type Service struct {
	conn           *systemd.Conn
	runcBin        string
	debug          bool
	root           string
	noNewNamespace bool
	publisher      events.Publisher
	events         chan eventEnvelope
	waitEvents     chan struct{}

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

func unitName(ns, id, mod string) string {
	n := "io-containerd-systemd-" + ns + "-" + id
	if mod != "" {
		n += "-" + mod
	}
	return n + ".service"
}

func (s *Service) Close() {
	s.conn.Unsubscribe()
	s.conn.Close()
	close(s.events)
	<-s.waitEvents
}

// Pause the container
func (s *Service) Pause(ctx context.Context, r *taskapi.PauseRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	ctx, span := StartSpan(ctx, "service.Pause", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "pause")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: %s", errdefs.ErrNotFound, r.ID)
	}
	ctx = WithShimLog(ctx, p.LogWriter())

	err = p.(*initProcess).Pause(ctx)
	if err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Resume", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "resume")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: %s", errdefs.ErrNotFound, r.ID)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	if err := p.(*initProcess).Resume(ctx); err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Kill a process
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (_ *ptypes.Empty, retErr error) {
	log.G(ctx).Debug("KILL")
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"namespace":    ns,
		"container_id": r.ID,
		"exec_id":      r.ExecID,
	}))

	ctx, span := StartSpan(ctx, "service.Kill")
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "kill")
			span.SetStatus(codes.Error, retErr.Error())
			log.G(ctx).WithError(retErr).Error("kill failed")
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, fmt.Errorf("exec process %s: %w", r.ExecID, errdefs.ErrNotFound)
		}
		if err := ep.Kill(ctx, int(r.Signal), r.All); err != nil {
			return nil, err
		}
	} else {
		if err := p.Kill(ctx, int(r.Signal), r.All); err != nil {
			return nil, err
		}
	}
	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *Service) Pids(ctx context.Context, r *taskapi.PidsRequest) (_ *taskapi.PidsResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Pids", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "pids")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: %s", errdefs.ErrNotFound, r.ID)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	procs, err := p.(*initProcess).Pids(ctx)
	if err != nil {
		return nil, err
	}

	return &taskapi.PidsResponse{
		Processes: procs,
	}, nil
}

// Checkpoint the container
func (s *Service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Checkpoint")
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "checkpoint")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
		log.G(ctx).WithError(retErr).Debug("Checkpoint")
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	if err := p.(*initProcess).Checkpoint(ctx, r.Options); err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}

// Connect returns shim information of the underlying Service
func (s *Service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (_ *taskapi.ConnectResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Connect", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "connect")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	return &taskapi.ConnectResponse{TaskPid: p.Pid(), ShimPid: uint32(os.Getpid())}, nil
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the Service can be stopped
func (s *Service) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*ptypes.Empty, error) {
	// We ignore this call because we don't actually want containerd to shut us down since systemd manages our lifecycle.
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *Service) Stats(ctx context.Context, r *taskapi.StatsRequest) (_ *taskapi.StatsResponse, retErr error) {
	// TODO: caching?

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Stats", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "Stats")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))

	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

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
func (s *Service) Update(ctx context.Context, r *taskapi.UpdateTaskRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Update")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
			retErr = errdefs.ToGRPCf(retErr, "update")
		}
		span.End()
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	var res specs.LinuxResources
	if err := json.Unmarshal(r.Resources.Value, &res); err != nil {
		return nil, err
	}

	if err := p.(*initProcess).Update(ctx, res); err != nil {
		return nil, err
	}
	return &ptypes.Empty{}, nil
}
