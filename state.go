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

func (s *Service) watchUnits(ctx context.Context) error {
	updates := make(chan *dbus.SubStateUpdate, 100)
	errs := make(chan error, 100)
	go func() {
		var st pState
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				log.G(ctx).WithError(err).Error("error while watching for unit state updates")
			case u := <-updates:
				p := s.units.Get(u.UnitName)
				if p == nil {
					continue
				}
				if err := getUnitState(ctx, s.conn, u.UnitName, &st); err != nil {
					log.G(ctx).WithError(err).Warn("Error getting unit state")
					continue
				}
				p.SetState(st)
				log.G(ctx).WithField("unit", u.UnitName).WithField("substate", u.SubState).Debugf("%+v", p.ProcessState())
				st.Reset()
			}
		}
	}()

	s.conn.SetSubStateSubscriber(updates, errs)
	return nil
}

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

	return &taskapi.StateResponse{
		ID:         st.ID,
		Bundle:     st.Bundle,
		Pid:        st.State.Pid,
		ExitStatus: st.State.ExitCode,
		ExitedAt:   st.State.ExitedAt,
		Status:     toStatus(st.State.Status),
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
	if status := state["SubState"]; status != nil {
		st.Status = status.(string)
	}
	return nil
}

type State struct {
	State                 pState
	Bundle                string
	ID                    string
	Stdin, Stdout, Stderr string
	Terminal              bool
}

func (p *initProcess) State(ctx context.Context) (*State, error) {
	resp := &State{
		Bundle:   p.Bundle,
		ID:       p.id,
		Stdin:    p.Stdin,
		Stdout:   p.Stdout,
		Stderr:   p.Stderr,
		Terminal: p.Terminal,
	}

	p.mu.Lock()
	p.state.CopyTo(&resp.State)
	p.mu.Unlock()

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
	p.state.CopyTo(&st.State)
	p.mu.Unlock()

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
	case "stopped", "dead", "failed":
		return task.StatusStopped
	default:
		return task.StatusUnknown
	}
}

type pState struct {
	ExitedAt time.Time
	ExitCode uint32
	Pid      uint32
	Status   string
}

func (s *pState) Reset() {
	s.ExitedAt = timeZero
	s.ExitCode = 0
	s.Pid = 0
	s.Status = ""
}

// CopyTo copies the state to the provided destination.
// It does not override non-zero values (except "Status") in the destination.
// This is to ensure we don't override real information in the state w/, for instance, state info for a deleted unit.
func (s *pState) CopyTo(other *pState) {
	if !other.ExitedAt.After(timeZero) {
		other.ExitedAt = s.ExitedAt
	}
	if other.ExitCode == 0 {
		other.ExitCode = s.ExitCode
	}
	if other.Pid == 0 {
		other.Pid = s.Pid
	}
	if s.Status != "" {
		other.Status = s.Status
	}
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
