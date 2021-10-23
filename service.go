package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/containerd/containerd/log"
	shimapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/ttrpc"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/coreos/go-systemd/v22/dbus"
)

const (
	// This is the socket location that we serve the containerd shim API on.
	defaultAddress = "/run/containerd/s/containerd-shim-systemd-v1.sock"
	serviceName    = "containerd-shim-systemd-v1"
)

func newService(ts shimapi.TaskService) (*service, error) {
	s, err := ttrpc.NewServer(ttrpc.WithServerHandshaker(ttrpc.UnixSocketRequireSameUser()))
	if err != nil {
		return nil, err
	}

	shimapi.RegisterTaskService(s, ts)

	return &service{
		srv: s,
	}, nil
}

type service struct {
	srv *ttrpc.Server
}

func (s *service) Serve(ctx context.Context, l net.Listener) error {
	daemon.SdNotify(false, daemon.SdNotifyReady)
	log.G(ctx).Info("Serving")
	return s.srv.Serve(ctx, l)
}

func (s *service) Close() error {
	return s.srv.Close()
}

func serviceUnit(exe string, root, addr, ttrpcAddr string, debug bool) string {
	return `
[Unit]
Description=containerd shim service that uses systemd to manage containers

[Service]
Type=notify
ExecStart=` + exe + ` serve --address=` + addr + ` --ttrpc-address=` + ttrpcAddr + ` --debug=` + strconv.FormatBool(debug) + ` --root=` + root + `
`
}

func socketUnit(addr string) string {
	return `
[Unit]
Description=containerd shim socket for ` + serviceName + `

[Install]
WantedBy = sockets.target

[Socket]
ListenStream=` + addr + `
SocketMode=0700
PassCredentials=yes
PassSecurity=yes
`
}

func install(ctx context.Context, root, addr, ttrpcAddr, socket string, debug bool) error {
	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	err = os.WriteFile("/etc/systemd/system/"+serviceName+".service", []byte(serviceUnit(exe, root, addr, ttrpcAddr, debug)), 0644)
	if err != nil {
		return err
	}

	err = os.WriteFile("/etc/systemd/system/"+serviceName+".socket", []byte(socketUnit(socket)), 0644)
	if err != nil {
		os.RemoveAll("/etc/systemd/system/" + serviceName + ".service")
		return err
	}

	if err := conn.ReloadContext(ctx); err != nil {
		return err
	}

	ch := make(chan string, 1)
	if _, err := conn.StartUnitContext(ctx, serviceName+".socket", "replace", ch); err != nil {
		return fmt.Errorf("error starting socket unit: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-ch:
		if status != "done" {
			return fmt.Errorf("error starting socket unit: %s", status)
		}
	}

	return nil
}

func uninstall(ctx context.Context) error {
	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.StopUnitContext(ctx, serviceName+".socket", "replace", nil); err != nil {
		return fmt.Errorf("error stopping socket unit: %w", err)
	}

	if _, err := conn.StopUnitContext(ctx, serviceName+".service", "replace", nil); err != nil {
		return fmt.Errorf("error stopping service unit: %w", err)
	}

	if _, err := conn.DisableUnitFilesContext(ctx, []string{serviceName + ".service", serviceName + ".socket"}, true); err != nil {
		return fmt.Errorf("error disabling units: %w", err)
	}

	if err := os.Remove("/etc/systemd/system/" + serviceName + ".socket"); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Error("failed to remove socket unit")
	}
	if err := os.Remove("/etc/systemd/system/" + serviceName + ".service"); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Error("failed to remove service unit")
	}

	if err := conn.ReloadContext(ctx); err != nil {
		return err
	}

	return nil
}
