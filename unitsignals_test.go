package main

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestPathBusUnescape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"an unescaped element is returned unchanged", "foostuff", "foostuff"},
		{"an escaped dot decodes to a period", "foo_2eservice", "foo.service"},
		{"escaped dashes decode to hyphens", "io_2dcontainerd_2dfoo_2eservice", "io-containerd-foo.service"},
		{"a lone underscore is the empty string", "_", ""},
		{"a trailing underscore is taken literally", "foo_", "foo_"},
		{"an underscore with too few following chars is literal", "foo_2", "foo_2"},
		{"an invalid hex escape is taken literally", "foo_zzbar", "foo_zzbar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathBusUnescape(tc.in); got != tc.want {
				t.Fatalf("pathBusUnescape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnitNameFromPath(t *testing.T) {
	t.Run("a unit object path decodes back to the unit name", func(t *testing.T) {
		const op = dbus.ObjectPath("/org/freedesktop/systemd1/unit/io_2dcontainerd_2dsystemd_2dns_2dabc_2dinit_2eservice")
		if got, want := unitNameFromPath(op), "io-containerd-systemd-ns-abc-init.service"; got != want {
			t.Fatalf("unitNameFromPath(%q) = %q, want %q", op, got, want)
		}
	})
}

func TestDecodeUnitPropertiesChanged(t *testing.T) {
	changed := changedProps(map[string]string{"ActiveState": "failed", "SubState": "failed"})

	t.Run("a Unit PropertiesChanged signal decodes to its unit and changed set", func(t *testing.T) {
		u, ok := decodeUnitPropertiesChanged(unitSignal("foo_2eservice", changed))
		if !ok {
			t.Fatal("expected a Unit PropertiesChanged signal to decode")
		}
		if u.Name != "foo.service" {
			t.Fatalf("unit name = %q, want foo.service", u.Name)
		}
		if v, ok := u.Changed["ActiveState"]; !ok || v.Value() != "failed" {
			t.Fatalf("changed[ActiveState] = %v, want failed", u.Changed["ActiveState"])
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
			got = append(got, u.Name)
		}
		if len(got) != 2 || got[0] != "keep.service" || got[1] != "also.service" {
			t.Fatalf("got %v, want [keep.service also.service]", got)
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

// unitSignal builds a Unit PropertiesChanged signal for the given escaped unit
// path element and changed property set.
func unitSignal(escapedUnit string, changed map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{
		Path: dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + escapedUnit),
		Name: propertiesChangedSignal,
		Body: []interface{}{unitInterface, changed, []string{}},
	}
}
