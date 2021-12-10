package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
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
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	var st pState
	if r.ExecID != "" {
		pInit := p.(*initProcess)
		ep := pInit.execs.Get(r.ExecID)
		if ep == nil {
			return nil, fmt.Errorf("exec %s: %w", r.ExecID, errdefs.ErrNotFound)
		}
		st, err = ep.Delete(ctx)
		if err != nil {
			return nil, err
		}
		pInit.execs.Delete(r.ExecID)
		s.units.Delete(ep)
	} else {
		st, err = p.Delete(ctx)
		if err != nil {
			return nil, err
		}

		p.(*initProcess).execs.mu.Lock()
		for _, ep := range p.(*initProcess).execs.ls {
			s.units.Delete(ep)
		}
		p.(*initProcess).execs.mu.Unlock()
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
		if cl, ok := p.shimLog.(io.Closer); ok {
			cl.Close()
		}
		if retErr != nil {
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

	if !p.ProcessState().Exited() {
		var st pState
		if err := getUnitState(ctx, p.systemd, p.Name(), &st); err == nil {
			if !st.Exited() {
				return pState{}, fmt.Errorf("container has not exited: %w, %s", errdefs.ErrFailedPrecondition, p.ProcessState())
			}
		}
	}

	defer func() {
		if retErr != nil {
			if err := os.RemoveAll(p.root); err != nil {
				log.G(ctx).WithError(err).Error("Error removing container root directory")
			}
		}
	}()

	ch := make(chan string)
	if _, err := p.systemd.StopUnitContext(ctx, p.Name(), "replace", ch); err != nil {
		log.G(ctx).WithError(err).Info("Failed to stop unit")
	}

	// Try to wait for stop to complete
	// On context or stop failure we'll use SIGKILL instead.
	select {
	case <-ctx.Done():
	case <-ch:
	}

	p.systemd.KillUnitContext(ctx, p.Name(), int32(syscall.SIGKILL))

	if err := p.runc.Delete(ctx, p.id, &runc.DeleteOpts{Force: true}); err != nil {
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
		p.systemd.KillUnitContext(ctx, unitName(p.ns, p.id, "tty"), 9)
	}

	if err := os.Remove("/run/systemd/system/" + p.Name()); err != nil {
		return pState{}, err
	}
	if err := p.systemd.ReloadContext(ctx); err != nil {
		log.G(ctx).WithError(err).Error("systemd reload failed")
	}

	if err := p.systemd.ResetFailedUnitContext(ctx, p.Name()); err != nil {
		// Just a debug message since this is just precautionary and the unit may not even be failed.
		log.G(ctx).WithError(err).Debug("Failed to reset systemd unit")
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

	if p.Pid() != 0 && !p.ProcessState().Exited() && !p.parent.ProcessState().Exited() {
		return pState{}, fmt.Errorf("exec has not exited: %w", errdefs.ErrFailedPrecondition)
	}

	ch := make(chan string)
	if _, err := p.systemd.StopUnitContext(ctx, p.Name(), "replace", ch); err != nil {
		log.G(ctx).WithError(err).Info("Failed to stop unit")
	}

	// Try to wait for stop to complete
	// On context or stop failure we'll use SIGKILL instead.
	select {
	case <-ctx.Done():
	case <-ch:
	}

	p.systemd.KillUnitWithTarget(ctx, p.Name(), dbus.Main, 9)
	if p.Terminal {
		p.systemd.KillUnitWithTarget(ctx, p.ttyUnitName(), dbus.Main, 9)
	}

	var ps pState
	if p.Pid() > 0 {
		var err error
		ps, err = p.waitForExit(ctx)
		if err != nil {
			return pState{}, err
		}
	}
	p.mu.Lock()
	p.deleted = true
	p.cond.Broadcast()
	p.mu.Unlock()

	p.parent.execs.Delete(p.execID)
	if err := os.Remove("/run/systemd/system/" + p.Name()); err != nil {
		log.G(ctx).WithError(err).Debug("Failed to remove exec unit")
	}

	if err := p.systemd.ReloadContext(ctx); err != nil {
		log.G(ctx).WithError(err).Error("systemd reload failed")
	}
	p.systemd.ResetFailedUnitContext(ctx, p.Name())

	return ps, nil
}
