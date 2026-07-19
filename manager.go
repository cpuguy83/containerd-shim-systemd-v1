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

	"github.com/containerd/cgroups/v3"
	cgroupsv1 "github.com/containerd/cgroups/v3/cgroup1"
	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"
)

const shimName = "io.containerd.systemd.v1"

const systemUnitDir = "/run/systemd/system"

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
		conn.Close()
		return nil, fmt.Errorf("error looking up runc path: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	s, err := newServiceWithConfig(ctx, serviceConfig{
		Config:  cfg,
		conn:    conn,
		runcBin: runcPath,
		exe:     exe,
		unitDir: systemUnitDir,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

type serviceConfig struct {
	Config
	conn    *systemd.Conn
	runcBin string
	exe     string
	unitDir string
}

func newServiceWithConfig(ctx context.Context, cfg serviceConfig) (*Service, error) {
	runcRoot := filepath.Join(cfg.Root, "runc")
	if err := os.MkdirAll(runcRoot, 0710); err != nil {
		return nil, err
	}

	log.L = log.G(ctx).WithFields(logrus.Fields{
		"root":      cfg.Root,
		"runc.root": runcRoot,
	})

	debug := logrus.GetLevel() >= logrus.DebugLevel
	return &Service{
		conn:           cfg.conn,
		exe:            cfg.exe,
		root:           cfg.Root,
		unitDir:        cfg.unitDir,
		noNewNamespace: cfg.NoNewNamespace,
		publisher:      cfg.Publisher,
		events:         make(chan eventEnvelope, 128),
		waitEvents:     make(chan struct{}),
		defaultLogMode: cfg.LogMode,
		processes:      &processManager{ls: make(map[string]Process)},
		units:          newUnitManager(),
		runcBin:        cfg.runcBin,
		debug:          debug,
	}, nil
}

type Service struct {
	conn           *systemd.Conn
	runcBin        string
	debug          bool
	root           string
	unitDir        string
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
func (s *Service) Pause(ctx context.Context, r *taskapi.PauseRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	ctx, span := StartSpan(ctx, "service.Pause", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("pause: %w", retErr))
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
	return &emptypb.Empty{}, nil
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Resume", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("resume: %w", retErr))
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
	return &emptypb.Empty{}, nil
}

// Kill a process
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (_ *emptypb.Empty, retErr error) {
	log.G(ctx).Debug("KILL")
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"namespace":    ns,
		"container_id": r.ID,
		"exec_id":      r.ExecID,
	}))

	ctx, span := StartSpan(ctx, "service.Kill")
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("kill: %w", retErr))
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
	return &emptypb.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *Service) Pids(ctx context.Context, r *taskapi.PidsRequest) (_ *taskapi.PidsResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Pids", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("pids: %w", retErr))
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
func (s *Service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Checkpoint")
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("checkpoint: %w", retErr))
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
	return &emptypb.Empty{}, nil
}

// Connect returns shim information of the underlying Service
func (s *Service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (_ *taskapi.ConnectResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Connect", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("connect: %w", retErr))
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
func (s *Service) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	// We ignore this call because we don't actually want containerd to shut us down since systemd manages our lifecycle.
	return &emptypb.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *Service) Stats(ctx context.Context, r *taskapi.StatsRequest) (_ *taskapi.StatsResponse, retErr error) {
	// TODO: caching?

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Stats", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("stats: %w", retErr))
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
		cg, err := cgroupsv2.Load(g)
		if err != nil {
			return nil, err
		}
		m, err := cg.Stat()
		if err != nil {
			return nil, err
		}
		stats = m
	} else {
		cg, err := cgroupsv1.Load(cgroupsv1.PidPath(int(pid)))
		if err != nil {
			return nil, err
		}
		m, err := cg.Stat(cgroupsv1.IgnoreNotExist)
		if err != nil {
			return nil, err
		}
		stats = m
	}

	data, err := typeurl.MarshalAnyToProto(stats)
	if err != nil {
		return nil, err
	}
	return &taskapi.StatsResponse{
		Stats: data,
	}, nil
}

// Update the live container
func (s *Service) Update(ctx context.Context, r *taskapi.UpdateTaskRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Update")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
			retErr = errgrpc.ToGRPC(fmt.Errorf("update: %w", retErr))
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
	return &emptypb.Empty{}, nil
}
