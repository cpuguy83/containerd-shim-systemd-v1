package main

import (
	"context"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

func TestDecodeUnitPropertiesChanged(t *testing.T) {
	changed := changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"})

	t.Run("a Unit PropertiesChanged signal decodes to its escaped path base and changed set", func(t *testing.T) {
		u, ok := decodeUnitPropertiesChanged(unitSignal("foo_2eservice", changed))
		if !ok {
			t.Fatal("expected a Unit PropertiesChanged signal to decode")
		}
		if u.pathBase != "foo_2eservice" {
			t.Fatalf("path base = %q, want foo_2eservice (left escaped)", u.pathBase)
		}
		if v, ok := u.changed["ActiveState"]; !ok || v.Value() != "failed" {
			t.Fatalf("changed[ActiveState] = %v, want failed", u.changed["ActiveState"])
		}
	})

	t.Run("a PropertiesChanged for a non-Unit interface is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Body[0] = "org.freedesktop.systemd1.Service"
		if _, ok := decodeUnitPropertiesChanged(sig); ok {
			t.Fatal("expected a non-Unit interface to be ignored")
		}
	})

	t.Run("a signal that is not PropertiesChanged is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Name = "org.freedesktop.systemd1.Manager.UnitNew"
		if _, ok := decodeUnitPropertiesChanged(sig); ok {
			t.Fatal("expected a non-PropertiesChanged signal to be ignored")
		}
	})

	t.Run("a signal with a short body is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Body = sig.Body[:1]
		if _, ok := decodeUnitPropertiesChanged(sig); ok {
			t.Fatal("expected a short body to be ignored")
		}
	})

	t.Run("a signal whose changed set is not a property map is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Body[1] = "not a map"
		if _, ok := decodeUnitPropertiesChanged(sig); ok {
			t.Fatal("expected a malformed changed set to be ignored")
		}
	})

	t.Run("a nil signal is ignored", func(t *testing.T) {
		if _, ok := decodeUnitPropertiesChanged(nil); ok {
			t.Fatal("expected a nil signal to be ignored")
		}
	})
}

func TestUnitUpdates(t *testing.T) {
	changed := changedProps(map[string]string{"ActiveState": "failed"})

	t.Run("only Unit PropertiesChanged signals reach the consumer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigs := make(chan *dbus.Signal, 8)
		sigs <- unitSignal("keep_2eservice", changed)
		other := unitSignal("drop_2eservice", changed)
		other.Name = "org.freedesktop.systemd1.Manager.UnitNew"
		sigs <- other
		sigs <- unitSignal("also_2eservice", changed)
		close(sigs)

		var got []string
		for u := range unitUpdates(ctx, sigs) {
			got = append(got, u.pathBase)
		}
		if len(got) != 2 || got[0] != "keep_2eservice" || got[1] != "also_2eservice" {
			t.Fatalf("got %v, want [keep_2eservice also_2eservice]", got)
		}
	})

	t.Run("the stream ends when the context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan *dbus.Signal) // never sends
		cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range unitUpdates(ctx, sigs) {
			}
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("unitUpdates did not stop after context cancellation")
		}
	})

	t.Run("the stream ends when the signal channel closes", func(t *testing.T) {
		sigs := make(chan *dbus.Signal)
		close(sigs)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range unitUpdates(context.Background(), sigs) {
			}
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("unitUpdates did not stop after the channel closed")
		}
	})

	t.Run("a consumer that stops early ends the stream", func(t *testing.T) {
		sigs := make(chan *dbus.Signal, 4)
		sigs <- unitSignal("first_2eservice", changed)
		sigs <- unitSignal("second_2eservice", changed)

		var seen int
		for range unitUpdates(context.Background(), sigs) {
			seen++
			break
		}
		if seen != 1 {
			t.Fatalf("consumer saw %d updates before breaking, want 1", seen)
		}
	})
}

// Signal intake runs for every PropertiesChanged the bus broadcasts, most of
// which are not ours, so it must not allocate per signal.
func TestSignalIntakeDoesNotAllocate(t *testing.T) {
	active := changedProps(map[string]string{"ActiveState": "active", "SubState": "running"})
	failed := changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"})

	ours := "io-containerd-systemd-ns-abc-def-init.service"
	sigs := []*dbus.Signal{
		unitSignal("systemd_2dlogind_2eservice", active),
		unitSignal("dbus_2eservice", active),
		unitSignal("systemd_2djournald_2eservice", failed), // a foreign unit's exit
		unitSignal(systemd.PathBusEscape(ours), active),
		unitSignal(systemd.PathBusEscape(ours), failed),
	}

	p := &fakeProcess{name: ours}
	p.setState(pState{ExitCode: 3, ExitedAt: time.Now()})
	units := newUnitManager()
	units.Add(p)
	q := newUnitWorkQueue()
	defer q.ShutDown()

	got := testing.AllocsPerRun(100, func() {
		for _, s := range sigs {
			if u, ok := decodeUnitPropertiesChanged(s); ok {
				enqueueIfExit(q, units, u)
			}
		}
	})
	if got != 0 {
		t.Fatalf("signal intake allocated %v times/run, want 0", got)
	}
}

// unitSignal builds a Unit PropertiesChanged signal for the given escaped unit
// path element and changed property set.
func unitSignal(escapedUnit string, changed map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{
		Path: dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + escapedUnit),
		Name: propertiesChangedSignal,
		Body: []interface{}{unitInterface, changed, []string{}},
	}
}
