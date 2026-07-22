package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

func TestTTYControlClientReadMsg(t *testing.T) {
	t.Run("a newline-terminated line is returned without the newline", func(t *testing.T) {
		got, err := ttyClientReading("1 80 24\n").readMsg()
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if got != "1 80 24" {
			t.Fatalf("got %q, want %q", got, "1 80 24")
		}
	})

	t.Run("only the first line is consumed from a stream of many", func(t *testing.T) {
		got, err := ttyClientReading("0\n0\n").readMsg()
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if got != "0" {
			t.Fatalf("got %q, want %q", got, "0")
		}
	})

	t.Run("an unframed reply ending without a newline is accepted", func(t *testing.T) {
		// An older, pre-framing handler writes a bare "0" and no newline; the
		// read ends at EOF (or, over a real socket, the deadline) with the reply
		// buffered. It must be accepted rather than reported as a failure.
		got, err := ttyClientReading("0").readMsg()
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if got != "0" {
			t.Fatalf("got %q, want %q", got, "0")
		}
	})

	t.Run("a closed connection with no reply is an error", func(t *testing.T) {
		if _, err := ttyClientReading("").readMsg(); err == nil {
			t.Fatal("got nil error, want one")
		}
	})

	t.Run("a line longer than the cap without a newline is rejected", func(t *testing.T) {
		_, err := ttyClientReading(strings.Repeat("x", maxTTYResponseLen+10)).readMsg()
		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if !strings.Contains(err.Error(), "without a newline") {
			t.Fatalf("error %q does not report the missing newline", err)
		}
	})
}

func TestParseTTYResponse(t *testing.T) {
	t.Run("a bare ok status is success", func(t *testing.T) {
		if err := parseTTYResponse("0\n"); err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
	})

	t.Run("an ok status with no trailing newline is success", func(t *testing.T) {
		if err := parseTTYResponse("0"); err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
	})

	t.Run("an error status carries the handler message", func(t *testing.T) {
		err := parseTTYResponse("1 error setting window size\n")
		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if !strings.Contains(err.Error(), "error setting window size") {
			t.Fatalf("error %q does not contain the handler message", err)
		}
	})

	t.Run("a bare error status reports an unspecified error", func(t *testing.T) {
		err := parseTTYResponse("1\n")
		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if !strings.Contains(err.Error(), "unspecified") {
			t.Fatalf("error %q does not mention it is unspecified", err)
		}
	})

	t.Run("an unrecognized status is surfaced as unexpected", func(t *testing.T) {
		err := parseTTYResponse("garbage\n")
		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("error %q is not reported as unexpected", err)
		}
	})

	t.Run("carriage returns and newlines are trimmed before matching", func(t *testing.T) {
		if err := parseTTYResponse("0\r\n"); err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
	})
}

func TestAppendTTYResizeRequest(t *testing.T) {
	t.Run("a resize request is the op, width and height terminated by a newline", func(t *testing.T) {
		got := string(appendTTYResizeRequest(nil, 80, 24))
		want := "1 80 24\n"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("reusing the buffer overwrites the previous request", func(t *testing.T) {
		buf := appendTTYResizeRequest(nil, 80, 24)
		got := string(appendTTYResizeRequest(buf[:0], 100, 40))
		want := "1 100 40\n"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("encoding into the reusable buffer does not allocate", func(t *testing.T) {
		var buf [ttyRequestBufSize]byte
		var got []byte
		// Largest possible request: op plus two max-uint32 dimensions.
		allocs := testing.AllocsPerRun(1000, func() {
			got = appendTTYResizeRequest(buf[:0], 4294967295, 4294967295)
		})
		if len(got) > len(buf) {
			t.Fatalf("encoded %d bytes into a %d-byte buffer", len(got), len(buf))
		}
		if allocs != 0 {
			t.Fatalf("appendTTYResizeRequest allocated %v times per call, want 0", allocs)
		}
	})
}

func TestResizePTY(t *testing.T) {
	t.Run("a resize sends a framed request and succeeds on the handler ack", func(t *testing.T) {
		h := startFakeTTYHandler(t, serveAcks)

		p := newTTYProcess(t)
		if err := runResize(t, p, h.sock, 80, 24); err != nil {
			t.Fatalf("resize: %v", err)
		}
		if got := h.lastRequest(); got != "1 80 24" {
			t.Fatalf("handler received request %q, want %q", got, "1 80 24")
		}
	})

	t.Run("consecutive resizes reuse a single connection", func(t *testing.T) {
		h := startFakeTTYHandler(t, serveAcks)

		p := newTTYProcess(t)
		if err := runResize(t, p, h.sock, 80, 24); err != nil {
			t.Fatalf("first resize: %v", err)
		}
		if err := runResize(t, p, h.sock, 100, 40); err != nil {
			t.Fatalf("second resize: %v", err)
		}
		if got := h.connCount(); got != 1 {
			t.Fatalf("handler accepted %d connections, want 1 (reuse)", got)
		}
	})

	t.Run("a handler error response is surfaced to the caller", func(t *testing.T) {
		h := startFakeTTYHandler(t, func(h *fakeTTYHandler, conn net.Conn) {
			serveResponses(h, conn, "1 error setting window size\n")
		})

		p := newTTYProcess(t)
		err := runResize(t, p, h.sock, 80, 24)
		if err == nil {
			t.Fatal("got nil error, want the handler error")
		}
		if !strings.Contains(err.Error(), "error setting window size") {
			t.Fatalf("error %q does not carry the handler message", err)
		}
	})

	t.Run("a response fragmented across writes is reassembled", func(t *testing.T) {
		h := startFakeTTYHandler(t, func(h *fakeTTYHandler, conn net.Conn) {
			r := bufio.NewReader(conn)
			for {
				if _, ok := h.readRequest(r); !ok {
					conn.Close()
					return
				}
				// Split the ack so the client must buffer across reads to find
				// the terminating newline.
				conn.Write([]byte("0"))
				time.Sleep(20 * time.Millisecond)
				conn.Write([]byte("\n"))
			}
		})

		p := newTTYProcess(t)
		if err := runResize(t, p, h.sock, 80, 24); err != nil {
			t.Fatalf("fragmented ack: %v", err)
		}
	})
}

func TestTTYControlClientResize(t *testing.T) {
	t.Run("a resize on a reused connection whose deadline has elapsed still succeeds", func(t *testing.T) {
		h := startFakeTTYHandler(t, serveAcks)
		c := newTTYControlClient(h.sock)
		t.Cleanup(func() { c.Close() })

		if err := c.Resize(context.Background(), 80, 24); err != nil {
			t.Fatalf("first resize: %v", err)
		}

		// Leave the cached connection with an already-elapsed deadline, as an
		// earlier request with a short context deadline would. The next resize
		// must set a fresh deadline rather than inherit this one.
		c.mu.Lock()
		c.conn.SetDeadline(time.Now().Add(-time.Hour))
		c.mu.Unlock()

		if err := c.Resize(context.Background(), 100, 40); err != nil {
			t.Fatalf("resize after an elapsed deadline: %v", err)
		}
		// It reused the one connection (a bled-through deadline would have failed
		// the exchange above instead).
		if got := h.connCount(); got != 1 {
			t.Fatalf("handler accepted %d connections, want 1", got)
		}
	})
}

func TestTTYServiceProperties(t *testing.T) {
	tests := []struct {
		name   string
		stdin  string
		stdout string
	}{
		{
			name:   "empty stream paths do not set systemd file properties",
			stdin:  "",
			stdout: "",
		},
		{
			name:   "configured stream paths set systemd file properties",
			stdin:  "/tmp/stdin",
			stdout: "/tmp/stdout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &process{
				exe:    "/usr/bin/shim",
				Stdin:  tc.stdin,
				Stdout: tc.stdout,
			}
			properties := p.ttyServiceProperties("/tmp/tty.sock", "/tmp/tty.log")

			assertTTYFileProperty(t, properties, "StandardInputFile", tc.stdin)
			assertTTYFileProperty(t, properties, "StandardOutputFile", tc.stdout)
		})
	}
}

func assertTTYFileProperty(t *testing.T, properties []dbus.Property, name, want string) {
	t.Helper()

	for _, property := range properties {
		if property.Name != name {
			continue
		}
		got, ok := property.Value.Value().(string)
		if !ok {
			t.Fatalf("%s value type = %T, want string", name, property.Value.Value())
		}
		if want == "" {
			t.Fatalf("%s is set to %q, want it omitted", name, got)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		return
	}
	if want != "" {
		t.Fatalf("%s is omitted, want %q", name, want)
	}
}

// ttyClientReading builds a ttyControlClient whose readMsg reads from s, wired
// through the same reusable io.LimitedReader as a dialed client so the cap is
// exercised.
func ttyClientReading(s string) *ttyControlClient {
	limit := &io.LimitedReader{R: strings.NewReader(s)}
	return &ttyControlClient{reader: bufio.NewReader(limit), limit: limit}
}

// newTTYProcess builds the minimal process ResizePTY needs and ensures its
// tty control client is closed when the test ends.
func newTTYProcess(t *testing.T) *process {
	t.Helper()
	p := &process{Terminal: true}
	t.Cleanup(p.closeTTYControl)
	return p
}

// runResize runs ResizePTY with a watchdog so a hang (e.g. a lock deadlock)
// fails the test instead of hanging the suite.
func runResize(t *testing.T, p *process, sock string, w, h int) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- p.ResizePTY(context.Background(), w, h, sock)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("ResizePTY did not return within 10s; possible lock deadlock")
		return nil
	}
}

// fakeTTYHandler is an in-process stand-in for the pty.c tty handler. It serves
// the newline-framed resize protocol over a unix socket so ResizePTY can be
// exercised end to end.
type fakeTTYHandler struct {
	sock string

	mu       sync.Mutex
	requests []string
	conns    int
}

func startFakeTTYHandler(t *testing.T, serve func(h *fakeTTYHandler, conn net.Conn)) *fakeTTYHandler {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	h := &fakeTTYHandler{sock: sock}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns++
			h.mu.Unlock()
			serve(h, conn)
		}
	}()
	return h
}

// readRequest reads one framed request from the client and records it.
func (h *fakeTTYHandler) readRequest(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", false
	}
	req := strings.TrimRight(line, "\n")
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	return req, true
}

func (h *fakeTTYHandler) lastRequest() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		return ""
	}
	return h.requests[len(h.requests)-1]
}

func (h *fakeTTYHandler) connCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

// serveAcks answers every request on the connection with a success ack.
func serveAcks(h *fakeTTYHandler, conn net.Conn) {
	serveResponses(h, conn, "0\n")
}

// serveResponses answers every request on the connection with resp until the
// client closes it.
func serveResponses(h *fakeTTYHandler, conn net.Conn, resp string) {
	r := bufio.NewReader(conn)
	for {
		if _, ok := h.readRequest(r); !ok {
			conn.Close()
			return
		}
		if _, err := conn.Write([]byte(resp)); err != nil {
			conn.Close()
			return
		}
	}
}
