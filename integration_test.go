package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// These tests exercise the real event reactor against a live systemd manager.
// They use the system private bus when run as root (CI) and otherwise fall back
// to the per-user systemd instance's private socket, so they can run
// unprivileged during development. They skip in -short mode and when no private
// bus is reachable.

func TestReactorReactsToRealExit(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)
	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	t.Run("a real unit exit is observed and handled exactly once", func(t *testing.T) {
		reg := newUnitRegistry(t, path)
		unit := reg.unit("react-once")
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			f.state = pState{ExitCode: 3, ExitedAt: time.Now()}
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		sigConn := dialSignalConn(t, ctx, path)
		defer sigConn.Close()
		sigs := make(chan *dbus.Signal, signalBufferSize)
		sigConn.Signal(sigs)

		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go reactToUnitEvents(rctx, units, unitUpdates(rctx, sigs))

		startTransient(t, ctx, conn, unit, []string{"/bin/sh", "-c", "exit 3"})

		if !eventually(10*time.Second, 10*time.Millisecond, func() bool { return p.LoadStateCalls() >= 1 }) {
			t.Fatal("reactor never observed the unit exit")
		}

		// Confirm it reacts exactly once: the count must settle and stay at 1.
		// stableFor returns as soon as the count stops changing, so a correct
		// (single-reaction) run returns after the short debounce rather than a
		// fixed delay; a duplicate-reaction bug keeps it climbing until timeout.
		if got := stableFor(10*time.Second, 300*time.Millisecond, 10*time.Millisecond, func() int64 { return int64(p.LoadStateCalls()) }); got != 1 {
			t.Fatalf("expected exactly one reaction to a single exit, got %d", got)
		}
	})
}

func TestSignalConsumerDoesNotAmplify(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)
	reg := newUnitRegistry(t, path)

	mon := newSignalMonitor(t, ctx, path)
	defer mon.close()

	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	// Run our raw signal consumer during the churn: it decodes unit updates but
	// never reads unit properties, so unlike go-systemd's substate subscriber it
	// cannot feed back UnitNew/UnitRemoved and amplify the signal stream.
	sigConn := dialSignalConn(t, ctx, path)
	defer sigConn.Close()
	sigs := make(chan *dbus.Signal, 1024)
	sigConn.Signal(sigs)
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	go func() {
		for range unitUpdates(cctx, sigs) {
		}
	}()

	// Churn short-lived units.
	const rounds, per = 5, 20
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < per; i++ {
			unit := reg.unit(fmt.Sprintf("amp-%d-%d", r, i))
			ch := make(chan string, 1)
			props := []systemd.Property{
				systemd.PropExecStart([]string{"/bin/true"}, false),
				{Name: "Type", Value: dbus.MakeVariant("oneshot")},
			}
			if _, err := conn.StartTransientUnitContext(ctx, unit, "replace", props, ch); err != nil {
				continue
			}
			wg.Add(1)
			go func() { defer wg.Done(); <-ch }()
		}
		wg.Wait()
	}

	afterWorkload := mon.total()

	// Storm signature: signals keep flooding after the workload stops. With a
	// consumer that never reads properties the bus goes quiet almost immediately,
	// so awaitQuiet returns fast on success; a self-amplifying consumer never
	// reaches a quiet step and this only returns false after the full timeout (a
	// failing test is allowed to take longer).
	if !awaitQuiet(10*time.Second, 100*time.Millisecond, 50, mon.total) {
		t.Fatalf("bus never went quiet after workload; likely event amplification (%d signals since workload)", mon.total()-afterWorkload)
	}
}

// awaitQuiet polls total() every step and returns true as soon as fewer than
// maxPerStep new signals arrive within a step (the bus has gone quiet). If
// signals keep arriving above that rate until timeout, it returns false. A quiet
// (passing) bus returns after roughly one step; a storming bus runs to timeout.
func awaitQuiet(timeout, step time.Duration, maxPerStep int64, total func() int64) bool {
	deadline := time.Now().Add(timeout)
	prev := total()
	for time.Now().Before(deadline) {
		time.Sleep(step)
		cur := total()
		if cur-prev <= maxPerStep {
			return true
		}
		prev = cur
	}
	return false
}

// --- integration helpers ---

// TestLoadExitFromUnitReadsRealExitCode is the load-bearing check for removing
// the ExecStopPost exit helper: with no helper writing the state file on stop,
// the reconcile path's only source of the terminal code is systemd's
// ExecMainStatus. This confirms it is populated and read for a real exit.
// Signal, tty, and mount-cleanup behaviour is exercised by the container-level
// integration harness, where the unit wraps a real runc container rather than a
// bare shell (a self-signalled transient unit does not faithfully model it).
func TestLoadExitFromUnitReadsRealExitCode(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)
	reg := newUnitRegistry(t, path)
	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	t.Run("a non-zero exit is read from ExecMainStatus", func(t *testing.T) {
		unit := reg.unit("exit-nonzero")
		startTransient(t, ctx, conn, unit, []string{"/bin/sh", "-c", "exit 7"})

		var st pState
		if !eventually(10*time.Second, 20*time.Millisecond, func() bool {
			s, err := loadExitFromUnit(ctx, conn, unit)
			if err != nil {
				return false
			}
			st = s
			return st.Exited()
		}) {
			t.Fatalf("unit %s never reported an exit via systemd", unit)
		}
		if st.ExitCode != 7 {
			t.Fatalf("expected exit code 7 from systemd, got %d", st.ExitCode)
		}
	})
}

// privateBusPath returns the address of a systemd private bus to test against,
// or skips the test when one is not available. It skips in -short mode (these
// tests talk to a real systemd and create transient units) and when no private
// bus is reachable: the system bus when running as root, otherwise the caller's
// user bus.
func privateBusPath(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	if os.Geteuid() == 0 {
		if _, err := os.Stat("/run/systemd/private"); err == nil {
			return "unix:path=/run/systemd/private"
		}
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		t.Skip("no systemd private bus available (set XDG_RUNTIME_DIR or run as root)")
	}
	p := xdg + "/systemd/private"
	if _, err := os.Stat(p); err != nil {
		t.Skipf("user systemd private socket not available: %v", err)
	}
	return "unix:path=" + p
}

func dialPrivate(t *testing.T, ctx context.Context, path string) *systemd.Conn {
	t.Helper()
	conn, err := dialPrivateConn(ctx, path)
	if err != nil {
		t.Fatalf("dial private bus: %v", err)
	}
	return conn
}

func dialPrivateConn(ctx context.Context, path string) (*systemd.Conn, error) {
	return systemd.NewConnection(func() (*dbus.Conn, error) {
		c, err := dbus.Dial(path, dbus.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if err := c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	})
}

// unitRegistry tracks the transient units a test creates and resets them all on
// cleanup, using its own short-lived connection. Registering via t.Cleanup means
// units are reset even when a test fails or panics, so the harness never leaves
// failed shim-itest-* units behind on the host bus.
type unitRegistry struct {
	path  string
	mu    sync.Mutex
	names []string
}

func newUnitRegistry(t *testing.T, path string) *unitRegistry {
	r := &unitRegistry{path: path}
	t.Cleanup(func() {
		r.mu.Lock()
		names := r.names
		r.mu.Unlock()
		if len(names) == 0 {
			return
		}
		conn, err := dialPrivateConn(context.Background(), path)
		if err != nil {
			return
		}
		defer conn.Close()
		for _, n := range names {
			conn.StopUnitContext(context.Background(), n, "replace", nil)
			conn.ResetFailedUnitContext(context.Background(), n)
		}
	})
	return r
}

// unit returns a fresh unique unit name and registers it for cleanup.
func (r *unitRegistry) unit(tag string) string {
	name := uniqueUnit(tag)
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	return name
}

func uniqueUnit(tag string) string {
	return fmt.Sprintf("shim-itest-%s-%d.service", tag, time.Now().UnixNano())
}

func startTransient(t *testing.T, ctx context.Context, conn *systemd.Conn, name string, cmd []string) {
	t.Helper()
	ch := make(chan string, 1)
	props := []systemd.Property{
		systemd.PropExecStart(cmd, false),
		{Name: "Type", Value: dbus.MakeVariant("exec")},
	}
	if _, err := conn.StartTransientUnitContext(ctx, name, "replace", props, ch); err != nil {
		t.Fatalf("start transient unit %s: %v", name, err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out starting unit %s", name)
	}
}

type signalMonitor struct {
	conn  *dbus.Conn
	count int64
}

// dialSignalConn opens a raw godbus connection to the given private bus for
// receiving signals: authenticate as the current uid and skip Hello, since the
// private bus talks straight to systemd with no message bus.
func dialSignalConn(t *testing.T, ctx context.Context, path string) *dbus.Conn {
	t.Helper()
	c, err := dbus.Dial(path, dbus.WithContext(ctx))
	if err != nil {
		t.Fatalf("dial signal conn: %v", err)
	}
	if err := c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		c.Close()
		t.Fatalf("auth signal conn: %v", err)
	}
	return c
}

func newSignalMonitor(t *testing.T, ctx context.Context, path string) *signalMonitor {
	t.Helper()
	c := dialSignalConn(t, ctx, path)
	m := &signalMonitor{conn: c}
	ch := make(chan *dbus.Signal, 4096)
	c.Signal(ch)
	go func() {
		for range ch {
			atomic.AddInt64(&m.count, 1)
		}
	}()
	return m
}

func (m *signalMonitor) total() int64 { return atomic.LoadInt64(&m.count) }
func (m *signalMonitor) close()       { m.conn.Close() }
