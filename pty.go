package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/coreos/go-systemd/v22/daemon"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	ttySockPathEnv  = "_TTY_SOCKET_PATH"
	ttyHandshakeEnv = "_TTY_HANDSHAKE"
)

func ttyHandshake() error {
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

	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		return fmt.Errorf("error notifying that the tty handler is ready")
	}

	conn, err := l.Accept()
	if err != nil {
		return fmt.Errorf("error accepting connection to tty handler: %w", err)
	}
	defer conn.Close()

	console, err := recvFd(conn.(*net.UnixConn))
	if err != nil {
		return fmt.Errorf("error receiving console fd from tty handler: %w", err)
	}

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

func ttySockPath(root, ns, id, execID string) string {
	if execID != "" {
		return filepath.Join(root, ns, id, "tty-"+execID+".sock")
	}
	return filepath.Join(root, ns, id, "tty.sock")
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

	log.G(ctx).Info("start")
	defer func() {
		log.G(ctx).WithError(err).Info("end")
	}()

	conn, err := s.getTTY(ctx, ns, r.ID, r.ExecID)
	if err != nil {
		return nil, err
	}

	if err := s.doResizePty(ctx, conn, r.Width, r.Height); err != nil {
		sp := ttySockPath(s.root, ns, r.ID, r.ExecID)
		s.ttyMu.Lock(sp)
		delete(s.ttys, sp)
		s.ttyMu.Unlock(sp)

		conn, err = s.getTTY(ctx, ns, r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}

		if err := s.doResizePty(ctx, conn, r.Width, r.Height); err != nil {
			return nil, err
		}
	}

	return &ptypes.Empty{}, nil
}

func (s *Service) doResizePty(ctx context.Context, conn net.Conn, width, height uint32) error {
	_, err := conn.Write([]byte("1 " + strconv.Itoa(int(width)) + " " + strconv.Itoa(int(height))))
	if err != nil {
		return fmt.Errorf("error writing winsize to tty handler: %w", err)
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

func (s *Service) getTTY(ctx context.Context, ns, id, execID string) (net.Conn, error) {
	sockPath := ttySockPath(s.root, ns, id, execID)

	s.ttyMu.Lock(sockPath)
	defer s.ttyMu.Unlock(sockPath)

	if conn, ok := s.ttys[sockPath]; ok {
		return conn, nil
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("error getting connection to tty handler: %s", sockPath)
	}

	s.ttys[sockPath] = conn
	return conn, nil
}

// CloseIO of a process
func (s *Service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (_ *ptypes.Empty, retErr error) {
	return nil, errdefs.ErrNotImplemented
}

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
