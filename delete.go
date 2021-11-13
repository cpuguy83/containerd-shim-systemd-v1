package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/coreos/go-systemd/v22/dbus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Delete a process or container
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (_ *taskapi.DeleteResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Delete", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "delete")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("execID", r.ExecID))

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s", r.ID)
	}

	var st pState
	if r.ExecID != "" {
		pInit := p.(*initProcess)
		ep := pInit.execs.Get(r.ExecID)
		if ep == nil {
			return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "exec %s", r.ExecID)
		}
		st, err = ep.Delete(ctx)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		pInit.execs.Delete(r.ExecID)
		s.units.Delete(ep)
	} else {
		st, err = p.Delete(ctx)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		s.processes.Delete(path.Join(ns, r.ID))
		s.units.Delete(p)
	}

	return &taskapi.DeleteResponse{
		Pid:        st.Pid,
		ExitStatus: st.ExitCode,
		ExitedAt:   st.ExitedAt,
	}, nil
}

func (p *initProcess) Delete(ctx context.Context) (retState pState, retErr error) {
	ctx, span := StartSpan(ctx, "InitProcess.Delete")
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("delete: %w", retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(
			attribute.Int("pid", int(retState.Pid)),
			attribute.Int("exitCode", int(retState.ExitCode)),
			attribute.String("unitStatus", retState.Status),
			attribute.Stringer("exitedAt", retState.ExitedAt),
		)
		span.End()
	}()

	if !p.ProcessState().ExitedAt.After(timeZero) {
		return pState{}, fmt.Errorf("container has not exited: %w", errdefs.ErrFailedPrecondition)
	}

	defer func() {
		if err := mount.UnmountAll(filepath.Join(p.Bundle, "rootfs"), 0); err != nil {
			log.G(ctx).WithError(err).Error("failed to cleanup rootfs mount")
		}
		if err := os.RemoveAll(p.root); err != nil {
			log.G(ctx).WithError(err).Error("Error removing container root directory")
		}
	}()

	if err := p.runc.Delete(ctx, runcName(p.ns, p.id), &runc.DeleteOpts{Force: true}); err != nil {
		return pState{}, err
	}

	var ps pState
	if p.Pid() > 0 {
		var err error
		ps, err = p.waitForExit(ctx)
		if err != nil {
			return pState{}, err
		}
	}

	if p.Terminal {
		p.systemd.KillUnitContext(ctx, unitName(p.ns, p.id+"-tty"), 9)
	}

	p.mu.Lock()
	p.deleted = true
	p.cond.Broadcast()
	p.mu.Unlock()

	return ps, nil
}

// TODO: It seems like the runc shim deletes the init process in this case
// Here we are cleaning up the exec process, which is different, but seems more correct...
// That said this may cause some unexpected behavior as related to the runc shim.
func (p *execProcess) Delete(ctx context.Context) (retState pState, retErr error) {
	ctx, span := StartSpan(ctx, "ExecProcess.Delete")
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("delete: %w", retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(
			attribute.Int("pid", int(retState.Pid)),
			attribute.Int("exitCode", int(retState.ExitCode)),
			attribute.String("unitStatus", retState.Status),
			attribute.Stringer("exitedAt", retState.ExitedAt),
		)
		span.End()
	}()

	if !p.ProcessState().Exited() && !p.parent.ProcessState().Exited() {
		return pState{}, fmt.Errorf("exec has not exited: %w", errdefs.ErrFailedPrecondition)
	}

	p.systemd.KillUnitWithTarget(ctx, p.Name(), dbus.Main, 9)

	if p.Terminal {
		ttyName := unitName(p.ns, p.id+"-tty")
		p.systemd.KillUnitWithTarget(ctx, ttyName, dbus.Main, 9)
	}

	p.mu.Lock()
	p.deleted = true
	p.cond.Broadcast()
	p.mu.Unlock()

	p.parent.execs.Delete(p.execID)

	return p.ProcessState(), nil
}
