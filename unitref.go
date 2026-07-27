package main

import (
	"context"
	"os"

	"github.com/containerd/log"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

const (
	systemdDBusName = "org.freedesktop.systemd1"
	systemdDBusPath = dbus.ObjectPath("/org/freedesktop/systemd1")
)

// connectSystemdMessageBus dials the D-Bus message bus systemd is registered on:
// the system bus as root, otherwise the caller's session bus (a user manager).
// Unlike systemd's private socket, a message-bus connection has a unique bus
// name, which unit references are anchored to -- AddRef/UnrefUnit reject a
// private-bus caller (it has no sender) with EINVAL.
func connectSystemdMessageBus(ctx context.Context) (*dbus.Conn, error) {
	if os.Geteuid() == 0 {
		return dbus.ConnectSystemBus(dbus.WithContext(ctx))
	}
	return dbus.ConnectSessionBus(dbus.WithContext(ctx))
}

// newPinConnection opens a single message-bus connection for pinning transient
// units. It returns a go-systemd Conn used to StartTransientUnit with AddRef
// (which also gives StartTransientUnit its job-completion channel) and the raw
// method connection the reference is anchored to, used for UnrefUnit (which
// go-systemd does not expose). Both wrap the same underlying connection, so one
// bus name owns every reference and UnrefUnit releases what AddRef took;
// closing the Conn closes both.
func newPinConnection(ctx context.Context) (*systemd.Conn, *dbus.Conn, error) {
	var method *dbus.Conn
	dialed := 0
	conn, err := systemd.NewConnection(func() (*dbus.Conn, error) {
		c, err := connectSystemdMessageBus(ctx)
		if err != nil {
			return nil, err
		}
		// NewConnection dials twice; the first connection is the one method
		// calls (StartTransientUnit, UnrefUnit) go out on and that references are
		// anchored to.
		if dialed == 0 {
			method = c
		}
		dialed++
		return c, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return conn, method, nil
}

// unrefUnit releases the reference AddRef installed on a transient unit, letting
// systemd collect it once inactive. refConn must be the method connection the
// reference was taken on.
func unrefUnit(ctx context.Context, refConn *dbus.Conn, name string) error {
	return refConn.Object(systemdDBusName, systemdDBusPath).
		CallWithContext(ctx, systemdDBusName+".Manager.UnrefUnit", 0, name).Err
}

// startTransientUnit starts the process's transient unit. When a pin connection
// is configured it starts on that message-bus connection with AddRef=true, so
// systemd takes a reference atomically with unit creation: the unit then stays
// queryable over D-Bus after a clean exit -- the shim's source of truth for
// terminal state -- even if the exit signal is missed and would otherwise let
// systemd collect it. Without a pin connection it falls back to the private
// method connection and takes no reference.
//
// It does not release any existing reference: the start-retry paths that
// re-create a unit they already started release the prior reference explicitly
// (a referenced unit lingers and StartTransientUnit would fail "already
// exists"), so that a *duplicate* start of a still-live unit does not strip its
// reference.
func (p *process) startTransientUnit(ctx context.Context, name, mode string, props []systemd.Property, ch chan string) error {
	if p.pinConn == nil {
		_, err := p.systemd.StartTransientUnitContext(ctx, name, mode, props, ch)
		return err
	}
	pinned := make([]systemd.Property, len(props), len(props)+1)
	copy(pinned, props)
	pinned = append(pinned, systemd.Property{Name: "AddRef", Value: dbus.MakeVariant(true)})
	_, err := p.pinConn.StartTransientUnitContext(ctx, name, mode, pinned, ch)
	return err
}

// releaseUnit drops the AddRef reference taken by startTransientUnit so systemd
// can collect the inactive transient unit at Delete. It is a no-op without a pin
// connection. Callers tearing down after a failure must pass a context that is
// not already canceled (see cleanupContext), since a reference that is never
// released keeps the unit loaded.
func (p *process) releaseUnit(ctx context.Context, name string) {
	if p.pinConn == nil {
		return
	}
	if err := unrefUnit(ctx, p.pinRef, name); err != nil {
		log.G(ctx).WithField("unit", name).WithError(err).Debug("Failed to release transient unit reference")
	}
}
