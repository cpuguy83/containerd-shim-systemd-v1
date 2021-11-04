package main

import (
	"context"
	"path"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/sirupsen/logrus"
)

// Wait for a process to exit
func (s *Service) Wait(ctx context.Context, r *taskapi.WaitRequest) (retResp *taskapi.WaitResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"id":        r.ID,
		"ns":        ns,
		"apiAction": "wait",
	}))

	log.G(ctx).Info("Start")
	defer func() {
		if retResp != nil {
			log.G(ctx).WithError(retErr).WithField("exitedAt", retResp.ExitedAt).Info("End")
		}
	}()

	// TODO: exec
	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
	}

	st, err := p.Wait(ctx)
	if err != nil {
		return nil, errdefs.ToGRPCf(err, "wait")
	}

	return &taskapi.WaitResponse{
		ExitedAt:   st.ExitedAt,
		ExitStatus: st.ExitCode,
	}, nil
}

// TODO: May need to refactor this for exec processes
func (p *process) Wait(ctx context.Context) (*pState, error) {
	if err := p.systemd.Subscribe(); err != nil {
		return nil, err
	}

	name := unitName(p.ns, p.id) + ".service"
	unitCh, errCh := p.systemd.SubscribeUnitsCustom(time.Second, 10, func(u1, u2 *systemd.UnitStatus) bool { return u1.ActiveState != u2.ActiveState }, func(id string) bool {
		return id != name
	})

	var st pState

	if err := getUnitState(ctx, p.systemd, name, &st); err != nil {
		log.G(ctx).WithError(err).Error("failed to get state")
	}

	log.G(ctx).Debugf("%+v", st)
	if st.ExitedAt.After(timeZero) {
		return &st, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-errCh:
			return nil, err
		case units := <-unitCh:
			log.G(ctx).Debugf("Got %d state updates", len(units))
			_, ok := units[name]
			if !ok {
				log.G(ctx).Debug("Our unit is not in the state changes")
				continue
			}

			if err := getUnitState(ctx, p.systemd, name, &st); err != nil {
				log.G(ctx).WithError(err).Error("failed to get state")
			}

			log.G(ctx).Debugf("%+v", st)

			if st.Pid == 0 {
				c, err := p.runc.State(ctx, runcName(p.ns, p.id))
				if err != nil {
					log.G(ctx).WithError(err).Warn("error getting runc state")
					return nil, errdefs.ToGRPC(err)
				}
				if toStatus(c.Status) == task.StatusStopped {
					st.ExitCode = 139
					st.ExitedAt = time.Now()
					return &st, nil
				}
			}

			if st.ExitedAt.After(timeZero) {
				return &st, nil
			}
		}
	}
}
