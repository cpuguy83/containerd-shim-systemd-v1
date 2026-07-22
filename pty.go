package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	ttySockPathEnv  = "_TTY_SOCKET_PATH"
	ttyHandshakeEnv = "_TTY_HANDSHAKE"
)

// The tty handler protocol is a small newline-framed, text request/response
// exchanged over a persistent unix socket. A request is "<op> <width> <height>"
// and a response is a single status line: "0" on success or "1 <message>" on
// error. These constants are mirrored by op_resize/reply_* in pty.c.
const (
	ttyOpResize  = 1
	ttyStatusOK  = "0"
	ttyStatusErr = "1"

	// maxTTYResponseLen bounds how many bytes we buffer for a single response
	// line so a misbehaving handler cannot make us read without limit. It
	// comfortably exceeds the longest reply pty.c can produce.
	maxTTYResponseLen = 256

	// ttyRequestBufSize sizes the reusable request buffer. A resize request is
	// "<op> <width> <height>\n"; width and height arrive as uint32 (at most 10
	// digits each), so the longest possible request, "1 4294967295 4294967295\n",
	// is 24 bytes. Rounded up for headroom.
	ttyRequestBufSize = 32

	// ttyResponseTimeout bounds a single resize exchange. The read runs while the
	// process lock is held, so without a deadline a stuck handler -- or an old,
	// pre-framing handler that writes a bare "0" and never sends the newline this
	// client now waits for -- would hang the whole lifecycle. The timeout is
	// generous: a healthy handler replies in microseconds.
	ttyResponseTimeout = 2 * time.Second
)

// ttyControlClient sends control operations to a container's tty handler over
// its unix control socket. The handler speaks the small newline-framed
// request/response protocol described above; this client hides that behind
// Resize and Close, dialing lazily and reusing the connection across resizes.
// Its own mutex serializes exchanges, so tty I/O never blocks the process
// lifecycle lock.
type ttyControlClient struct {
	sockPath string

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	limit  *io.LimitedReader
	// buf is a reusable scratch buffer for encoding each request, so a resize
	// does not allocate.
	buf [ttyRequestBufSize]byte
}

func newTTYControlClient(sockPath string) *ttyControlClient {
	return &ttyControlClient{sockPath: sockPath}
}

// Resize sets the tty window size. It dials the handler on first use and reuses
// the connection thereafter; if the exchange fails it drops the connection so
// the next resize redials.
func (c *ttyControlClient) Resize(ctx context.Context, width, height int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.dial(); err != nil {
			return err
		}
	}

	line, err := c.exchange(ctx, appendTTYResizeRequest(c.buf[:0], width, height))
	if err != nil {
		// The handler is a transient local unit: a failed exchange means it is
		// gone, not that a retry would help, so just drop the connection and let
		// the next resize redial.
		c.reset()
		return err
	}
	return parseTTYResponse(line)
}

// Close closes the control connection. It is safe to call more than once and
// concurrently with Resize. The caller must not hold c.mu.
func (c *ttyControlClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reset()
}

// dial establishes the control connection and its bounded reader. The caller
// holds c.mu.
func (c *ttyControlClient) dial() error {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return fmt.Errorf("could not dial tty sock: %w", err)
	}
	c.conn = conn
	c.limit = &io.LimitedReader{R: conn}
	c.reader = bufio.NewReader(c.limit)
	return nil
}

// reset closes and clears the control connection, returning the close error.
// The caller holds c.mu.
func (c *ttyControlClient) reset() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.reader, c.limit = nil, nil, nil
	return err
}

// exchange writes req and reads one response line, bounded by ttyResponseTimeout
// (or an earlier context deadline). The caller holds c.mu.
func (c *ttyControlClient) exchange(ctx context.Context, req []byte) (string, error) {
	deadline := time.Now().Add(ttyResponseTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("error setting tty handler deadline: %w", err)
	}

	if _, err := c.conn.Write(req); err != nil {
		return "", fmt.Errorf("error writing winsize to the tty handler: %w", err)
	}

	line, err := c.readMsg()
	if err != nil {
		return "", fmt.Errorf("error reading response from tty handler: %w", err)
	}
	return line, nil
}

// readMsg reads one status line from the handler, bounding the read at
// maxTTYResponseLen bytes via the reusable limit. A well-behaved handler ends
// the line with a newline. An older, pre-framing handler writes an unframed
// reply (e.g. a bare "0") then waits for the next request, so no newline
// arrives; the read then ends by deadline or EOF with the message buffered, and
// we return it rather than hanging or reporting a failure. The caller holds c.mu.
func (c *ttyControlClient) readMsg() (string, error) {
	c.limit.N = maxTTYResponseLen
	line, err := c.reader.ReadString('\n')
	if err != nil {
		if c.limit.N <= 0 {
			return "", fmt.Errorf("tty handler response exceeded %d bytes without a newline", maxTTYResponseLen)
		}
		if len(line) == 0 {
			return "", err
		}
		// Unframed reply: the message is buffered but no newline arrived.
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// appendTTYResizeRequest appends a newline-terminated resize request to buf and
// returns the extended buffer, so a caller can reuse a single buffer across
// resizes instead of allocating one per call.
func appendTTYResizeRequest(buf []byte, width, height int) []byte {
	buf = strconv.AppendInt(buf, ttyOpResize, 10)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(width), 10)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(height), 10)
	return append(buf, '\n')
}

// parseTTYResponse interprets a single status line from the tty handler.
func parseTTYResponse(line string) error {
	line = strings.TrimRight(line, "\r\n")
	switch {
	case line == ttyStatusOK:
		return nil
	case line == ttyStatusErr:
		return fmt.Errorf("tty handler returned an unspecified error")
	case strings.HasPrefix(line, ttyStatusErr+" "):
		return fmt.Errorf("tty handler returned an error: %s", strings.TrimPrefix(line, ttyStatusErr+" "))
	default:
		return fmt.Errorf("tty handler returned an unexpected response: %q", line)
	}
}

func (p *process) ResizePTY(ctx context.Context, width, height int, sockPath string) error {
	if !p.Terminal {
		// This mimics what the runc shim does, and what the containerd integration tests expect
		return nil
	}

	p.mu.Lock()
	if p.ttyControl == nil {
		p.ttyControl = newTTYControlClient(sockPath)
	}
	c := p.ttyControl
	p.mu.Unlock()

	return c.Resize(ctx, width, height)
}

// closeTTYControl detaches and closes the process's tty control client, if any.
// It holds p.mu only to detach the pointer, never across the close, so a slow or
// stuck resize cannot wedge the lifecycle lock.
func (p *process) closeTTYControl() {
	p.mu.Lock()
	c := p.ttyControl
	p.ttyControl = nil
	p.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// ResizePty of a process
func (s *Service) ResizePty(ctx context.Context, r *taskapi.ResizePtyRequest) (_ *emptypb.Empty, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"id":        r.ID,
		"ns":        ns,
		"apiAction": "resizePty",
	}))

	log.G(ctx).Debug("systemd.ResizePTY start")
	defer func() {
		log.G(ctx).WithError(retErr).Debug("systemd.ResizePTY end")
		if retErr != nil {
			retErr = errgrpc.ToGRPC(retErr)
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

	return &emptypb.Empty{}, nil
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
func (s *Service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (_ *emptypb.Empty, retErr error) {
	// TODO: I'm not sure what we should do here since we aren't really doing anything with container I/O
	// Potentially we should signal the tty handler to stop copying stdio?
	return &emptypb.Empty{}, nil
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

	tmp, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "pty")
	if err != nil {
		return "", err
	}
	s := filepath.Join(tmp, "s")
	if err := os.WriteFile(sockInfoPath, []byte(s), 0600); err != nil {
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

	tmp, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "pty")
	if err != nil {
		return "", err
	}
	s := filepath.Join(tmp, "s")
	if err := os.WriteFile(sockInfoPath, []byte(s), 0600); err != nil {
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
			logData, err := os.ReadFile(logPath)
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
	properties := p.ttyServiceProperties(sockPath, logPath)

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

func (p *process) ttyServiceProperties(sockPath, logPath string) []systemd.Property {
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
		{Name: "StandardErrorFile", Value: dbus.MakeVariant(logPath)},
	}
	if p.Stdin != "" {
		properties = append(properties, systemd.Property{Name: "StandardInputFile", Value: dbus.MakeVariant(p.Stdin)})
	}
	if p.Stdout != "" {
		properties = append(properties, systemd.Property{Name: "StandardOutputFile", Value: dbus.MakeVariant(p.Stdout)})
	}
	return properties
}
