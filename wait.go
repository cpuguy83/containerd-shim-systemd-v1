package main

import (
	"context"
	"fmt"
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
		"execID":    r.ExecID,
	}))

	log.G(ctx).Info("systemd.Wait Start")
	defer func() {
		if retResp != nil {
			log.G(ctx).WithError(retErr).WithField("exitedAt", retResp.ExitedAt).Info("systemd.Wait End")
		}
		if retErr != nil {
			retErr = errdefs.ToGRPC(fmt.Errorf("wait: %w", err))
		}
	}()

	p := s.processes.Get(path.Join(ns, r.ID))

	if r.ExecID != "" {
		p = p.(*initProcess).execs.Get(r.ExecID)
		if p == nil {
			return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", r.ID)
		}
		defer func() {
			if retErr == nil {
				p.Delete(ctx)
			}
		}()
	}

	st, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}

	return &taskapi.WaitResponse{
		ExitedAt:   st.ExitedAt,
		ExitStatus: st.ExitCode,
	}, nil
}

func (p *process) waitForPid() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for p.pid == 0 {
		p.cond.Wait()
	}
}

// TODO: May need to refactor this for exec processes
func (p *process) Wait(ctx context.Context) (*pState, error) {
	p.waitForPid()

	if err := p.systemd.Subscribe(); err != nil {
		return nil, err
	}

	name := unitName(p.ns, p.id) + ".service"
	// TODO: We should only have one subscriber for the whole daemon that updates the internal state.
	unitCh, errCh := p.systemd.SubscribeUnitsCustom(time.Second, 10, func(u1, u2 *systemd.UnitStatus) bool {
		return *u1 != *u2 && (u2.ActiveState == "inactive" || u2.ActiveState == "failed")
	}, func(id string) bool {
		return id != name
	})

	var st pState
	st.Pid = p.Pid()

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
			state, ok := units[name]
			if !ok {
				log.G(ctx).Debugf("Our unit is not in the state changes: %+v", units)
				continue
			}

			log.G(ctx).Debugf("+%v", state)

			if err := getUnitState(ctx, p.systemd, name, &st); err != nil {
				log.G(ctx).WithError(err).Error("failed to get state")
			}

			if st.ExitedAt.After(timeZero) {
				return &st, nil
			}

			if state == nil {
				log.G(ctx).Debug("Unit was deleted")
				// The unit was deleted and we no longer have this information
				st.Pid = p.Pid()
				st.ExitCode = 139
				st.ExitedAt = time.Now()
				return &st, nil
			}

			// TODO: This will always fail on execs
			c, err := p.runc.State(ctx, runcName(p.ns, p.id))
			if err == nil {
				if toStatus(c.Status) == task.StatusStopped {
					st.ExitCode = 139
					st.ExitedAt = time.Now()
					return &st, nil
				}
			}

			log.G(ctx).Debugf("%+v", st)

			if st.ExitedAt.After(timeZero) {
				return &st, nil
			}
		}
	}
}
