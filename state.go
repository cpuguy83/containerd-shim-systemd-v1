package main

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
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
	countState(metricGetUnitCalls)
	state, err := conn.GetAllPropertiesContext(ctx, unit)
	if err != nil {
		return err
	}
	applyUnitProperties(state, st)
	return nil
}

// applyUnitProperties folds a unit's systemd properties into st. It is the
// property-read counterpart of serviceExitState, which decodes the same facts
// out of a signal, and is separate from the read itself so the precedence
// between the two accounts of an exit is testable without a bus.
func applyUnitProperties(state map[string]interface{}, st *pState) {
	if status := state["SubState"]; status != nil {
		st.Status = status.(string)
	}

	// ExecMainPID is the create re-exec's own pid until it hands the workload
	// over with MAINPID=, which for a notify unit is the moment the unit
	// finishes activating. Reading it before then latches the re-exec's pid as
	// the task's, and the first pid the shim records is the one it keeps.
	if !activatingSubState(st.Status) {
		if p := state["ExecMainPID"]; p != nil {
			st.Pid = uint32(p.(uint32))
		}
	}
	// systemd reports a killed main process as the bare signal number, with
	// ExecMainCode carrying the si_code that says so.
	if c := state["ExecMainStatus"]; c != nil {
		var mainCode int32
		if v := state["ExecMainCode"]; v != nil {
			mainCode = v.(int32)
		}
		st.ExitCode = exitStatusFor(mainCode, c.(int32))
	}

	if ts := state["ExecMainExitTimestamp"]; ts != nil {
		if micros := ts.(uint64); micros > 0 {
			st.ExitedAt = time.UnixMicro(int64(micros))
		}
	}

	// The create re-exec's own report comes last because it is the only source
	// for an exit systemd did not witness: when the re-exec reaped the workload
	// itself, ExecMainPID/Code/Status describe the re-exec rather than the
	// workload, and SubState only says the unit is dead.
	if v := state["StatusText"]; v != nil {
		if status, pid := parseStatusText(v.(string)); status == exitedInit || status == "exited" {
			st.Status = status
			st.Pid = pid
			st.ExitCode = 0
			if e := state["StatusErrno"]; e != nil {
				if errno := e.(int32); errno > 0 {
					st.ExitCode = uint32(errno)
				}
			}
		}
	}

	// A unit that has not started yet reads exactly like one that failed before it
	// ever had a main process: inactive/dead, no main pid, no exit code. Result
	// tells them apart -- it stays "success" until something actually fails.
	//
	// Getting this wrong in either direction is a real bug: reporting the
	// pre-start state as an exit kills a container before runc runs, and
	// discarding a genuine early failure (an ExecStartPre that is not allowed to
	// fail, say) leaves the task looking alive with nothing left to report it.
	if st.Pid == 0 && st.ExitCode == 0 && toStatus(st.Status) == task.Status_STOPPED {
		if unitFailed(state) {
			// No workload ran, so there is no exit code to report. 255 is the
			// shim's code for a failure of its own making.
			st.ExitCode = 255
		} else {
			st.Reset()
		}
	}

	// A terminal state is worth nothing to containerd without an exit time, and
	// a report the create re-exec sent carries none of its own. Stamping the
	// read is the closest available: the exit it describes has only just
	// happened.
	if st.Exited() && !st.ExitedAt.After(timeZero) {
		st.ExitedAt = time.Now()
	}
}

// unitFailed reports whether systemd's Result says the unit failed. It reads
// "success" both for a unit that has not run yet and for one that ran cleanly,
// and systemd sets it only after applying the "-" ignore-failure prefix on Exec*
// commands -- so a command that is allowed to fail never shows up here.
func unitFailed(state map[string]interface{}) bool {
	result, _ := state["Result"].(string)
	return result != "" && result != "success"
}

// activatingSubState reports whether a service SubState is one systemd uses
// while the unit is still starting, so its main process is the create re-exec
// rather than the workload the re-exec is bringing up.
func activatingSubState(s string) bool {
	switch s {
	case "start-pre", "start":
		return true
	}
	return false
}

// parseStatusText splits the STATUS= text notifyStatus sends into the status
// and the pid it belongs to.
//
// Text without a usable pid yields the status alone rather than an error: the
// status is what says the workload exited, and losing the pid is better than
// losing the exit.
func parseStatusText(s string) (status string, pid uint32) {
	status, rest, ok := strings.Cut(s, " ")
	if !ok {
		return status, 0
	}
	v, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return status, 0
	}
	return status, uint32(v)
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
	st.Reset()
	if err := getUnitState(ctx, p.systemd, p.Name(), &st); err != nil {
		return err
	}
	p.SetState(ctx, st)
	return nil
}

func (p *execProcess) LoadState(ctx context.Context) error {
	var st pState
	st.Reset()
	if err := getUnitState(ctx, p.systemd, p.Name(), &st); err != nil {
		return err
	}
	p.SetState(ctx, st)
	return nil
}

// loadExitFromUnit reads a unit's terminal state directly, for callers that have
// no recorded exit to fall back on -- delete, which has just stopped the unit
// itself, and the tests that assert against systemd.
func loadExitFromUnit(ctx context.Context, conn *dbus.Conn, name string) (pState, error) {
	var st pState
	st.Reset()
	if err := getUnitState(ctx, conn, name, &st); err != nil {
		return pState{}, err
	}
	return st, nil
}

// LoadRecordedExitState applies the terminal state the reactor decoded from the
// unit's own signal, and reports whether there was one. It does no I/O: the
// signal that enqueued a unit already carried the exit, so a reconcile can
// answer from it rather than asking systemd for what it has already been told.
func (p *initProcess) LoadRecordedExitState(ctx context.Context) bool {
	st, ok := p.loadRecordedSystemdExitState()
	if !ok {
		return false
	}
	countState(metricReactorHits)
	p.SetState(ctx, st)
	return true
}

func (p *execProcess) LoadRecordedExitState(ctx context.Context) bool {
	st, ok := p.loadRecordedSystemdExitState()
	if !ok {
		return false
	}
	countState(metricReactorHits)
	p.SetState(ctx, st)
	return true
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
	beforeRunning := p.phase < phaseRunning
	p.mu.Unlock()

	// An exec reports Created until its process actually runs. If the
	// container's init died before this exec's process started, runc's exec
	// init exits (status exited-init) without ever running the process, so
	// report Created rather than a synthetic exit. An exec that was set up but
	// never started likewise has no recorded state.
	initDied := st.State.Status == exitedInit && p.parent.ProcessState().Exited()
	if initDied || (beforeRunning && !st.State.Exited()) {
		st.State.Status = "created"
		st.State.ExitCode = 0
		st.State.ExitedAt = timeZero
	}

	return st, nil
}

const (
	exitedInit = "exited-init"

	// exitSignalOffset matches containerd's own reaper: a process terminated by
	// a signal is reported as 128+signal, which is what its clients render as
	// e.g. 137 for SIGKILL.
	exitSignalOffset = 128

	// cldExited, cldKilled and cldDumped are the si_code values systemd exposes
	// as ExecMainCode: whether the main process returned an exit code or was
	// terminated by a signal.
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

// exitStatusFor turns a systemd (ExecMainCode, ExecMainStatus) pair into the
// exit status containerd expects. systemd reports a process terminated by a
// signal as the bare signal number, distinguished only by the si_code in
// ExecMainCode; containerd reports it as 128+signal.
//
// Both places the shim learns an exit from systemd -- the reactor's signal
// decoder and the GetAll fallback -- go through this, so the same kill cannot
// come out as 9 or 137 depending on which of them saw it first.
func exitStatusFor(mainCode, mainStatus int32) uint32 {
	if mainStatus <= 0 {
		return 0
	}
	switch mainCode {
	case cldKilled, cldDumped:
		return uint32(mainStatus) + exitSignalOffset
	}
	return uint32(mainStatus)
}

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
	// A pid-less, non-terminal state carries no real information (e.g. a read of
	// a deleted or never-started unit) and must not clobber the destination. A
	// terminal exit, however, is meaningful even without a pid: systemd reports
	// ExecMainPID as 0 once the unit is dead, so requiring a pid here would drop
	// the exit and leave the process wedged in a non-terminal state.
	if s.Pid == 0 && !s.Exited() {
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
