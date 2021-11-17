package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
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
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/golang/protobuf/proto"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

	if opts.LogMode == "" {
		opts.LogMode = s.defaultLogMode.String()
	}

	root := filepath.Join(s.root, ns, r.ID)
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
				SystemdCgroup: false,
				PdeathSignal:  syscall.SIGKILL,
				Root:          filepath.Join(s.root, "runc"),
			},
			exe:  s.exe,
			root: root,
		},
		Bundle:           r.Bundle,
		Rootfs:           r.Rootfs,
		checkpoint:       r.Checkpoint,
		parentCheckpoint: r.ParentCheckpoint,
		sendEvent:        s.send,
		execs: &processManager{
			ls: make(map[string]Process),
		},
	}
	log.G(ctx).Debugf("%+v", p)
	p.process.cond = sync.NewCond(&p.process.mu)

	if err := s.processes.Add(path.Join(ns, r.ID), p); err != nil {
		return nil, err
	}
	s.units.Add(p)

	defer func() {
		if retErr != nil {
			p.SetState(ctx, pState{ExitCode: 139, ExitedAt: time.Now(), Status: "failed"})
			s.processes.Delete(path.Join(ns, r.ID))
			s.units.Delete(p)
			if _, err := p.Delete(ctx); err != nil {
				log.G(ctx).WithError(err).Error("error cleaning up failed process")
			}
		}
	}()

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("error creating state dir: %w", err)
	}

	defer func() {
		if retErr != nil {
			os.RemoveAll(root)
		}
	}()

	pid, err := p.Create(ctx)
	if err != nil {
		return nil, err
	}

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
	pInit := p.(*initProcess)

	// TODO: In order to support shim restarts we need to persist this.
	ep := &execProcess{
		Spec:   r.Spec,
		parent: pInit,
		execID: r.ExecID,
		process: &process{
			ns:       ns,
			root:     pInit.root,
			id:       r.ID + "-" + r.ExecID,
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
			systemd:  s.conn,
			runc:     pInit.runc,
			exe:      s.exe,
			opts:     CreateOptions{LogMode: s.defaultLogMode.String()},
		}}
	ep.process.cond = sync.NewCond(&ep.process.mu)
	err = pInit.execs.Add(r.ExecID, ep)
	if err != nil {
		return nil, fmt.Errorf("process %s: %w", r.ExecID, err)
	}
	s.units.Add(ep)

	s.send(ctx, ns, &eventsapi.TaskExecAdded{
		ContainerID: pInit.id,
		ExecID:      r.ExecID,
	})
	return &ptypes.Empty{}, nil
}

func (p *process) startUnit(ctx context.Context, prefixCmd, cmd []string, pidFile, id string, extraEnvs []string) (_ uint32, retErr error) {
	ctx, span := StartSpan(ctx, "process.StartUnit")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	execStart, err := p.runcCmd(cmd)
	if err != nil {
		return 0, err
	}
	if len(prefixCmd) > 0 {
		execStart = append(prefixCmd, execStart...)
	}
	var properties []systemd.Property
	if p.opts.SdNotifyEnable {
		properties = []systemd.Property{systemd.PropType("notify")}
	} else {
		if pidFile != "" {
			execStart = append(execStart, "--pid-file="+pidFile)
			properties = []systemd.Property{
				systemd.PropType("forking"),
				{Name: "PIDFile", Value: dbus.MakeVariant(pidFile)},
			}
		}
	}

	properties = append(properties, systemd.Property{Name: "Delegate", Value: dbus.MakeVariant(true)})
	if len(extraEnvs) > 0 {
		properties = append(properties, systemd.Property{Name: "Environment", Value: dbus.MakeVariant(extraEnvs)})
	}

	if p.Terminal || p.opts.Terminal {
		// TODO: We need to bind the pty copier to the container's lifecycle
		// This would ensure that once the container exits, the pty copier exits
		ttyUnit, sockPath, err := p.makePty(ctx)
		if err != nil {
			return 0, fmt.Errorf("error setting up tty handler: %w", err)
		}
		defer func() {
			if retErr != nil {
				if _, err := p.systemd.StopUnitContext(ctx, ttyUnit, "replace", nil); err != nil {
					log.G(ctx).WithError(err).WithField("unit", ttyUnit).Debug("failed to stop tty unit unit")
				}
				if err := p.systemd.ResetFailedUnitContext(ctx, ttyUnit); err != nil {
					log.G(ctx).WithError(err).WithField("unit", ttyUnit).Debug("Error reseting tty unit")
				}
			}
		}()

		// Add the console socket option to runc's exec-start
		execStart = append(execStart, "--console-socket", sockPath)
	}

	if p.Stdin != "" {
		properties = append(properties, systemd.Property{Name: "StandardInputFile", Value: dbus.MakeVariant(p.Stdin)})
	}

	// TODO: journald+tty?
	switch options.LogMode(options.LogMode_value[p.opts.LogMode]) {
	case options.LogMode_STDIO:
		if p.Stdout != "" {
			properties = append(properties, systemd.Property{Name: "StandardOutputFile", Value: dbus.MakeVariant(p.Stdout)})
		}
		if p.Stderr != "" {
			properties = append(properties, systemd.Property{Name: "StandardErrorFile", Value: dbus.MakeVariant(p.Stderr)})
		}
	case options.LogMode_NULL:
		properties = append(properties, systemd.Property{Name: "StandardOutput", Value: dbus.MakeVariant("null")})
		properties = append(properties, systemd.Property{Name: "StandardError", Value: dbus.MakeVariant("null")})
	case options.LogMode_JOURNALD:
	default:
		return 0, fmt.Errorf("%w: invalid log mode: %s", errdefs.ErrInvalidArgument, p.opts.LogMode)
	}

	name := p.Name()
	defer func() {
		if retErr != nil {
			p.systemd.StopUnitContext(ctx, name, "replace", nil)
			p.systemd.ResetFailedUnitContext(ctx, name)
		}
	}()

	properties = append(properties, systemd.PropExecStart(append(execStart, id), false))

	chMain := make(chan string, 1)
	_, err = p.systemd.StartTransientUnitContext(ctx, name, "replace", properties, chMain)
	if err != nil {
		if e := p.systemd.ResetFailedUnitContext(ctx, name); e == nil {
			chMain = make(chan string, 1)
			_, err2 := p.systemd.StartTransientUnitContext(ctx, name, "replace", properties, chMain)
			if err2 == nil {
				err = nil
			}
		} else {
			log.G(ctx).WithField("unit", name).WithError(e).Warn("Error reseting failed unit")
		}
		if err != nil {
			return 0, fmt.Errorf("error starting systemd runc unit: %w", err)
		}
	}

	select {
	case <-ctx.Done():
	case status := <-chMain:
		if status != "done" {
			var ps pState
			if err := getUnitState(ctx, p.systemd, name, &ps); err != nil {
				log.G(ctx).WithError(err).Warn("Error getting unit state")
			} else {
				ps = p.SetState(ctx, ps)
			}
			ret := fmt.Errorf("failed to start runc init: %s", status)
			if p.runc.Debug {
				ret = fmt.Errorf("%w: %v", ret, execStart)
				debug, err := os.ReadFile(filepath.Join(p.root, p.id+"-runc-debug.log"))
				if err == nil {
					ret = fmt.Errorf("%w:\n%s", ret, string(debug))
				} else {
					log.G(ctx).WithError(err).Warn("Error opening runc debug log")
				}
			}
			return 0, ret
		}
	}

	pid, err := p.systemd.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return 0, err
	}

	ps := pState{Pid: pid.Value.Value().(uint32)}
	p.SetState(ctx, ps)
	return ps.Pid, nil
}

// For init processes we start a unit immediately.
// runc will hold a process open in the background and wait for the caller to setup namespaces and so on.
// Then once that is complete the caller will call "start", which we will just call `runc start`.
//
// TODO: checkpoint support
func (p *initProcess) Create(ctx context.Context) (_ uint32, retErr error) {
	ctx, span := StartSpan(ctx, "InitProcess.Create")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
			p.runc.Delete(ctx, runcName(p.ns, p.id), &runc.DeleteOpts{Force: true})
		}
		span.End()
	}()

	if p.checkpoint != "" {
		if p.opts.CriuWorkPath == "" {
			p.opts.CriuWorkPath = filepath.Join(p.root, "criu-work")
		}
		// We seem to be missing Terminal info when doing a restore, so get that from the spec.
		data, err := os.ReadFile(filepath.Join(p.Bundle, "config.json"))
		if err != nil {
			return 0, fmt.Errorf("could not read config.json: %w", err)
		}
		var spec specs.Spec
		if err := json.Unmarshal(data, &spec); err != nil {
			return 0, fmt.Errorf("error unmarshalling config.json")
		}
		p.Terminal = spec.Process.Terminal
		return 0, nil
	}

	execStart := []string{
		"create",
		"--bundle=" + p.Bundle,
		"--no-pivot=" + strconv.FormatBool(p.opts.NoPivotRoot),
		"--no-new-keyring=" + strconv.FormatBool(p.opts.NoNewKeyring),
	}

	f, err := ioutil.TempFile(os.Getenv("XDG_RUNTIME_DIR"), p.id+"-task-config")
	if err != nil {
		return 0, fmt.Errorf("error creating task config temp file: %w", err)
	}
	defer func() {
		f.Close()
		os.RemoveAll(f.Name())
	}()

	req := taskapi.CreateTaskRequest{Bundle: p.Bundle, Rootfs: p.Rootfs}
	data, err := proto.Marshal(&req)
	if err != nil {
		return 0, fmt.Errorf("error marshaling task create config")
	}

	if _, err := f.Write(data); err != nil {
		return 0, fmt.Errorf("error writing task create config to file")
	}

	pidFile := filepath.Join(p.root, "pid")
	return p.startUnit(ctx, []string{p.exe, "create"}, execStart, pidFile, runcName(p.ns, p.id), []string{"CREATE_TASK_CONFIG=" + f.Name()})
}
