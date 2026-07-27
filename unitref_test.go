package main

import (
	"context"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// TestStartTransientUnitAddRefSurvivesGC proves the fix for the missed-signal
// race: a transient unit started with AddRef=true stays loaded after a clean
// exit -- so its terminal state can still be recovered over D-Bus -- while an
// otherwise-identical unreferenced unit is garbage-collected. The unreferenced
// unit is the control: it proves the reference, not systemd merely keeping the
// unit around, is what survives collection.
func TestStartTransientUnitAddRefSurvivesGC(t *testing.T) {
	ctx := context.Background()
	path := privateBusPath(t)
	reg := newUnitRegistry(t, path)
	conn := dialPrivate(t, ctx, path)
	defer conn.Close()

	pinConn, pinRef, err := newPinConnection(ctx)
	if err != nil {
		t.Skipf("no D-Bus message bus for unit references: %v", err)
	}
	defer pinConn.Close()

	// unitLoaded reports whether systemd still holds the unit. It uses
	// Manager.GetUnit, which does NOT load the unit -- unlike a property read on
	// the unit's object path, which re-materializes a collected transient unit as
	// an empty one -- so it distinguishes a still-present unit from a collected
	// one. pinRef is the raw message-bus (godbus) connection; unit load state is
	// global, so it observes the same units the private connection created.
	unitLoaded := func(name string) bool {
		var p dbus.ObjectPath
		err := pinRef.Object(systemdDBusName, systemdDBusPath).
			CallWithContext(ctx, systemdDBusName+".Manager.GetUnit", 0, name).Store(&p)
		return err == nil
	}

	// Both units exit cleanly (code 0), leaving them inactive-dead -- the state
	// systemd garbage-collects once unreferenced. ("exit 0" is a shell builtin, so
	// it does not depend on the unit's PATH.) AddRef takes the reference
	// atomically with unit creation, the way startTransientUnit does.
	pinned := reg.unit("addref-pinned")
	control := reg.unit("addref-control")

	startClean := func(name string, addRef bool) {
		t.Helper()
		props := []systemd.Property{
			systemd.PropExecStart([]string{"/bin/sh", "-c", "exit 0"}, false),
			{Name: "Type", Value: dbus.MakeVariant("exec")},
		}
		if addRef {
			props = append(props, systemd.Property{Name: "AddRef", Value: dbus.MakeVariant(true)})
		}
		ch := make(chan string, 1)
		if _, err := pinConn.StartTransientUnitContext(ctx, name, "replace", props, ch); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		select {
		case status := <-ch:
			if status != "done" {
				t.Fatalf("start %s: status %s", name, status)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out starting %s", name)
		}
	}

	startClean(pinned, true)
	startClean(control, false)

	// Confirm both exited cleanly, reading their exit while still loaded.
	for _, name := range []string{pinned, control} {
		var st pState
		if !eventually(10*time.Second, 20*time.Millisecond, func() bool {
			s, err := loadExitFromUnit(ctx, conn, name)
			if err == nil && s.Exited() {
				st = s
				return true
			}
			return false
		}) {
			t.Fatalf("%s never reported an exit via systemd", name)
		}
		if st.ExitCode != 0 {
			t.Fatalf("%s did not exit cleanly (code %d); a failed unit lingers regardless of the reference", name, st.ExitCode)
		}
	}

	// The unreferenced control is collected once systemd's GC queue runs (churn
	// via ListUnits drives it). Probe collection with GetUnit, not a property
	// read, so the probe itself does not reload it.
	if !eventually(10*time.Second, 50*time.Millisecond, func() bool {
		for i := 0; i < 20; i++ {
			conn.ListUnitsContext(ctx)
		}
		return !unitLoaded(control)
	}) {
		t.Fatal("unreferenced control unit was not collected after a clean exit")
	}
	// Under the very same churn the AddRef'd unit stays loaded and its exit stays
	// recoverable.
	if !unitLoaded(pinned) {
		t.Fatal("AddRef'd unit was collected after a clean exit")
	}
	if st, err := loadExitFromUnit(ctx, conn, pinned); err != nil || !st.Exited() {
		t.Fatalf("AddRef'd unit's exit was not recoverable after a clean exit: err=%v state=%+v", err, st)
	}

	// The reference releases cleanly on the same connection, letting systemd
	// finally collect the unit.
	if err := unrefUnit(ctx, pinRef, pinned); err != nil {
		t.Fatalf("UnrefUnit: %v", err)
	}
}
