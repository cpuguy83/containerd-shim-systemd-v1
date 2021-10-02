package systemdshim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/pkg/errors"
)

const (
	defaultAddress     = "/run/containerd-systemd-shim/shim.sock"
	defaultServiceName = "containerd-systemd-shim"
)

type StartOpts struct {
	Address      string
	TTRPCAddress string
	Debug        bool
	Namespace    string
}

func socketAddress(address, namespace string) (string, string) {
	sockRoot := filepath.Join(defaults.DefaultStateDir, "s")
	d := sha256.Sum256([]byte(filepath.Join(address, namespace, grouping)))
	hex := fmt.Sprintf("%x", d)
	return hex, "unix://" + filepath.Join(sockRoot, hex)
}

// StartShim is a binary call that executes a new shim returning the address
func (s *Service) StartShim(ctx context.Context, opts StartOpts) (_ string, retErr error) {
	if opts.Address == "" {
		return "", errors.New("address must be provided")
	}
	base, socket := socketAddress(opts.Address, opts.Namespace)

	if shim.CanConnect(socket) {
		return socket, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", errors.Wrap(err, "error getting current executable path")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", errors.Wrap(err, "error getting current working directory")
	}

	flags := []string{
		"-address=" + opts.Address,
		"-socket=" + socket,
		"--namespace=" + opts.Namespace,
		"--root=" + filepath.Dir(cwd),
	}
	if opts.Debug {
		flags = append(flags, "-debug")
	}

	cmd := append([]string{exe}, flags...)
	cmd = append(cmd, "serve")

	properties := []dbus.Property{
		dbus.PropType("notify"),
		dbus.PropExecStart(cmd, false),
		{Name: "Environment", Value: godbus.MakeVariant([]string{"TTRPC_ADDRESS=" + opts.TTRPCAddress})},
	}
	unit := "containerd-systemd-shim-" + base + ".service"

	log.G(ctx).WithField("unit-name", unit).Debugf("Starting shim with args: %s", cmd)

	wait := make(chan string, 1)

	_, err = s.conn.StartTransientUnitContext(ctx, unit, "replace", properties, wait)
	if err != nil {
		if err2 := s.conn.ResetFailedUnitContext(ctx, unit); err2 != nil {
			return "", err
		}
		_, err = s.conn.StartTransientUnitContext(ctx, unit, "replace", properties, wait)
		if err != nil {
			return "", errors.Wrap(err, "error starting systemd shim unit")
		}
	}

	log.G(ctx).Debug("Waiting for shim to be ready")

	select {
	case <-ctx.Done():
		return "", errors.Wrap(ctx.Err(), "context cancelled while waiting for shim to be ready")
	case status := <-wait:
		if status != "done" && status != "skipped" {
			return "", errors.Errorf("failed to start shim: %s", status)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("error waiting for shim to be ready to connect to: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
			if shim.CanConnect(socket) {
				return socket, nil
			}
		}
	}
}
