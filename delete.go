package main

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
)

// Delete a process or container
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (_ *taskapi.DeleteResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns))
	log.G(ctx).Info("systemd.Delete begin")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Delete end")
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s", r.ID)
	}

	st, err := p.Delete(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	s.processes.Delete(path.Join(ns, r.ID))

	return &taskapi.DeleteResponse{
		Pid:        st.Pid,
		ExitStatus: st.ExitCode,
		ExitedAt:   st.ExitedAt,
	}, nil
}

func (p *initProcess) Delete(ctx context.Context) (*pState, error) {
	name := unitName(p.ns, p.id) + ".service"

	defer func() {
		if err := mount.UnmountAll(filepath.Join(p.Bundle, "rootfs"), 0); err != nil {
			log.G(ctx).WithError(err).Error("failed to cleanup rootfs mount")
		}
		if err := os.RemoveAll(p.root); err != nil {
			log.G(ctx).WithError(err).Error("Error removing container root directory")
		}
	}()

	if err := p.runc.Delete(ctx, runcName(p.ns, p.id), &runc.DeleteOpts{Force: true}); err != nil {
		return nil, err
	}

	var st pState
	if err := getUnitState(ctx, p.systemd, name, &st); err != nil {
		log.G(ctx).WithError(err).Error("Error getting unit state")
	} else {
		ctx = log.WithLogger(ctx, log.G(ctx).WithField("statusCode", st.ExitCode))
	}

	if p.Terminal {
		p.systemd.KillUnitContext(ctx, unitName(p.ns, p.id+"-tty")+".service", 9)
	}

	if st.ExitCode != 0 {
		if err := p.systemd.ResetFailedUnitContext(ctx, name); err != nil {
			log.G(ctx).WithError(err).Debug("Error reseting failed unit")
		}
		if p.Terminal {
			p.systemd.ResetFailedUnitContext(ctx, unitName(p.ns, p.id+"-tty")+".service")
		}
	}

	return &st, nil
}
