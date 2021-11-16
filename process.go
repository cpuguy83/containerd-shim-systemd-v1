package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	ptypes "github.com/gogo/protobuf/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type processManager struct {
	mu sync.Mutex
	ls map[string]Process
}

func newUnitManager(conn *systemd.Conn) *unitManager {
	um := &unitManager{idx: make(map[string]Process), sd: conn}
	um.cond = sync.NewCond(&um.mu)
	return um
}

type unitManager struct {
	sd   *systemd.Conn
	mu   sync.Mutex
	cond *sync.Cond
	idx  map[string]Process
}

func (m *unitManager) Add(p Process) {
	m.mu.Lock()
	m.idx[p.Name()] = p
	m.cond.Signal()
	m.mu.Unlock()
}

func (m *unitManager) Delete(p Process) {
	m.mu.Lock()
	delete(m.idx, p.Name())
	m.mu.Unlock()
	log.G(context.TODO()).Debugf("deleted unit %s", p.Name())
}

func (m *unitManager) Get(name string) Process {
	m.mu.Lock()
	p := m.idx[name]
	m.mu.Unlock()
	return p
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
	log.G(context.TODO()).Debugf("deleted process %s", id)
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
	SetState(context.Context, pState) pState
	ProcessState() pState
}

type CreateOptions struct {
	// Native config
	LogMode        string
	SdNotifyEnable bool

	// From runc types
	BinaryName          string
	Root                string
	CriuPath            string
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

	exe string

	Stdin    string
	Stdout   string
	Stderr   string
	Terminal bool

	opts CreateOptions

	systemd *systemd.Conn
	runc    *runc.Runc
	ttyConn net.Conn

	mu      sync.Mutex
	cond    *sync.Cond
	state   pState
	deleted bool
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
	state.CopyTo(&p.state)
	if p.state.Pid > 0 && p.state.Status == "dead" && !p.state.Exited() {
		p.state.ExitedAt = time.Now()
	}
	p.state.CopyTo(&state)
	p.cond.Broadcast()
	p.mu.Unlock()
	return state
}

func (p *process) Name() string {
	return unitName(p.ns, p.id)
}

func (p *process) Pid() uint32 {
	p.mu.Lock()
	pid := p.state.Pid
	p.mu.Unlock()
	return pid
}

func (p *process) runcCmd(cmd []string) ([]string, error) {
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	root := []string{runcPath, "--debug=" + strconv.FormatBool(p.runc.Debug), "--systemd-cgroup=" + strconv.FormatBool(p.opts.SystemdCgroup), "--root", p.runc.Root}
	if p.runc.Debug {
		root = append(root, "--log="+filepath.Join(p.root, p.id+"-runc-debug.log"))
	}

	return append(root, cmd...), nil
}

func (p *process) Kill(ctx context.Context, sig int, all bool) error {
	who := systemd.Main
	if all {
		who = systemd.All
	}
	if p.Pid() == 0 {
		return fmt.Errorf("not started: %w", errdefs.ErrFailedPrecondition)
	}
	if err := p.systemd.KillUnitWithTarget(ctx, unitName(p.ns, p.id), who, int32(sig)); err != nil {
		units, e := p.systemd.ListUnitsByNamesContext(ctx, []string{p.Name()})
		if err != nil {
			log.G(ctx).WithError(e).Errorf("Failed to list units")
		} else {
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

	execs *processManager

	sendEvent func(ctx context.Context, ns string, evt interface{})
}

func (p *initProcess) Start(ctx context.Context) (pid uint32, retErr error) {
	ctx, span := StartSpan(ctx, "InitProcess.Start")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(attribute.Int("pid", int(pid)))
		span.End()
	}()

	if p.checkpoint != "" {
		return p.restore(ctx)
	}
	if err := p.runc.Start(ctx, runcName(p.ns, p.id)); err != nil {
		return 0, fmt.Errorf("runc start: %w", err)
	}
	p.mu.Lock()
	pid = p.state.Pid
	p.mu.Unlock()
	return pid, nil
}

func (p *initProcess) restore(ctx context.Context) (pid uint32, retErr error) {
	execStart := []string{
		"restore",
		"--image-path=" + p.checkpoint,
		"--work-path=" + p.root,
		"--bundle=" + p.Bundle,
		"--no-pivot=" + strconv.FormatBool(p.opts.NoPivotRoot),
		"--no-subreaper",
	}

	if p.Terminal || p.opts.Terminal {
		execStart = append(execStart, "--detach")
	}

	execStart = append(execStart, p.opts.RestoreArgs()...)
	return p.startUnit(ctx, nil, execStart, filepath.Join(p.root, "pid"), runcName(p.ns, p.id), nil)
}

func (p *initProcess) SetState(ctx context.Context, state pState) pState {
	st := p.process.SetState(ctx, state)
	if st.Exited() {
		p.sendEvent(ctx, p.ns, &eventsapi.TaskExit{
			ContainerID: p.id,
			ID:          p.id,
			ExitStatus:  st.ExitCode,
			ExitedAt:    st.ExitedAt,
			Pid:         st.Pid,
		})
	}
	return st
}

type execProcess struct {
	*process
	Spec   *ptypes.Any
	parent *initProcess
	execID string
}

func (p *execProcess) Start(ctx context.Context) (pid uint32, retErr error) {
	ctx, span := StartSpan(ctx, "ExecProcess.Start")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(attribute.Int("pid", int(pid)))
		span.End()
	}()

	f, err := ioutil.TempFile(os.Getenv("XDG_RUNTIME_DIR"), p.id+"process-json")
	if err != nil {
		return 0, fmt.Errorf("error creating process.json file: %w", err)
	}

	if _, err := f.Write(p.Spec.Value); err != nil {
		return 0, fmt.Errorf("error writing process.json: %w", err)
	}
	f.Close()

	execStart := []string{"exec", "--process", f.Name(), "-d"}

	pidFile := filepath.Join(p.root, p.id+"-pid")
	if p.Terminal {
		execStart = append(execStart, "-t")
	}

	pid, err = p.startUnit(ctx, nil, execStart, pidFile, runcName(p.ns, p.parent.id), nil)
	if err != nil {
		if _, err := p.Delete(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("Error cleaning up after failed exec start")
		}
		return 0, err
	}
	return pid, nil
}

func (p *execProcess) SetState(ctx context.Context, state pState) pState {
	st := p.process.SetState(ctx, state)
	if st.Exited() {
		p.parent.sendEvent(ctx, p.ns, &eventsapi.TaskExit{
			ContainerID: p.parent.id,
			ID:          p.execID,
			ExitStatus:  st.ExitCode,
			ExitedAt:    st.ExitedAt,
			Pid:         st.Pid,
		})
	}
	return st
}
