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

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	ptypes "github.com/gogo/protobuf/types"
)

type processManager struct {
	mu sync.Mutex
	ls map[string]Process
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

type Process interface {
	Start(context.Context) (uint32, error)
	ResizePTY(ctx context.Context, w, h int) error
	Wait(context.Context) (*pState, error)
	Delete(context.Context) (*pState, error)
	State(ctx context.Context) (*State, error)
	Kill(context.Context, int, bool) error
	Pid() uint32
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

	opts options.CreateOptions

	systemd *systemd.Conn
	runc    *runc.Runc
	ttyConn net.Conn

	mu         sync.Mutex
	cond       *sync.Cond
	pid        uint32
	exitStatus int
	status     int
}

func (p *process) Name() string {
	return p.ns + "-" + p.id
}

func (p *process) Pid() uint32 {
	return p.pid
}

func (p *process) runcCmd(cmd []string) ([]string, error) {
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	return append([]string{runcPath, "--debug=" + strconv.FormatBool(p.runc.Debug), "--systemd-cgroup=" + strconv.FormatBool(p.runc.SystemdCgroup), "--root", p.runc.Root}, cmd...), nil
}

func (p *process) Kill(ctx context.Context, sig int, all bool) error {
	who := systemd.Main
	if all {
		who = systemd.All
	}
	return p.systemd.KillUnitWithTarget(ctx, unitName(p.ns, p.id)+".service", who, int32(sig))
}

type initProcess struct {
	*process

	Bundle string
	Rootfs []*types.Mount

	checkpoint       string
	parentCheckpoint string

	execs *processManager
}

func (p *initProcess) Start(ctx context.Context) (uint32, error) {
	if err := p.runc.Start(ctx, runcName(p.ns, p.id)); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid, nil
}

type execProcess struct {
	*process
	Spec   *ptypes.Any
	parent *initProcess
	execID string
}

func (p *execProcess) Start(ctx context.Context) (uint32, error) {
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

	return p.startUnit(ctx, execStart, pidFile, runcName(p.ns, p.parent.id))
}
