package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/typeurl"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
)

// Create a new container
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns))

	log.G(ctx).Info("systemd.Create started")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Create end")
		if retErr != nil {
			retErr = errdefs.ToGRPC(fmt.Errorf("delete: %w", retErr))
		}
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
			runc:     s.runc,
			exe:      s.exe,
			root:     root,
		},
		Bundle:           r.Bundle,
		Rootfs:           r.Rootfs,
		checkpoint:       r.Checkpoint,
		parentCheckpoint: r.ParentCheckpoint,
		execs: &processManager{
			ls: make(map[string]Process),
		},
	}
	p.process.cond = sync.NewCond(&p.process.mu)

	if err := s.processes.Add(path.Join(ns, r.ID), p); err != nil {
		return nil, err
	}
	s.units.Add(p)

	defer func() {
		if retErr != nil {
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

// Exec an additional process inside the container
func (s *Service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
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
			runc:     s.runc,
			exe:      s.exe,
			opts:     options.CreateOptions{LogMode: s.defaultLogMode},
		}}
	ep.process.cond = sync.NewCond(&ep.process.mu)
	err = pInit.execs.Add(r.ExecID, ep)
	if err != nil {
		return nil, errdefs.ToGRPCf(err, "process %s", r.ExecID)
	}
	s.units.Add(ep)

	s.send(ns, &eventsapi.TaskExecAdded{
		ContainerID: pInit.id,
		ExecID:      r.ExecID,
	})
	return &ptypes.Empty{}, nil
}

func (p *process) startUnit(ctx context.Context, cmd []string, pidFile, id string) (_ uint32, retErr error) {
	execStart, err := p.runcCmd(cmd)
	if err != nil {
		return 0, err
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

	if p.Terminal {
		// TODO: We need to bind the pty copier to the container's lifecycle
		// This would ensure that once the container exits, the pty copier exits

		// Add the console socket option to runc's exec-start
		ttyUnit, sockPath, err := p.makePty(ctx)
		if err != nil {
			return 0, fmt.Errorf("error setting up tty handler: %w", err)
		}
		defer func() {
			if retErr != nil {
				if _, err := p.systemd.StopUnitContext(ctx, ttyUnit, "replace", nil); err != nil {
					log.G(ctx).WithError(err).WithField("unit", ttyUnit).Error("failed to stop tty unit unit")
				}
				if err := p.systemd.ResetFailedUnitContext(ctx, ttyUnit); err != nil {
					log.G(ctx).WithError(err).WithField("unit", ttyUnit).Warn("Error reseting tty unit")
				}
			}
		}()

		execStart = append(execStart, "--console-socket", sockPath)
	}

	if p.Stdin != "" {
		properties = append(properties, systemd.Property{Name: "StandardInputFile", Value: dbus.MakeVariant(p.Stdin)})
	}

	// TODO: journald+tty?
	switch p.opts.LogMode {
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
				log.G(ctx).WithError(err).Warn("Errring getting unit state")
			} else {
				p.SetState(ps)
			}
			return 0, fmt.Errorf("failed to start runc init: %s", status)
		}
	}

	pid, err := p.systemd.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return 0, err
	}

	ps := pState{Pid: pid.Value.Value().(uint32)}
	p.SetState(ps)
	return ps.Pid, nil
}

// For init processes we start a unit immediately.
// runc will hold a process open in the background and wait for the caller to setup namespaces and so on.
// Then once that is complete the caller will call "start", which we will just call `runc start`.
//
// TODO: checkpoint support
func (p *initProcess) Create(ctx context.Context) (_ uint32, retErr error) {
	if len(p.Rootfs) > 0 {
		var mounts []mount.Mount
		for _, m := range p.Rootfs {
			mounts = append(mounts, mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			})
		}

		rootfs := filepath.Join(p.Bundle, "rootfs")
		if err := os.Mkdir(rootfs, 0700); err != nil && !os.IsExist(err) {
			return 0, err
		}
		if err := mount.All(mounts, rootfs); err != nil {
			return 0, err
		}
		defer func() {
			if retErr != nil {
				mount.UnmountAll(rootfs, 0)
			}
		}()
	}

	execStart := []string{"create", "--bundle=" + p.Bundle}

	pidFile := filepath.Join(p.root, "pid")
	return p.startUnit(ctx, execStart, pidFile, runcName(p.ns, p.id))
}