package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/coreos/go-systemd/v22/dbus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Delete a process or container
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (_ *taskapi.DeleteResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Delete", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("delete: %w", retErr))
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

		// Deleting the container force-deletes runc, which kills any surviving
		// exec process. Release each exec unit's reference (and clear failed
		// state) so systemd can collect it, rather than leaking it until shim
		// shutdown; individual exec Delete does the same. Detached from the
		// request context because the units are untracked just below, leaving no
		// way to retry a release the caller's cancellation skipped.
		p.(*initProcess).execs.Each(func(ep Process) {
			if e, ok := ep.(*execProcess); ok {
				// Mark before releasing, so an exec Start racing this teardown
				// either sees the flag and abandons its unit, or created it early
				// enough that the release below covers it. Each exec gets its own
				// budget so one slow release cannot starve the rest.
				e.markDeleting()
				relCtx, cancel := cleanupContext(ctx)
				e.releaseUnit(relCtx, e.Name())
				e.systemd.ResetFailedUnitContext(relCtx, e.Name())
				cancel()
			}
			s.units.Delete(ep)
		})
		s.processes.Delete(path.Join(ns, r.ID))
		s.units.Delete(p)
	}

	return &taskapi.DeleteResponse{
		Pid:        st.Pid,
		ExitStatus: st.ExitCode,
		ExitedAt:   timestamppb.New(st.ExitedAt),
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
		if st, err := loadExitFromUnit(ctx, p.systemd, p.Name()); err == nil {
			if !st.Exited() {
				return pState{}, fmt.Errorf("container has not exited: %w, %s", errdefs.ErrFailedPrecondition, p.ProcessState())
			}
			// The unit exited but the shim never observed the signal -- the case the
			// unit reference keeps recoverable. Record it: waitForExit below reads
			// the in-memory state, so dropping this exit would leave Delete blocked
			// on an exit that already happened and never publish its task exit.
			p.SetState(ctx, st)
		}
	}

	// Signal teardown before any of it runs, so a Start racing this delete can
	// see it and not leave a unit behind (see markDeleting).
	p.markDeleting()

	// Past the precondition check we are committed to tearing the unit down, so
	// release its reference on every exit path -- including the runc.Delete and
	// waitForExit failures below -- otherwise systemd keeps the unit pinned until
	// shim shutdown. Detached from the request context: a successful Delete is
	// immediately followed by the caller untracking this process, so a release
	// skipped because the caller canceled could never be retried.
	defer func() {
		relCtx, cancel := cleanupContext(ctx)
		defer cancel()
		p.releaseUnit(relCtx, p.Name())
	}()

	defer func() {
		if retErr != nil {
			if err := os.RemoveAll(p.root); err != nil {
				log.G(ctx).WithError(err).Error("Error removing container root directory")
			}
		}
	}()

	ch := make(chan string, 1)
	if _, err := p.systemd.StopUnitContext(ctx, p.Name(), "replace", ch); err != nil {
		log.G(ctx).WithError(err).Info("Failed to stop unit")
	} else {
		// Try to wait for stop to complete. On context failure we'll use SIGKILL instead.
		select {
		case <-ctx.Done():
		case <-ch:
		}
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

	// Reset any failed state so systemd can collect the transient unit once its
	// reference is released (see the deferred releaseUnit above).
	if err := p.systemd.ResetFailedUnitContext(ctx, p.Name()); err != nil && !strings.Contains(err.Error(), "not loaded") {
		// Just a debug message since this is just precautionary and the unit may not even be failed.
		log.G(ctx).WithError(err).Debug("Failed to reset systemd unit")
	}
	p.mu.Lock()
	p.deleted = true
	p.cond.Broadcast()
	p.mu.Unlock()

	p.closeTTYControl()

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

	// Signal teardown before any of it runs, so a Start racing this delete can
	// see it and not leave a unit behind (see markDeleting).
	p.markDeleting()

	// Past the precondition check we are committed to teardown; release the unit
	// reference on every exit path -- including the waitForExit failure below --
	// so systemd is not left pinning the unit until shim shutdown. Detached from
	// the request context for the same reason as the init path: the exec is
	// untracked as soon as this returns, so a skipped release is unrecoverable.
	defer func() {
		relCtx, cancel := cleanupContext(ctx)
		defer cancel()
		p.releaseUnit(relCtx, p.Name())
	}()

	ch := make(chan string, 1)
	if _, err := p.systemd.StopUnitContext(ctx, p.Name(), "replace", ch); err != nil {
		log.G(ctx).WithError(err).Info("Failed to stop unit")
	} else {
		// Try to wait for stop to complete. On context failure we'll use SIGKILL instead.
		select {
		case <-ctx.Done():
		case <-ch:
		}
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

	p.closeTTYControl()

	p.parent.execs.Delete(p.execID)
	// Reset any failed state so systemd can collect the transient unit once its
	// reference is released (see the deferred releaseUnit above).
	p.systemd.ResetFailedUnitContext(ctx, p.Name())
	if err := os.RemoveAll(p.stateDir()); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Debug("Failed to remove exec state dir")
	}

	return ps, nil
}
