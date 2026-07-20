package main

import (
	"context"
	"fmt"
	"io"
	"net"
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
	ns      string
	id      string
	root    string
	unitDir string

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
	ttyConn net.Conn

	mu           sync.Mutex
	cond         *sync.Cond
	state        pState
	deleted      bool
	exitNotified bool
	started      bool

	eventMu             sync.Mutex
	startEventPublished bool
	pendingExitEvent    func()

	systemdExitState    pState
	hasSystemdExitState bool

	shimCgroup string
}

// claimExitNotification returns true for exactly one caller: the first to reach
// it. Every state path (the D-Bus event reconciler, the reconnect resync, and the
// start/delete helpers) funnels an exit through SetState, but the resulting
// TaskExit must be emitted only once no matter how many of them observe the exit
// concurrently. Callers gate emission on st.Exited() && p.claimExitNotification().
func (p *process) claimExitNotification() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exitNotified {
		return false
	}
	p.exitNotified = true
	return true
}

func (p *process) sendExitAfterStart(send func()) {
	p.eventMu.Lock()
	if !p.startEventPublished {
		p.pendingExitEvent = send
		p.eventMu.Unlock()
		return
	}
	p.eventMu.Unlock()
	send()
}

func (p *process) markStartEventPublished() {
	p.eventMu.Lock()
	p.startEventPublished = true
	send := p.pendingExitEvent
	p.pendingExitEvent = nil
	p.eventMu.Unlock()
	if send != nil {
		send()
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

func (p *process) SetState(ctx context.Context, state pState) pState {
	p.mu.Lock()
	if !p.started && toStatus(state.Status) == task.Status_RUNNING {
		state.Status = "created"
	}
	state.CopyTo(&p.state)
	if p.state.ExitCode > 0 && !p.state.Exited() {
		p.state.ExitedAt = time.Now()
	}
	p.state.CopyTo(&state)
	p.cond.Broadcast()
	p.mu.Unlock()
	return state
}

func (p *process) markStarted() {
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
}

func (p *process) hasStarted() bool {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	return started
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

func (p *initProcess) unitPath() string {
	return filepath.Join(p.unitDir, p.Name())
}

func (p *execProcess) unitPath() string {
	return filepath.Join(p.unitDir, p.Name())
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
		if _, err2 := p.runcForBundle().State(ctx, p.id); err2 != nil && strings.Contains(err2.Error(), "does not exist") {
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

	checkpoint       string
	parentCheckpoint string

	noNewNamespace bool
	killAllOnExit  bool

	execs *processManager

	sendEvent func(ctx context.Context, ns string, evt interface{})
	shimLog   io.Writer
}

func (p *initProcess) LogWriter() io.Writer {
	return p.shimLog
}

func (p *initProcess) pidFile() string {
	return filepath.Join(p.root, "init.pid")
}

func (p *initProcess) SetState(ctx context.Context, state pState) pState {
	st := p.process.SetState(ctx, state)
	if st.Exited() && p.claimExitNotification() {
		if st.Status != exitedInit && p.hasStarted() && p.killAllOnExit {
			if err := p.runcForBundle().Kill(ctx, p.id, int(syscall.SIGKILL), &runc.KillOpts{All: true}); err != nil {
				log.G(ctx).WithError(err).WithField("id", p.id).Error("failed to kill init's children")
			}
		}
		log.G(ctx).Debugf("EXITED: %s %s", p.Name(), st)
		p.execs.Each(func(exec Process) {
			if err := exec.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).WithField("exec", p.Name()).Info("Could not load exec state")
			}
			if !exec.ProcessState().Exited() {
				exec.SetState(ctx, pState{ExitedAt: time.Now(), ExitCode: 255})
			}
		})
		p.cond.Broadcast()
		// If the init helper process exited, this should not yield a task exit event as the task never actually started.
		if st.Status != exitedInit {
			p.sendExitAfterStart(func() {
				p.sendEvent(ctx, p.ns, &eventsapi.TaskExit{
					ContainerID: p.id,
					ID:          p.id,
					ExitStatus:  st.ExitCode,
					ExitedAt:    timestamppb.New(st.ExitedAt),
					Pid:         st.Pid,
				})
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

	if err := p.runcForBundle().Checkpoint(ctx, p.id, &opts, actions...); err != nil {
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
	return p.runcForBundle().Pause(ctx, p.id)
}

func (p *initProcess) Resume(ctx context.Context) error {
	return p.runcForBundle().Resume(ctx, p.id)
}

func (p *initProcess) Pids(ctx context.Context) ([]*task.ProcessInfo, error) {
	ls, err := p.runcForBundle().Ps(ctx, p.id)
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
	return p.runcForBundle().Update(ctx, p.id, &res)
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

func (p *execProcess) SetState(ctx context.Context, state pState) pState {
	st := p.process.SetState(ctx, state)
	if st.Exited() && p.claimExitNotification() {
		p.cond.Broadcast()
		p.sendExitAfterStart(func() {
			p.parent.sendEvent(ctx, p.ns, &eventsapi.TaskExit{
				ContainerID: p.parent.id,
				ID:          p.execID,
				ExitStatus:  st.ExitCode,
				ExitedAt:    timestamppb.New(st.ExitedAt),
				Pid:         st.Pid,
			})
		})
	}
	return st
}
