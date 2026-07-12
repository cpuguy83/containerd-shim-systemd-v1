package main

import (
	"context"
	"iter"
	"os"
	"path"
	"strconv"

	"github.com/godbus/dbus/v5"
)

// The shim subscribes to systemd unit state by reading D-Bus signals off a
// dedicated connection itself, rather than via go-systemd's Set*Subscriber. That
// avoids go-systemd's dispatch goroutine, which reads the subscriber field
// without the lock its setter uses (a data race, coreos/go-systemd#519) and
// which issues a GetUnitPathProperties (GetAll) per signal -- the read that,
// against an unloaded unit, feeds back UnitNew/UnitRemoved and drove the
// historical event storm. Reading raw signals never reads unit properties, so it
// cannot self-trigger; we do a single targeted read only for our own units, only
// when they exit.

const (
	// signalBufferSize bounds the raw signal channel. The private bus broadcasts
	// every unit's signals, so this absorbs bursts while the informer drains.
	signalBufferSize = 256

	unitInterface           = "org.freedesktop.systemd1.Unit"
	propertiesChangedSignal = "org.freedesktop.DBus.Properties.PropertiesChanged"

	// systemdPrivateBus is systemd's direct private socket. It requires no
	// message bus (no Hello) and broadcasts all unit signals unconditionally.
	systemdPrivateBus = "unix:path=/run/systemd/private"
)

// unitUpdate is a decoded systemd Unit PropertiesChanged event. pathBase is the
// unit's D-Bus object-path base, left systemd-escaped (e.g.
// "io_2dcontainerd_2dsystemd_2d..._2eservice"). It is used verbatim as an index
// key via processLookup.GetByPath: the private bus broadcasts every unit's
// signals, so unescaping each name would allocate a string we discard for all
// the units we do not track. Escaping our own names once, when a unit is added,
// is far cheaper than unescaping every signal.
type unitUpdate struct {
	pathBase string
	changed  map[string]dbus.Variant
}

// dialSignalBus opens a dedicated connection to systemd's private bus for
// receiving signals. It authenticates as the current uid and, like
// go-systemd's direct connection, skips Hello -- the private bus talks straight
// to systemd with no message bus.
func dialSignalBus(ctx context.Context) (*dbus.Conn, error) {
	conn, err := dbus.Dial(systemdPrivateBus, dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// unitUpdates yields decoded Unit property changes from a raw signal stream
// until ctx is cancelled or the stream closes. Signals that are not a Unit
// PropertiesChanged are dropped. It does no I/O, so it can never trigger the
// GetAll feedback storm.
func unitUpdates(ctx context.Context, sigs <-chan *dbus.Signal) iter.Seq[unitUpdate] {
	return func(yield func(unitUpdate) bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigs:
				if !ok {
					return
				}
				u, ok := decodeUnitPropertiesChanged(sig)
				if !ok {
					continue
				}
				if !yield(u) {
					return
				}
			}
		}
	}
}

// decodeUnitPropertiesChanged extracts a unitUpdate from a raw signal, reporting
// false for anything that is not a systemd Unit PropertiesChanged. It keeps the
// unit's object-path base escaped rather than unescaping it, so it does no
// allocation for the name (see unitUpdate).
func decodeUnitPropertiesChanged(sig *dbus.Signal) (unitUpdate, bool) {
	if sig == nil || sig.Name != propertiesChangedSignal || len(sig.Body) < 2 {
		return unitUpdate{}, false
	}
	if iface, _ := sig.Body[0].(string); iface != unitInterface {
		return unitUpdate{}, false
	}
	changed, ok := sig.Body[1].(map[string]dbus.Variant)
	if !ok {
		return unitUpdate{}, false
	}
	return unitUpdate{pathBase: path.Base(string(sig.Path)), changed: changed}, true
}
