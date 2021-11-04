package main

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"sync"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
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
	Create(context.Context) (uint32, error)
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
}

func (p *initProcess) Start(ctx context.Context) (uint32, error) {
	if err := p.runc.Start(ctx, runcName(p.ns, p.id)); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid, nil
}
