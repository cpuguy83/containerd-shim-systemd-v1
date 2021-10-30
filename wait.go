package main

import (
	"context"
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

	if err := s.conn.Subscribe(); err != nil {
		return nil, err
	}

	name := unitName(ns, r.ID, "service")
	unitCh, errCh := s.conn.SubscribeUnitsCustom(time.Second, 10, func(u1, u2 *systemd.UnitStatus) bool { return u1.ActiveState != u2.ActiveState }, func(id string) bool {
		return id != name
	})

	var (
		st execState
	)

	if err := s.getState(ctx, name, &st); err != nil {
		log.G(ctx).WithError(err).Error("failed to get state")
	}

	log.G(ctx).Debugf("%+v", st)
	if st.ExitedAt.After(timeZero) {
		return &taskapi.WaitResponse{
			ExitStatus: st.ExitCode,
			ExitedAt:   st.ExitedAt,
		}, nil
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

			if err := s.getState(ctx, name, &st); err != nil {
				log.G(ctx).WithError(err).Error("failed to get state")
			}

			log.G(ctx).Debugf("%+v", st)

			if st.Pid == 0 {
				c, err := s.runc.State(ctx, runcName(ns, r.ID))
				if err != nil {
					log.G(ctx).WithError(err).Warn("error getting runc state")
					return nil, errdefs.ToGRPC(err)
				}
				if toStatus(c.Status) == task.StatusStopped {
					return &taskapi.WaitResponse{ExitStatus: 139, ExitedAt: time.Now()}, nil
				}
			}

			if st.ExitedAt.After(timeZero) {
				return &taskapi.WaitResponse{
					ExitStatus: st.ExitCode,
					ExitedAt:   st.ExitedAt,
				}, nil
			}
		}
	}
}
