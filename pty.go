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
	"github.com/coreos/go-systemd/v22/daemon"
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

func ttyHandshake() (retErr error) {
	fmt.Fprintln(os.Stderr, "TTY handshake phase 1")
	defer func() {
		if retErr != nil {
			daemon.SdNotify(false, daemon.SdNotifyStopping)
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	sockPath := os.Getenv(ttySockPathEnv)
	if sockPath == "" {
		return fmt.Errorf("%s not set", ttySockPathEnv)
	}

	unix.Unlink(sockPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		return fmt.Errorf("error creating sock path parent dir: %w", err)
	}

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("error listening on sock path: %w", err)
	}

	daemon.SdNotify(false, daemon.SdNotifyReady)

	fmt.Fprintln(os.Stderr, "Waiting for init client")
	conn, err := l.Accept()
	if err != nil {
		return fmt.Errorf("error accepting connection to tty handler: %w", err)
	}
	defer conn.Close()

	fmt.Fprintln(os.Stderr, "Waiting for TTY handshake")
	console, err := recvFd(conn.(*net.UnixConn))
	if err != nil {
		return fmt.Errorf("error receiving console fd from tty handler: %w", err)
	}

	fmt.Fprintln(os.Stderr, "Received console fd")

	if console != 100 {
		err = unix.Dup2(console, 100)
		if err != nil {
			return fmt.Errorf("error copying console to fd 3: %w", err)
		}
		unix.Close(console)
	}

	os.Unsetenv(sockPath)
	if err := os.Setenv(ttyHandshakeEnv, "2"); err != nil {
		return fmt.Errorf("error setting %s: %w", ttyHandshakeEnv, err)
	}

	rc, err := l.(*net.UnixListener).SyscallConn()
	if err != nil {
		return fmt.Errorf("error getting raw connection: %w", err)
	}

	var dupErr error
	err = rc.Control(func(fd uintptr) {
		dupErr = unix.Dup2(int(fd), 101)
	})
	if err != nil {
		return fmt.Errorf("error controlling socket: %w", err)
	}
	if dupErr != nil {
		return fmt.Errorf("error duplicating socket fd: %w", dupErr)
	}

	fmt.Fprintln(os.Stderr, "Starting TTY copier")

	// We re-exec with ttyHandshakeEnv set to 2
	// The new process stays in C-land and does not let the go runtime spin-up.
	// This allows us to consume significantly less memory.
	//
	// We add extra flags just for the humans who see this in the process list.
	// The flags don't actually do anything.
	if err := unix.Exec(exe, []string{exe, "tty-handler", "--socket-path=" + sockPath}, os.Environ()); err != nil {
		return fmt.Errorf("error re-execing to handshake phase 2: %w", err)
	}
	return nil
}

func (p *process) ResizePTY(ctx context.Context, width, height int) error {
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

		s, err := p.ttySockPath()
		if err != nil {
			return fmt.Errorf("error getting tty sock path: %w", err)
		}
		conn, err = net.Dial("unix", s)
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
			return p.ResizePTY(ctx, width, height)
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

func (p *process) ttySockPath() (string, error) {
	sockInfoPath := filepath.Join(p.root, p.id+"-tty.sock")
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

func (p *process) makePty(ctx context.Context) (_, _ string, retErr error) {
	ctx, span := StartSpan(ctx, "process.StartTTY")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	sockPath, err := p.ttySockPath()
	if err != nil {
		return "", "", fmt.Errorf("error getting tty sock path: %w", err)
	}

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
	properties := []systemd.Property{
		systemd.PropType("notify"),
		systemd.PropExecStart([]string{p.exe, "tty-handshake"}, false),
		systemd.PropDescription("TTY Handshake for " + p.Name()),
		{Name: "Environment", Value: dbus.MakeVariant([]string{
			ttyHandshakeEnv + "=1",
			ttySockPathEnv + "=" + sockPath,
		})},
		{Name: "StandardInputFile", Value: dbus.MakeVariant(p.Stdin)},
		{Name: "StandardOutputFile", Value: dbus.MakeVariant(p.Stdout)},
		{Name: "StandardErrorFile", Value: dbus.MakeVariant(logPath)},
	}

	ttyUnit := unitName(p.ns, p.id+"-tty")
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
