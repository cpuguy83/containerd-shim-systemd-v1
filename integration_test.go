//go:build integration

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
// unprivileged during development. Run with: go test -tags integration ./...

func TestReactorReactsToRealExit(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)
	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	t.Run("a real unit exit is observed and handled exactly once", func(t *testing.T) {
		unit := uniqueUnit("react-once")
		p := &fakeProcess{name: unit, loadStateFn: func(f *fakeProcess) error {
			f.state = pState{ExitCode: 3, ExitedAt: time.Now()}
			return nil
		}}
		units := &fakeLookup{m: map[string]Process{unit: p}}

		updates := make(chan *systemd.PropertiesUpdate, 256)
		errs := make(chan error, 256)
		conn.SetPropertiesSubscriber(updates, errs)

		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go reactToUnitEvents(rctx, units, updates, errs)

		startTransient(t, ctx, conn, unit, []string{"/bin/sh", "-c", "exit 3"})
		defer resetUnit(ctx, conn, unit)

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

func TestPropertiesSubscriberDoesNotAmplify(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)

	mon := newSignalMonitor(t, ctx, path)
	defer mon.close()

	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	// New design: properties subscriber only, no GetAll in the subscriber.
	updates := make(chan *systemd.PropertiesUpdate, 1024)
	errs := make(chan error, 1024)
	conn.SetPropertiesSubscriber(updates, errs)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-updates:
			case <-errs:
			}
		}
	}()

	// Churn short-lived units.
	const rounds, per = 5, 20
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < per; i++ {
			unit := uniqueUnit(fmt.Sprintf("amp-%d-%d", r, i))
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

	// Storm signature: signals keep flooding after the workload stops. With the
	// properties subscriber (no GetAll feedback) the bus goes quiet almost
	// immediately, so awaitQuiet returns fast on success; a self-amplifying
	// subscriber never reaches a quiet step and this only returns false after
	// the full timeout (a failing test is allowed to take longer).
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

func privateBusPath(t *testing.T) string {
	t.Helper()
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
	conn, err := systemd.NewConnection(func() (*dbus.Conn, error) {
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
	if err != nil {
		t.Fatalf("dial private bus: %v", err)
	}
	return conn
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

func resetUnit(ctx context.Context, conn *systemd.Conn, name string) {
	conn.StopUnitContext(ctx, name, "replace", nil)
	conn.ResetFailedUnitContext(ctx, name)
}

type signalMonitor struct {
	conn  *dbus.Conn
	count int64
}

func newSignalMonitor(t *testing.T, ctx context.Context, path string) *signalMonitor {
	t.Helper()
	c, err := dbus.Dial(path, dbus.WithContext(ctx))
	if err != nil {
		t.Fatalf("dial monitor: %v", err)
	}
	if err := c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		c.Close()
		t.Fatalf("auth monitor: %v", err)
	}
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
