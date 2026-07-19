package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/coreos/go-systemd/v22/dbus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.State", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("state: %w", retErr))
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

	var st *State
	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, fmt.Errorf("exec %s: %w", r.ExecID, errdefs.ErrNotFound)
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
		ID:         r.ID,
		ExecID:     r.ExecID,
		Bundle:     st.Bundle,
		Pid:        st.State.Pid,
		ExitStatus: st.State.ExitCode,
		ExitedAt:   timestamppb.New(st.State.ExitedAt),
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

	// if ts := state["ExecMainExitTimestamp"]; ts != nil {
	// st.ExitedAt = time.UnixMicro(int64(ts.(uint64)))
	// if !st.ExitedAt.After(timeZero) {
	if st.ExitCode == 0 {
		execStart := state["ExecStart"].([][]interface{})
		if len(execStart) > 0 {
			code, _ := readExecStatusExit(state["ExecStart"].([][]interface{})[0])
			// if t.After(timeZero) {
			// st.ExitedAt = t
			if code > 0 {
				st.ExitCode = uint32(code)
			}
		}
		// }
	}
	//}
	// }
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

func (p *initProcess) LoadState(ctx context.Context) error {
	var st pState
	if err := p.readExitState(&st); err == nil {
		if st.Pid > 0 && st.Status == "" {
			st.Status = "running"
		}
		p.SetState(ctx, st)
		return nil
	} else if p.Pid() == 0 {
		// Nothing to load
		return nil
	}

	st.Reset()
	if err := getUnitState(ctx, p.systemd, p.Name(), &st); err != nil {
		return err
	}
	p.SetState(ctx, st)
	return nil
}

func (p *execProcess) LoadState(ctx context.Context) error {
	var st pState
	err := p.readExitState(&st)
	if err == nil {
		p.SetState(ctx, st)
		return nil
	}

	if !os.IsNotExist(err) {
		log.G(ctx).WithField("unit", p.Name()).WithError(err).Debug("Error reading exit state file")
	}
	return nil
}

// loadExitFromUnit is the fallback when the signal stream did not record a
// terminal Service update. It reads ExecMainStatus and SubState while the unit
// is still loaded. getUnitState leaves ExitedAt unset, so stamp it for a clean
// exit whose code is 0 (its exited status still comes through SubState).
func loadExitFromUnit(ctx context.Context, conn *dbus.Conn, name string) (pState, error) {
	var st pState
	st.Reset()
	if err := getUnitState(ctx, conn, name, &st); err != nil {
		return pState{}, err
	}
	if st.Exited() && !st.ExitedAt.After(timeZero) {
		st.ExitedAt = time.Now()
	}
	return st, nil
}

func (p *initProcess) LoadExitState(ctx context.Context) error {
	if st, ok := p.loadRecordedSystemdExitState(); ok {
		p.SetState(ctx, st)
		return nil
	}
	st, err := loadExitFromUnit(ctx, p.systemd, p.Name())
	if err != nil {
		return err
	}
	p.SetState(ctx, st)
	return nil
}

func (p *execProcess) LoadExitState(ctx context.Context) error {
	if st, ok := p.loadRecordedSystemdExitState(); ok {
		p.SetState(ctx, st)
		return nil
	}
	st, err := loadExitFromUnit(ctx, p.systemd, p.Name())
	if err != nil {
		return err
	}
	p.SetState(ctx, st)
	return nil
}

func (p *execProcess) exitStatePath() string {
	return filepath.Join(p.stateDir(), "exit_status.json")
}

func (p *execProcess) readExitState(st *pState) error {
	data, err := os.ReadFile(p.exitStatePath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, st)
}

func (p *initProcess) exitStatePath() string {
	return filepath.Join(p.Bundle, "init_exit_status.json")
}

func (p *initProcess) readExitState(st *pState) error {
	data, err := os.ReadFile(p.exitStatePath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, st)
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

const (
	exitedInit = "exited-init"
)

func toStatus(s string) task.Status {
	switch s {
	case "created", "start-pre":
		return task.Status_CREATED
	case "running", "start-post":
		return task.Status_RUNNING
	case "pausing":
		return task.Status_PAUSING
	case "paused":
		return task.Status_PAUSED
	case "stopped", "dead", "failed", "stop-post", "exited", exitedInit, "exit-code":
		return task.Status_STOPPED
	default:
		return task.Status_UNKNOWN
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

func (s pState) Exited() bool {
	if s.ExitCode > 0 {
		return true
	}
	if toStatus(s.Status) == task.Status_STOPPED {
		return true
	}
	return s.ExitedAt.After(timeZero)
}

func (s pState) Started() bool {
	return s.Pid > 0
}

func (s pState) String() string {
	if !s.ExitedAt.After(timeZero) {
		return fmt.Sprintf("pid: %d, code: %d, status: %s", s.Pid, s.ExitCode, s.Status)
	}
	return fmt.Sprintf("pid: %d, code: %d, exitedAt: %s, status: %s", s.Pid, s.ExitCode, s.ExitedAt, s.Status)
}

// CopyTo copies the state to the provided destination.
// It does not override non-zero values in the destination or regress a terminal
// status to a non-terminal status.
// This is to ensure we don't override real information in the state w/, for instance, state info for a deleted unit.
func (s *pState) CopyTo(other *pState) {
	if s.Pid == 0 {
		return
	}
	if s.ExitedAt.After(timeZero) && !other.ExitedAt.After(timeZero) {
		other.ExitedAt = s.ExitedAt
	}
	if s.ExitCode > 0 && other.ExitCode == 0 {
		other.ExitCode = s.ExitCode
	}
	if other.Pid == 0 {
		other.Pid = s.Pid
	}
	if s.Status != "" && (!other.Exited() || s.Exited()) {
		other.Status = s.Status
	}
}

type execState struct {
	Path       string
	Started    time.Time
	Exited     time.Time
	ExitStatus uint32
	Args       []string
	Pid        uint32
	StatusCode uint32 // This is the systemd status (e.g. "exited")
}

func readExecStatusExit(i []interface{}) (uint32, time.Time) {
	return uint32(i[9].(int32)), time.UnixMicro(int64(i[5].(uint64)))
}

func parseExecStartStatus(ii [][]interface{}, st *execState) error {
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
