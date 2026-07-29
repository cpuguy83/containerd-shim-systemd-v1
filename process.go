package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types"
	v2runcopts "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/errdefs"
	"github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type processManager struct {
	mu sync.Mutex
	ls map[string]Process
}

func newUnitManager() *unitManager {
	return &unitManager{byPath: make(map[string]Process)}
}

type unitManager struct {
	mu sync.Mutex
	// byPath maps a unit's systemd-escaped D-Bus object-path base to its
	// process. The event reactor resolves incoming signals through this index so
	// it never has to unescape a name on the signal path; the escaping happens
	// once here, when a unit is added or removed.
	byPath map[string]Process
}

func (m *unitManager) Add(p Process) {
	m.mu.Lock()
	m.byPath[p.PathName()] = p
	m.mu.Unlock()
}

func (m *unitManager) Delete(p Process) {
	m.mu.Lock()
	delete(m.byPath, p.PathName())
	m.mu.Unlock()
	log.G(context.TODO()).Debugf("deleted unit %s", p.Name())
}

// GetByPath resolves a process from a systemd unit object-path base (the escaped
// unit name carried by a D-Bus signal), without unescaping it. It returns nil
// for units the shim does not track -- the private bus broadcasts every unit's
// signals, so most lookups miss.
func (m *unitManager) GetByPath(pathBase string) Process {
	m.mu.Lock()
	p := m.byPath[pathBase]
	m.mu.Unlock()
	return p
}

// Paths returns the systemd-escaped object-path base of every tracked unit --
// the keys GetByPath accepts. The reactor uses it to resync all tracked units
// after a reconnect.
func (m *unitManager) Paths() []string {
	m.mu.Lock()
	paths := make([]string, 0, len(m.byPath))
	for p := range m.byPath {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	return paths
}

func (m *processManager) Add(id string, p Process) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.ls[id]; ok {
		return errdefs.ErrAlreadyExists
	}

	m.ls[id] = p
	return nil
}

func (m *processManager) Get(id string) Process {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ls[id]
}

func (m *processManager) Delete(id string) {
	m.mu.Lock()
	delete(m.ls, id)
	m.mu.Unlock()
}

func (m *processManager) Each(do func(p Process)) {
	m.mu.Lock()
	for _, p := range m.ls {
		do(p)
	}
	m.mu.Unlock()
}

type Process interface {
	Start(context.Context) (uint32, error)
	ResizePTY(ctx context.Context, w, h int) error
	Wait(context.Context) (pState, error)
	Delete(context.Context) (pState, error)
	State(ctx context.Context) (*State, error)
	Kill(context.Context, int, bool) error
	Pid() uint32
	Name() string
	// PathName returns the unit's systemd-escaped D-Bus object-path base, the
	// identifier the event reactor matches incoming signals against.
	PathName() string
	LoadState(context.Context) error
	// LoadExitState refreshes the process from terminal systemd properties
	// recorded by the signal stream, falling back to GetAll when necessary.
	LoadExitState(context.Context) error
	RecordSystemdExitState(pState)
	SetState(context.Context, pState) pState
	ProcessState() pState
	LogWriter() io.Writer
}

type CreateOptions struct {
	// Native config
	LogMode        string
	SdNotifyEnable bool

	// From runc types
	BinaryName          string
	Root                string
	NoPivotRoot         bool
	OpenTcp             bool
	ExternalUnixSockets bool
	Terminal            bool
	FileLocks           bool
	EmptyNamespaces     []string
	CgroupsMode         string
	NoNewKeyring        bool
	IoUid               uint32
	IoGid               uint32
	CriuWorkPath        string
	CriuImagePath       string
	SystemdCgroup       bool
	ShimCgroup          string
}

func (c CreateOptions) RestoreArgs() []string {
	var args []string

	if c.NoPivotRoot {
		args = append(args, "--no-pivot")
	}
	if c.OpenTcp {
		args = append(args, "--tcp-established")
	}
	if c.FileLocks {
		args = append(args, "--file-locks")
	}
	if c.ExternalUnixSockets {
		args = append(args, "--ext-unix-sk")
	}
	for _, ns := range c.EmptyNamespaces {
		args = append(args, "--empty-ns="+ns)
	}
	if c.CgroupsMode != "" {
		args = append(args, "--manage-cgroups-mode="+c.CgroupsMode)
	}

	return args
}

type process struct {
	ns   string
	id   string
	root string

	// pathName is the unit's systemd-escaped D-Bus object-path base, computed
	// once at creation. The event reactor matches incoming signals against it,
	// so it is stored rather than re-escaped on every manager lookup.
	pathName string

	exe        string
	notifyFifo string

	Stdin    string
	Stdout   string
	Stderr   string
	Terminal bool

	opts CreateOptions

	systemd *systemd.Conn
	runc    *runc.Runc

	// pinConn starts the transient unit with AddRef=true so systemd will not
	// garbage-collect it after a clean exit, keeping its terminal state queryable
	// over D-Bus. pinRef is the same message-bus connection's raw method handle,
	// used to UnrefUnit at Delete. Both are nil when no message bus is available,
	// in which case the unit starts on the private connection unpinned. They are
	// shared across all processes.
	pinConn *systemd.Conn
	pinRef  *dbus.Conn

	ttyControl *ttyControlClient

	mu   sync.Mutex
	cond *sync.Cond
	// state is the last observation merged into this process: what systemd, or
	// the create re-exec helper, said about the unit. phase is what the shim
	// makes of it -- see procPhase for why both exist.
	phase    procPhase
	state    pState
	deleting bool

	// events publishes this process's containerd task events. It decides which
	// of them containerd may see and in what order (see eventOutbox), so callers
	// hand it every event unconditionally. It lives here rather than on
	// initProcess so an exec publishes its own events instead of reaching
	// through its parent.
	events eventOutbox

	systemdExitState    pState
	hasSystemdExitState bool

	shimCgroup string
}

// advanceLocked moves the process into phase next, rejecting any transition
// phaseTransitions does not list. A rejection leaves the phase untouched and
// reports an invalidTransitionError; it never panics or forces the move, because
// most rejections are races the shim is expected to survive -- a unit event
// arriving after a delete, or a start losing to the exit it was starting.
//
// It reports whether the phase actually changed, which is what makes an exit
// observable exactly once: only one caller can be the one that moves a process
// into phaseExited.
//
// p.mu must be held.
func (p *process) advanceLocked(next procPhase) (changed bool, err error) {
	if !p.phase.canBecome(next) {
		return false, invalidTransitionError{from: p.phase, to: next}
	}
	if p.phase == next {
		return false, nil
	}
	p.phase = next
	return true, nil
}

// advance moves the process into phase next and wakes anything waiting on its
// state. It reports whether the phase actually changed, so a caller can act on a
// transition exactly once.
func (p *process) advance(next procPhase) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed, err := p.advanceLocked(next)
	p.cond.Broadcast()
	return changed, err
}

func (p *process) currentPhase() procPhase {
	p.mu.Lock()
	phase := p.phase
	p.mu.Unlock()
	return phase
}

// beginStart claims the process for a start attempt. It is the gate that makes
// an already-exited or already-deleted process impossible to start, and it runs
// before Start does any work so a rejected start never reaches runc. Only one
// attempt may hold the claim: a second concurrent Start is rejected rather than
// admitted alongside the first, whose unit it would otherwise tear down when its
// own runc start failed. Its counterparts are abortStart and
// initProcess/execProcess.finishStart.
func (p *process) beginStart() error {
	_, err := p.advance(phaseStarting)
	return err
}

// abortStart hands the claim back after a failed attempt so another can be made.
// It is a no-op once something else has moved the phase on -- the workload
// exited during the attempt, or a delete tore the process down -- because
// neither is a state a retry could start from.
func (p *process) abortStart(ctx context.Context) {
	if _, err := p.advance(phaseCreated); err != nil {
		log.G(ctx).WithError(err).WithField("id", p.id).Debug("Failed start attempt ended in a terminal phase")
	}
}

func (p *process) RecordSystemdExitState(state pState) {
	p.mu.Lock()
	if !p.hasSystemdExitState || state.ExitedAt.After(p.systemdExitState.ExitedAt) {
		p.systemdExitState = state
		p.hasSystemdExitState = true
	}
	p.mu.Unlock()
}

func (p *process) clearRecordedSystemdExitState() {
	p.mu.Lock()
	p.systemdExitState.Reset()
	p.hasSystemdExitState = false
	p.mu.Unlock()
}

func (p *process) loadRecordedSystemdExitState() (pState, bool) {
	p.mu.Lock()
	state := p.systemdExitState
	ok := p.hasSystemdExitState
	p.mu.Unlock()
	return state, ok
}

func (p *process) ProcessState() pState {
	p.mu.Lock()
	var st pState
	p.state.CopyTo(&st)
	p.mu.Unlock()
	return st
}

// stateUpdate is the outcome of merging one observation into a process: the
// resulting state and the lifecycle transition, if any, that it caused.
type stateUpdate struct {
	state    pState
	from, to procPhase
}

// justExited reports whether this update is the one that ended the process.
// Every state path -- the D-Bus event reconciler, the reconnect resync, and the
// start/delete helpers -- funnels an exit through SetState, but the resulting
// TaskExit must be emitted only once no matter how many of them observe the exit
// concurrently. Only one caller can move a process into phaseExited, so this
// answer is the exactly-once guarantee rather than a flag alongside it.
func (u stateUpdate) justExited() bool {
	return u.to == phaseExited && u.from != u.to
}

// wasRunning reports whether the process was running before this update.
func (u stateUpdate) wasRunning() bool {
	return u.from == phaseRunning
}

// applyState merges an observation into the process and advances the lifecycle
// if the result is terminal. The merge and the transition happen under one lock
// so no caller can see a terminal state that has not yet moved the phase.
func (p *process) applyState(ctx context.Context, state pState) stateUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()

	from := p.phase
	// systemd reports a unit as running as soon as `runc create` has the
	// container set up, which is still created as far as containerd is concerned.
	// The phase, not the observation, decides which of the two this is.
	if p.phase < phaseRunning && toStatus(state.Status) == task.Status_RUNNING {
		state.Status = "created"
	}
	state.CopyTo(&p.state)
	if p.state.ExitCode > 0 && !p.state.Exited() {
		p.state.ExitedAt = time.Now()
	}
	if p.state.Exited() {
		if _, err := p.advanceLocked(phaseExited); err != nil {
			log.G(ctx).WithError(err).WithField("id", p.id).Debug("Ignoring exit observed after delete")
		}
	}
	p.state.CopyTo(&state)
	p.cond.Broadcast()
	return stateUpdate{state: state, from: from, to: p.phase}
}

func (p *process) SetState(ctx context.Context, state pState) pState {
	return p.applyState(ctx, state).state
}

// markDeleting records that teardown has begun, before any of it runs. A start
// racing that teardown checks this once its unit exists (see isDeleting): the
// two orderings are then both safe -- either the unit already existed and
// teardown tears it down, or it did not and the starter sees the flag and
// abandons it. Without that, a delete could untrack the process just before a
// start created its unit, stranding a running, referenced unit that nothing
// would ever release.
//
// It is a flag rather than a phase because teardown is orthogonal to the
// lifecycle: it begins while the process may still be running and must still be
// able to observe that process exit, so a deleting phase would need edges both
// to and from phaseExited. phaseDeleted, which teardown reaches only once it has
// finished, is the phase half of the same story.
func (p *process) markDeleting() {
	p.mu.Lock()
	p.deleting = true
	p.mu.Unlock()
}

func (p *process) isDeleting() bool {
	p.mu.Lock()
	deleting := p.deleting
	p.mu.Unlock()
	return deleting
}

func (p *execProcess) Name() string {
	return unitName(p.ns, p.parent.id+"-"+p.id, "exec")
}

func (p *initProcess) Name() string {
	return unitName(p.ns, p.id, "init")
}

// PathName returns the unit's systemd-escaped D-Bus object-path base. It is
// populated once at creation (see Create in create.go) so signal lookups never
// re-escape the name.
func (p *process) PathName() string {
	return p.pathName
}

func (p *process) Pid() uint32 {
	p.mu.Lock()
	pid := p.state.Pid
	p.mu.Unlock()
	return pid
}

func (p *execProcess) Kill(ctx context.Context, sig int, all bool) error {
	return p.systemd.KillUnitWithTarget(ctx, p.Name(), systemd.Main, int32(sig))
}

func (p *initProcess) Kill(ctx context.Context, sig int, all bool) error {
	who := systemd.Main
	if all {
		who = systemd.All
	}
	if p.Pid() == 0 {
		return fmt.Errorf("not started: %w", errdefs.ErrFailedPrecondition)
	}

	if p.ProcessState().Exited() {
		return errdefs.ErrNotFound
	}

	if err := p.systemd.KillUnitWithTarget(ctx, p.Name(), who, int32(sig)); err != nil {
		if strings.Contains(err.Error(), "no main process") {
			return errdefs.ErrNotFound
		}
		if _, err2 := p.runc.State(ctx, p.id); err2 != nil && strings.Contains(err2.Error(), "does not exist") {
			return fmt.Errorf("could not get runc state: %w", errdefs.ErrNotFound)
		}
		units, e := p.systemd.ListUnitsByNamesContext(ctx, []string{p.Name()})
		if err != nil {
			log.G(ctx).WithError(e).Errorf("Failed to list units")
		} else {
			if len(units) == 0 {
				return errdefs.ErrNotFound
			}
			for _, u := range units {
				if u.Name != p.Name() {
					continue
				}
				if u.ActiveState != "running" {
					return fmt.Errorf("not running: %w", errdefs.ErrFailedPrecondition)
				}
			}
		}
		return err
	}

	return nil
}

type initProcess struct {
	*process

	Bundle string
	Rootfs []*types.Mount

	// unitProps holds the transient-unit properties computed by Create (or
	// createRestore) and consumed by startUnit. It bridges the two because a
	// checkpoint restore builds them at Create but starts the unit later.
	unitProps []systemd.Property

	checkpoint       string
	parentCheckpoint string

	noNewNamespace bool
	killAllOnExit  bool

	execs *processManager

	shimLog io.Writer
}

func (p *initProcess) LogWriter() io.Writer {
	return p.shimLog
}

func (p *initProcess) pidFile() string {
	return filepath.Join(p.root, "init.pid")
}

// finishStart completes a successful Start: it finishes the lifecycle
// transition beginStart claimed, records the running state, and publishes the
// task's start. A workload that died while Start was in flight rejects the
// transition, but its start is still published -- containerd needs it before the
// exit the outbox is already holding.
func (p *initProcess) finishStart(ctx context.Context, pid uint32) {
	if _, err := p.advance(phaseRunning); err != nil {
		log.G(ctx).WithError(err).Debug("Container exited while it was starting")
	}
	p.SetState(ctx, pState{Pid: pid, Status: "running"})
	p.events.Send(ctx, &eventsapi.TaskStart{
		ContainerID: p.id,
		Pid:         pid,
	})
}

// finishDelete records that teardown finished -- waking waitForExit for a
// process that will never report an exit of its own -- and publishes the task's
// delete. Nothing may follow it.
func (p *initProcess) finishDelete(ctx context.Context) {
	deleted, err := p.advance(phaseDeleted)
	if err != nil {
		log.G(ctx).WithError(err).WithField("id", p.id).Debug("Ignoring delete of an already deleted container")
	}
	if !deleted {
		return
	}
	st := p.ProcessState()
	p.events.Send(ctx, &eventsapi.TaskDelete{
		ContainerID: p.id,
		Pid:         st.Pid,
		ExitStatus:  st.ExitCode,
		ExitedAt:    timestamppb.New(st.ExitedAt),
	})
}

func (p *initProcess) SetState(ctx context.Context, state pState) pState {
	u := p.applyState(ctx, state)
	st := u.state
	if u.justExited() {
		if st.Status != exitedInit && u.wasRunning() && p.killAllOnExit {
			if err := p.runc.Kill(ctx, p.id, int(syscall.SIGKILL), &runc.KillOpts{All: true}); err != nil {
				log.G(ctx).WithError(err).WithField("id", p.id).Error("failed to kill init's children")
			}
		}
		log.G(ctx).Debugf("EXITED: %s %s", p.Name(), st)
		p.execs.Each(func(exec Process) {
			if err := exec.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).WithField("exec", p.Name()).Info("Could not load exec state")
			}
			// An exec that never started has no process to reap. Leave it in its
			// Created state so callers see it never ran, rather than synthesizing
			// a non-zero exit for a process that was only ever set up.
			if ep, ok := exec.(*execProcess); ok && ep.currentPhase() < phaseRunning {
				return
			}
			if !exec.ProcessState().Exited() {
				exec.SetState(ctx, pState{ExitedAt: time.Now(), ExitCode: 255})
			}
		})
		p.cond.Broadcast()
		// If the init helper process exited, this should not yield a task exit event as the task never actually started.
		if st.Status != exitedInit {
			p.events.Send(ctx, &eventsapi.TaskExit{
				ContainerID: p.id,
				ID:          p.id,
				ExitStatus:  st.ExitCode,
				ExitedAt:    timestamppb.New(st.ExitedAt),
				Pid:         st.Pid,
			})
		}
	}
	return st
}

func (p *initProcess) Checkpoint(ctx context.Context, r *anypb.Any) error {
	var opts runc.CheckpointOpts
	var exit bool
	if r != nil {
		v, err := typeurl.UnmarshalAny(r)
		if err != nil {
			log.G(ctx).WithError(err).WithField("typeurl", r.TypeUrl).Debug("error unmarshalling *Any")
			return err
		}
		switch vv := v.(type) {
		case *v2runcopts.CheckpointOptions:
			exit = vv.Exit
			opts.AllowOpenTCP = vv.OpenTcp
			opts.AllowExternalUnixSockets = vv.ExternalUnixSockets
			opts.AllowTerminal = vv.Terminal
			opts.FileLocks = vv.FileLocks
			opts.EmptyNamespaces = vv.EmptyNamespaces
			opts.Cgroups = runc.CgroupMode(vv.CgroupsMode)
			opts.ImagePath = vv.ImagePath
			opts.WorkDir = vv.WorkPath
		default:
			return fmt.Errorf("unknown checkpoint options type: %w", errdefs.ErrInvalidArgument)
		}
	}

	if opts.WorkDir == "" {
		workDir := filepath.Join(p.root, "criu-work")
		if err := os.MkdirAll(workDir, 0700); err != nil {
			return fmt.Errorf("error making criu work dir: %w", err)
		}
		opts.WorkDir = workDir
	}

	var actions []runc.CheckpointAction
	if !exit {
		actions = append(actions, runc.LeaveRunning)
	}

	if err := p.runc.Checkpoint(ctx, p.id, &opts, actions...); err != nil {
		if p.runc.Debug {
			f, err2 := os.ReadFile(filepath.Join(opts.WorkDir, "dump.log"))
			if err2 == nil {
				err = fmt.Errorf("%w: %s", err, string(f))
			}
		}
		return err
	}
	return nil
}

func (p *initProcess) Pause(ctx context.Context) error {
	return p.runc.Pause(ctx, p.id)
}

func (p *initProcess) Resume(ctx context.Context) error {
	return p.runc.Resume(ctx, p.id)
}

func (p *initProcess) Pids(ctx context.Context) ([]*task.ProcessInfo, error) {
	ls, err := p.runc.Ps(ctx, p.id)
	if err != nil {
		return nil, err
	}

	procs := make([]*task.ProcessInfo, 0, len(ls))
	for _, p := range ls {
		procs = append(procs, &task.ProcessInfo{Pid: uint32(p)})
	}
	return procs, nil
}

func (p *initProcess) Update(ctx context.Context, res specs.LinuxResources) error {
	return p.runc.Update(ctx, p.id, &res)
}

type execProcess struct {
	*process
	Spec   *anypb.Any
	parent *initProcess
	execID string
}

func (p *execProcess) LogWriter() io.Writer {
	return p.parent.shimLog
}

func (p *execProcess) getPid(context.Context) (uint32, error) {
	data, err := os.ReadFile(p.pidFile())
	if err != nil {
		var state pState
		if stateErr := p.readExitState(&state); stateErr == nil && state.Pid > 0 &&
			(state.Status == "running" || state.Status == "exited") {
			// systemd can remove PIDFile before Start observes it. A running or
			// exited helper state proves runc created the workload.
			return state.Pid, nil
		}
		return 0, fmt.Errorf("read exec pid file: %w", err)
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse exec pid: %w", err)
	}
	if pid == 0 {
		return 0, fmt.Errorf("exec pid file contains zero")
	}
	return uint32(pid), nil
}

// finishStart mirrors initProcess.finishStart for an exec process.
func (p *execProcess) finishStart(ctx context.Context, pid uint32) {
	if _, err := p.advance(phaseRunning); err != nil {
		log.G(ctx).WithError(err).Debug("Exec exited while it was starting")
	}
	p.SetState(ctx, pState{Pid: pid, Status: "running"})
	p.events.Send(ctx, &eventsapi.TaskExecStarted{
		ContainerID: p.parent.id,
		ExecID:      p.execID,
		Pid:         pid,
	})
}

// finishDelete mirrors initProcess.finishDelete. An exec publishes no delete
// event -- containerd's runc shim does not either -- so this only ends the
// lifecycle.
func (p *execProcess) finishDelete(ctx context.Context) {
	if _, err := p.advance(phaseDeleted); err != nil {
		log.G(ctx).WithError(err).WithField("id", p.id).Debug("Ignoring delete of an already deleted exec")
	}
}

func (p *execProcess) SetState(ctx context.Context, state pState) pState {
	u := p.applyState(ctx, state)
	st := u.state
	if u.justExited() {
		p.cond.Broadcast()
		p.events.Send(ctx, &eventsapi.TaskExit{
			ContainerID: p.parent.id,
			ID:          p.execID,
			ExitStatus:  st.ExitCode,
			ExitedAt:    timestamppb.New(st.ExitedAt),
			Pid:         st.Pid,
		})
	}
	return st
}
