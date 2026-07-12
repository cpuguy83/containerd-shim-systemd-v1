package main

import (
	"context"
	"encoding/hex"
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

// unitUpdate is a decoded systemd Unit PropertiesChanged event: the unit name
// and the properties that changed.
type unitUpdate struct {
	Name    string
	Changed map[string]dbus.Variant
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
// false for anything that is not a systemd Unit PropertiesChanged.
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
	return unitUpdate{Name: unitNameFromPath(sig.Path), Changed: changed}, true
}

// unitNameFromPath decodes a systemd unit object path (e.g.
// /org/freedesktop/systemd1/unit/foo_2eservice) back into the unit name
// (foo.service).
func unitNameFromPath(op dbus.ObjectPath) string {
	return pathBusUnescape(path.Base(string(op)))
}

// pathBusUnescape reverses the escaping systemd applies to build a unit's D-Bus
// object path, turning _xx hex escapes back into their bytes. It mirrors
// systemd's bus_label_unescape: a lone "_" is the empty string, and an "_" that
// is not followed by two hex digits is taken literally.
func pathBusUnescape(p string) string {
	if p == "_" {
		return ""
	}
	n := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '_' && i+2 < len(p) {
			if b, err := hex.DecodeString(p[i+1 : i+3]); err == nil {
				n = append(n, b...)
				i += 2
				continue
			}
		}
		n = append(n, p[i])
	}
	return string(n)
}
