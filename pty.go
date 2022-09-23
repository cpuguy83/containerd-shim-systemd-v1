package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sys/unix"
)

const (
	ttySockPathEnv  = "_TTY_SOCKET_PATH"
	ttyHandshakeEnv = "_TTY_HANDSHAKE"
)

func (p *process) ResizePTY(ctx context.Context, width, height int, sockPath string) error {
	if !p.Terminal {
		// This mimics what the runc shim does, and what the containerd integration tests expect
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	conn := p.ttyConn

	var noRetry bool
	if conn == nil {
		noRetry = true
		var err error
		conn, err = net.Dial("unix", sockPath)
		if err != nil {
			return fmt.Errorf("could not dial tty sock: %w", err)
		}
		p.ttyConn = conn
	}

	_, err := conn.Write([]byte("1 " + strconv.Itoa(width) + " " + strconv.Itoa(height)))
	if err != nil {
		if !noRetry {
			p.ttyConn.Close()
			p.ttyConn = nil
			return p.ResizePTY(ctx, width, height, sockPath)
		}
		return fmt.Errorf("error writing winsize to the tty handler: %w", err)
	}

	resp := make([]byte, 128)
	n, err := conn.Read(resp)
	if err != nil {
		return fmt.Errorf("error reading ack from tty handler: %w", err)
	}
	if n > 1 {
		return fmt.Errorf("tty handler returned an error: %s", string(resp[:n]))
	}
	if n == 0 {
		return fmt.Errorf("tty handler returned no data")
	}
	if resp[0] != '0' {
		return fmt.Errorf("tty handler returned unknown response code %s", string(resp[:n]))
	}
	return nil
}

// ResizePty of a process
func (s *Service) ResizePty(ctx context.Context, r *taskapi.ResizePtyRequest) (_ *ptypes.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"id":        r.ID,
		"ns":        ns,
		"apiAction": "resizePty",
	}))

	log.G(ctx).Info("systemd.ResizePTY start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.ResizePTY end")
		if retErr != nil {
			retErr = errdefs.ToGRPC(retErr)
		}
	}()

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("process %s: %w", r.ID, errdefs.ErrNotFound)
	}

	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if err := ep.ResizePTY(ctx, int(r.Width), int(r.Height)); err != nil {
			return nil, err
		}
	} else {
		if err := p.ResizePTY(ctx, int(r.Width), int(r.Height)); err != nil {
			return nil, err
		}
	}

	return &ptypes.Empty{}, nil
}

func (p *execProcess) ResizePTY(ctx context.Context, w, h int) error {
	ttyPath, err := p.ttySockPath()
	if err != nil {
		return err
	}
	return p.process.ResizePTY(ctx, w, h, ttyPath)
}

func (p *initProcess) ResizePTY(ctx context.Context, w, h int) error {
	ttyPath, err := p.ttySockPath()
	if err != nil {
		return err
	}
	return p.process.ResizePTY(ctx, w, h, ttyPath)
}

// CloseIO of a process
func (s *Service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (_ *ptypes.Empty, retErr error) {
	// TODO: I'm not sure what we should do here since we aren't really doing anything with container I/O
	// Potentially we should signal the tty handler to stop copying stdio?
	return &ptypes.Empty{}, nil
}

// This is pretty standard stuff but I copied this from github.com/containerd/go-runc, with some minor changes.
// This receives the pty master fd from runc.
func recvFd(socket *net.UnixConn) (int, error) {
	const MaxNameLen = 4096
	var oobSpace = unix.CmsgSpace(4)

	name := make([]byte, MaxNameLen)
	oob := make([]byte, oobSpace)

	n, oobn, _, _, err := socket.ReadMsgUnix(name, oob)
	if err != nil {
		return -1, err
	}

	if n >= MaxNameLen || oobn != oobSpace {
		return -1, fmt.Errorf("recvfd: incorrect number of bytes read (n=%d oobn=%d)", n, oobn)
	}

	// Truncate.
	name = name[:n]
	oob = oob[:oobn]

	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, err
	}
	if len(scms) != 1 {
		return -1, fmt.Errorf("recvfd: number of SCMs is not 1: %d", len(scms))
	}
	scm := scms[0]

	fds, err := unix.ParseUnixRights(&scm)
	if err != nil {
		return -1, err
	}
	if len(fds) != 1 {
		return -1, fmt.Errorf("recvfd: number of fds is not 1: %d", len(fds))
	}
	return fds[0], nil
}

func (p *initProcess) ttySockPath() (string, error) {
	sockInfoPath := filepath.Join(p.root, "tty.sock")
	b, err := os.ReadFile(sockInfoPath)
	if err == nil {
		return string(b), nil
	}

	if !os.IsNotExist(err) {
		return "", err
	}

	tmp, err := ioutil.TempDir(os.Getenv("XDG_RUNTIME_DIR"), "pty")
	if err != nil {
		return "", err
	}
	s := filepath.Join(tmp, "s")
	if err := ioutil.WriteFile(sockInfoPath, []byte(s), 0600); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	return s, nil
}

func (p *execProcess) ttySockPath() (string, error) {
	sockInfoPath := filepath.Join(p.stateDir(), "tty.sock")
	b, err := os.ReadFile(sockInfoPath)
	if err == nil {
		return string(b), nil
	}

	if !os.IsNotExist(err) {
		return "", err
	}

	tmp, err := ioutil.TempDir(os.Getenv("XDG_RUNTIME_DIR"), "pty")
	if err != nil {
		return "", err
	}
	s := filepath.Join(tmp, "s")
	if err := ioutil.WriteFile(sockInfoPath, []byte(s), 0600); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	return s, nil
}

func (p *process) ttyUnitName() string {
	return unitName(p.ns, p.id, "tty")
}

func (p *process) makePty(ctx context.Context, sockPath string) (_, _ string, retErr error) {
	ctx, span := StartSpan(ctx, "process.StartTTY")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	logPath := filepath.Join(p.root, p.id+"-tty.log")
	defer func() {
		if retErr != nil {
			logData, err := ioutil.ReadFile(logPath)
			if err != nil {
				log.G(ctx).WithError(err).Info("Could not read tty error log")
			} else {
				retErr = fmt.Errorf("%w: %s", retErr, string(logData))
			}
		}
	}()

	if p.Stdin != "" {
		f, _ := os.OpenFile(p.Stdin, os.O_RDWR, 0)
		if f != nil {
			defer f.Close()
		}
	}

	if p.Stdout != "" {
		f, _ := os.OpenFile(p.Stdout, os.O_RDWR, 0)
		if f != nil {
			defer f.Close()
		}
	}

	if p.Stderr != "" {
		f, _ := os.OpenFile(p.Stderr, os.O_RDWR, 0)
		if f != nil {
			defer f.Close()
		}
	}
	env := []string{
		ttyHandshakeEnv + "=1",
		ttySockPathEnv + "=" + sockPath,
	}
	if p.shimCgroup != "" {
		env = append(env, "SHIM_CGROUP="+p.shimCgroup)
	}

	properties := []systemd.Property{
		systemd.PropType("notify"),
		systemd.PropExecStart([]string{p.exe, "tty-handshake"}, false),
		{Name: "Environment", Value: dbus.MakeVariant(env)},
		{Name: "StandardInputFile", Value: dbus.MakeVariant(p.Stdin)},
		{Name: "StandardOutputFile", Value: dbus.MakeVariant(p.Stdout)},
		{Name: "StandardErrorFile", Value: dbus.MakeVariant(logPath)},
	}

	ttyUnit := p.ttyUnitName()
	defer func() {
		if retErr != nil {
			p.systemd.StopUnitContext(ctx, ttyUnit, "replace", nil)
		}
	}()

	chTTY := make(chan string, 1)
	if _, err := p.systemd.StartTransientUnitContext(ctx, ttyUnit, "replace", properties, chTTY); err != nil {
		if e := p.systemd.ResetFailedUnitContext(ctx, ttyUnit); e == nil {
			_, err2 := p.systemd.StartTransientUnitContext(ctx, ttyUnit, "replace", properties, chTTY)
			if err2 == nil {
				err = nil
			}
		} else {
			log.G(ctx).WithField("unit", ttyUnit).WithError(e).Warn("Error reseting failed unit")
		}
		if err != nil {
			return "", "", fmt.Errorf("error starting tty service: %w", err)
		}
	}
	select {
	case <-ctx.Done():
	case status := <-chTTY:
		span.SetAttributes(attribute.String("tty-unit-status", status))
		if status != "done" {
			return "", "", fmt.Errorf("failed to start tty service: %s", status)
		}
	}

	return ttyUnit, sockPath, nil
}
