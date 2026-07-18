package main

import (
	"context"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

func TestDecodeSystemdPropertiesChanged(t *testing.T) {
	changed := changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"})

	t.Run("a Unit PropertiesChanged signal decodes to its escaped path base and changed set", func(t *testing.T) {
		u, ok := decodeSystemdPropertiesChanged(unitSignal("foo_2eservice", changed))
		if !ok {
			t.Fatal("expected a Unit PropertiesChanged signal to decode")
		}
		if u.pathBase != "foo_2eservice" {
			t.Fatalf("path base = %q, want foo_2eservice (left escaped)", u.pathBase)
		}
		if v, ok := u.changed["ActiveState"]; !ok || v.Value() != "failed" {
			t.Fatalf("changed[ActiveState] = %v, want failed", u.changed["ActiveState"])
		}
		if u.interfaceName != unitInterface {
			t.Fatalf("interface = %q, want %q", u.interfaceName, unitInterface)
		}
	})

	t.Run("a Service PropertiesChanged signal decodes with its interface", func(t *testing.T) {
		u, ok := decodeSystemdPropertiesChanged(serviceSignal("foo_2eservice", changed))
		if !ok {
			t.Fatal("expected a Service PropertiesChanged signal to decode")
		}
		if u.interfaceName != serviceInterface {
			t.Fatalf("interface = %q, want %q", u.interfaceName, serviceInterface)
		}
	})

	t.Run("a PropertiesChanged for another interface is ignored", func(t *testing.T) {
		sig := propertiesSignal("foo_2eservice", "org.freedesktop.systemd1.Socket", changed)
		if _, ok := decodeSystemdPropertiesChanged(sig); ok {
			t.Fatal("expected an unrelated interface to be ignored")
		}
	})

	t.Run("a signal that is not PropertiesChanged is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Name = "org.freedesktop.systemd1.Manager.UnitNew"
		if _, ok := decodeSystemdPropertiesChanged(sig); ok {
			t.Fatal("expected a non-PropertiesChanged signal to be ignored")
		}
	})

	t.Run("a signal with a short body is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Body = sig.Body[:1]
		if _, ok := decodeSystemdPropertiesChanged(sig); ok {
			t.Fatal("expected a short body to be ignored")
		}
	})

	t.Run("a signal whose changed set is not a property map is ignored", func(t *testing.T) {
		sig := unitSignal("foo_2eservice", changed)
		sig.Body[1] = "not a map"
		if _, ok := decodeSystemdPropertiesChanged(sig); ok {
			t.Fatal("expected a malformed changed set to be ignored")
		}
	})

	t.Run("a nil signal is ignored", func(t *testing.T) {
		if _, ok := decodeSystemdPropertiesChanged(nil); ok {
			t.Fatal("expected a nil signal to be ignored")
		}
	})
}

func TestServiceExitState(t *testing.T) {
	t.Run("terminal service properties preserve the pid, exit code, and timestamp", func(t *testing.T) {
		exitedAt := time.Date(2026, time.July, 16, 12, 0, 0, 123000000, time.Local)
		changed := map[string]dbus.Variant{
			"ExecMainPID":           dbus.MakeVariant(uint32(42)),
			"ExecMainStatus":        dbus.MakeVariant(int32(17)),
			"ExecMainExitTimestamp": dbus.MakeVariant(uint64(exitedAt.UnixMicro())),
		}

		state, ok := serviceExitState(changed)
		if !ok {
			t.Fatal("terminal service properties were not recognized")
		}
		if state.Pid != 42 || state.ExitCode != 17 || !state.ExitedAt.Equal(exitedAt) || state.Status != "exited" {
			t.Fatalf("state = %s, want pid 42, exit 17, timestamp %s, status exited", state, exitedAt)
		}
	})

	t.Run("service properties without an exit timestamp are not terminal", func(t *testing.T) {
		changed := map[string]dbus.Variant{
			"ExecMainPID":           dbus.MakeVariant(uint32(42)),
			"ExecMainStatus":        dbus.MakeVariant(int32(0)),
			"ExecMainExitTimestamp": dbus.MakeVariant(uint64(0)),
		}

		if _, ok := serviceExitState(changed); ok {
			t.Fatal("running service properties were recognized as an exit")
		}
	})

	t.Run("service properties with a negative exit status are invalid", func(t *testing.T) {
		changed := map[string]dbus.Variant{
			"ExecMainPID":           dbus.MakeVariant(uint32(42)),
			"ExecMainStatus":        dbus.MakeVariant(int32(-1)),
			"ExecMainExitTimestamp": dbus.MakeVariant(uint64(time.Now().UnixMicro())),
		}

		if _, ok := serviceExitState(changed); ok {
			t.Fatal("a negative service exit status was accepted")
		}
	})
}

func TestUnitUpdates(t *testing.T) {
	changed := changedProps(map[string]string{"ActiveState": "failed"})

	t.Run("only Unit and Service PropertiesChanged signals reach the consumer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigs := make(chan *dbus.Signal, 8)
		sigs <- unitSignal("keep_2eservice", changed)
		sigs <- serviceSignal("service_2eservice", changed)
		other := unitSignal("drop_2eservice", changed)
		other.Name = "org.freedesktop.systemd1.Manager.UnitNew"
		sigs <- other
		sigs <- unitSignal("also_2eservice", changed)
		close(sigs)

		var got []string
		for u := range unitUpdates(ctx, sigs) {
			got = append(got, u.pathBase)
		}
		if len(got) != 3 || got[0] != "keep_2eservice" || got[1] != "service_2eservice" || got[2] != "also_2eservice" {
			t.Fatalf("got %v, want [keep_2eservice service_2eservice also_2eservice]", got)
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
		serviceSignal(systemd.PathBusEscape(ours), map[string]dbus.Variant{
			"ExecMainPID":           dbus.MakeVariant(uint32(42)),
			"ExecMainStatus":        dbus.MakeVariant(int32(3)),
			"ExecMainExitTimestamp": dbus.MakeVariant(uint64(time.Now().UnixMicro())),
		}),
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
			if u, ok := decodeSystemdPropertiesChanged(s); ok {
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
	return propertiesSignal(escapedUnit, unitInterface, changed)
}

func serviceSignal(escapedUnit string, changed map[string]dbus.Variant) *dbus.Signal {
	return propertiesSignal(escapedUnit, serviceInterface, changed)
}

func propertiesSignal(escapedUnit, interfaceName string, changed map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{
		Path: dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + escapedUnit),
		Name: propertiesChangedSignal,
		Body: []interface{}{interfaceName, changed, []string{}},
	}
}
