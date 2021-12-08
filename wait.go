package main

import (
	"context"
	"fmt"
	"path"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Wait for a process to exit
func (s *Service) Wait(ctx context.Context, r *taskapi.WaitRequest) (retResp *taskapi.WaitResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Wait", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "wait")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

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
			return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
		}
	}

	st, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Debugf("%+v", st)

	return &taskapi.WaitResponse{
		ExitedAt:   st.ExitedAt,
		ExitStatus: st.ExitCode,
	}, nil
}

func (p *process) waitForExit(ctx context.Context) (pState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		if p.deleted {
			log.G(ctx).Debug("wait: deleted")
			break
		}
		if p.state.Exited() {
			log.G(ctx).Debugf("wait: exited: %s", p.state.ExitedAt)
			break
		}

		log.G(ctx).Debugf("%+s", p.state)

		select {
		case <-ctx.Done():
			log.G(ctx).Debug("wait: cancelled")
			return pState{}, ctx.Err()
		default:
		}

		p.cond.Wait()
	}

	var st pState
	p.state.CopyTo(&st)
	return st, nil
}

func (p *process) Wait(ctx context.Context) (pState, error) {
	return p.waitForExit(ctx)
}
