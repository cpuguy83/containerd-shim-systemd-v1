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
	"github.com/coreos/go-systemd/v22/dbus"
)

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("execID", r.ExecID))

	log.G(ctx).Info("systemd.State")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.State end")
		if retErr != nil {
			retErr = errdefs.ToGRPC(fmt.Errorf("state: %w", retErr))
		}
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", r.ID)
	}

	var st *State
	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "exec %s not found", r.ExecID)
		}
		st, err = ep.State(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		st, err = p.State(ctx)
		if err != nil {
			return nil, err
		}
	}

	if st.pState.ExitedAt.After(timeZero) {
		ctx = log.WithLogger(ctx, log.G(ctx).WithField("status", st.Status).WithField("exitedAt", st.ExitedAt))
	}

	return &taskapi.StateResponse{
		ID:         st.ID,
		Bundle:     st.Bundle,
		Pid:        st.Pid,
		ExitStatus: uint32(st.Status),
		ExitedAt:   st.ExitedAt,
		Status:     st.Status,
		Stdin:      st.Stdin,
		Stdout:     st.Stdout,
		Stderr:     st.Stderr,
		Terminal:   st.Terminal,
	}, nil
}

func getUnitState(ctx context.Context, conn *dbus.Conn, unit string, st *pState) error {
	state, err := conn.GetAllPropertiesContext(ctx, unit)
	if err != nil {
		return err
	}

	if p := state["ExecMainPID"]; p != nil {
		st.Pid = uint32(p.(uint32))
	}
	if c := state["ExecMainStatus"]; c != nil {
		st.ExitCode = uint32(c.(int32))
	}
	if ts := state["ExecMainExitTimestamp"]; ts != nil {
		st.ExitedAt = time.UnixMicro(int64(ts.(uint64)))
	}
	return nil
}

type State struct {
	pState
	Bundle                string
	ID                    string
	Stdin, Stdout, Stderr string
	Terminal              bool
	Status                task.Status
}

func (p *initProcess) State(ctx context.Context) (*State, error) {
	c, err := p.runc.State(ctx, runcName(p.ns, p.id))
	if err != nil {
		return nil, err
	}

	status := toStatus(c.Status)
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("status", status))

	resp := &State{
		Bundle:   c.Bundle,
		ID:       c.ID,
		Stdin:    p.Stdin,
		Stdout:   p.Stdout,
		Stderr:   p.Stderr,
		Terminal: p.Terminal,
	}

	if status == task.StatusStopped {
		if err := getUnitState(ctx, p.systemd, unitName(p.ns, p.id)+".service", &resp.pState); err != nil {
			return nil, err
		}
	}

	return resp, nil
}

func (p *execProcess) State(ctx context.Context) (*State, error) {
	st := &State{
		ID:       p.id,
		Bundle:   p.parent.Bundle,
		Stdin:    p.Stdin,
		Stdout:   p.Stdout,
		Stderr:   p.Stderr,
		Terminal: p.Terminal,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pid != 0 {
		if err := getUnitState(ctx, p.systemd, unitName(p.ns, p.id)+".service", &st.pState); err != nil {
			return nil, err
		}
	}

	// TODO: pause states
	switch {
	case st.ExitedAt.After(timeZero):
		st.Status = task.StatusStopped
	case p.pid > 0:
		st.Status = task.StatusRunning
	default:
		st.Status = task.StatusCreated
	}

	return st, nil
}

func toStatus(s string) task.Status {
	switch s {
	case "created":
		return task.StatusCreated
	case "running":
		return task.StatusRunning
	case "pausing":
		return task.StatusPausing
	case "paused":
		return task.StatusPaused
	case "stopped":
		return task.StatusStopped
	default:
		return task.StatusUnknown
	}
}

type pState struct {
	ExitedAt time.Time
	ExitCode uint32
	Pid      uint32
}

type execStartState struct {
	Path       string
	Started    time.Time
	Exited     time.Time
	ExitStatus uint32
	Args       []string
	Pid        uint32
	StatusCode uint32 // This is the systemd status (e.g. "exited")
}

func parseExecStartStatus(ii [][]interface{}, st *execStartState) error {
	if len(ii) == 0 {
		return errdefs.ErrNotFound
	}

	i := ii[0]

	st.Path = i[0].(string)
	st.Args = i[1].([]string)

	if u64 := i[3].(uint64); u64 > 0 {
		st.Started = time.UnixMicro(int64(u64))
	}

	if u64 := i[5].(uint64); u64 > 0 {
		st.Exited = time.UnixMicro(int64(u64))
	}

	st.Pid = i[7].(uint32)
	st.StatusCode = uint32(i[8].(int32))
	st.ExitStatus = uint32(i[9].(int32))

	return nil
}
