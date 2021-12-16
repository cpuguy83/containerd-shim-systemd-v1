package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/coreos/go-systemd/v22/dbus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Watch periodically polls systemd for status updates of units being tracked.
//
// We are polling instead of watching for systemd events b/c the events seem to trigger a library bug
// where we end up getting bombarded with events at a rediculous rate, causing huge amounts of CPU usage.
// Until that gets figured out, we need to keep using polling.
func (m *unitManager) Watch(ctx context.Context) {
	filterFn := func(p Process) bool {
		return p.ProcessState().ExitedAt.After(timeZero)
	}

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	if !timer.Stop() {
		<-timer.C
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		m.mu.Lock()
		for len(m.idx) == 0 {
			select {
			case <-ctx.Done():
				m.mu.Unlock()
				log.G(ctx).WithError(ctx.Err()).Info("Exiting unit watch loop")
				return
			default:
			}
			m.cond.Wait()
		}
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			log.G(ctx).WithError(ctx.Err()).Info("Exiting unit watch loop")
			return
		default:
		}

		units, err := m.sd.ListUnitsByNamesContext(ctx, m.Keys(filterFn))
		if err != nil {
			log.G(ctx).WithError(err).Error("Error while watching unit statuses")
		}

		for _, unit := range units {
			unit := unit
			ctx := log.WithLogger(ctx, log.G(ctx).WithField("unit", unit.Name))

			p := m.Get(unit.Name)
			if p == nil {
				log.G(ctx).Debugf("Skipping unit status update for unknown unit")
				continue
			}

			ctx = WithShimLog(ctx, p.LogWriter())

			if p.ProcessState().Exited() {
				// Process is already exited, we don't care about state updates on this unit anymore
				log.G(ctx).Debug("Skipped unit status update for exited process")
				continue
			}

			log.G(ctx).Debugf("Getting unit state")
			if err := p.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).Error("Error loading process state")
				continue
			}

			log.G(ctx).WithField("unit", p.Name()).Debugf("Updated unit state: %s", p.ProcessState())
		}

		timer.Reset(time.Minute)
		select {
		case <-ctx.Done():
			log.G(ctx).WithError(ctx.Err()).Info("Exiting unit watch loop")
			return
		case <-hup:
			log.G(ctx).Debug("Received SIGHUP, reloading units")
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (m *unitManager) Keys(filter func(p Process) bool) []string {
	m.mu.Lock()
	keys := make([]string, 0, len(m.idx))
	for k, p := range m.idx {
		if filter(p) {
			continue
		}
		keys = append(keys, k)
	}
	m.mu.Unlock()
	return keys
}

func (s *Service) watchUnits(ctx context.Context) error {
	go s.units.Watch(ctx)
	return nil
}

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.State", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "state")
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

	// if ts := state["ExecMainExitTimestamp"]; ts != nil {
	// st.ExitedAt = time.UnixMicro(int64(ts.(uint64)))
	// if !st.ExitedAt.After(timeZero) {
	if st.ExitCode == 0 {
		code, _ := readExecStatusExit(state["ExecStart"].([][]interface{})[0])
		// if t.After(timeZero) {
		// st.ExitedAt = t
		if code > 0 {
			st.ExitCode = uint32(code)
		}
		// }
	}
	//}
	// }
	// if status := state["SubState"]; status != nil {
	// 	st.Status = status.(string)
	// }

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
		p.SetState(ctx, st)
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

	st.Reset()
	if err := getUnitState(ctx, p.systemd, p.Name(), &st); err != nil {
		return err
	}

	if st.Exited() {
		// ok try reading the exit state file again...
		var st2 pState
		if err := p.readExitState(&st2); err == nil {
			st = st2
		}
	}

	log.G(ctx).WithField("unit", p.Name()).Debugf("Setting unit state from systemd: %s", st)
	p.SetState(ctx, st)
	return nil
}

func (p *execProcess) exitStatePath() string {
	return filepath.Join(p.parent.Bundle, p.execID+"_exit_status.json")
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

func toStatus(s string) task.Status {
	switch s {
	case "created", "start-pre":
		return task.StatusCreated
	case "running", "start-post":
		return task.StatusRunning
	case "pausing":
		return task.StatusPausing
	case "paused":
		return task.StatusPaused
	case "stopped", "dead", "failed", "stop-post":
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

func (s pState) Exited() bool {
	if s.ExitCode > 0 {
		return true
	}
	// if s.Status == "exited" || s.Status == "failed" || s.Status == "dead" {
	// 	return true
	// }
	return s.ExitedAt.After(timeZero)
}

func (s pState) String() string {
	if !s.ExitedAt.After(timeZero) {
		return fmt.Sprintf("pid: %d, code: %d, status: %s", s.Pid, s.ExitCode, s.Status)
	}
	return fmt.Sprintf("pid: %d, code: %d, exitedAt: %s, status: %s", s.Pid, s.ExitCode, s.ExitedAt, s.Status)
}

// CopyTo copies the state to the provided destination.
// It does not override non-zero values (except "Status") in the destination.
// This is to ensure we don't override real information in the state w/, for instance, state info for a deleted unit.
func (s *pState) CopyTo(other *pState) {
	if s.Pid == 0 {
		return
	}
	if !other.ExitedAt.After(timeZero) {
		// systemd seems to have a habbit of keeping old data around...
		if s.Status == "dead" || s.Status == "failed" || s.Status == "exited" || s.Status != "stop-post" {
			other.ExitedAt = s.ExitedAt
		}
	}
	if s.ExitCode > 0 && other.ExitCode == 0 {
		other.ExitCode = s.ExitCode
	}
	if other.Pid == 0 {
		other.Pid = s.Pid
	}
	if s.Status != "" {
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
